package dag

import (
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"
)

func TestBuildLayersByDependencyDepth(t *testing.T) {
	// a → b → d, a → c → d, e alone; c ~ e relates (no depth).
	l := Build([]string{"e", "d", "c", "b", "a"}, []Edge{
		{From: "a", To: "b", Layered: true}, {From: "b", To: "d", Layered: true},
		{From: "a", To: "c", Layered: true}, {From: "c", To: "d", Layered: true},
		{From: "c", To: "e"},
	})
	want := map[string]int{"a": 0, "e": 0, "b": 1, "c": 1, "d": 2}
	if !reflect.DeepEqual(l.Layer, want) {
		t.Fatalf("layers %v, want %v", l.Layer, want)
	}
	if len(l.Layers) != 3 || len(l.Cyclic) != 0 {
		t.Fatalf("layout %+v", l)
	}
	// Input order breaks ties in the first layer; the barycentre keeps c
	// and b in input order under their shared prerequisite.
	if !reflect.DeepEqual(l.Layers[0], []string{"e", "a"}) || !reflect.DeepEqual(l.Layers[1], []string{"c", "b"}) {
		t.Fatalf("rows %v", l.Layers)
	}
	for id, layer := range l.Layer {
		if l.Layers[layer][l.Row[id]] != id {
			t.Fatalf("%s: row index disagrees with layers", id)
		}
	}
}

func TestBuildIsDeterministicWhateverTheEdgeOrder(t *testing.T) {
	nodes, edges := randomDAG(rand.New(rand.NewSource(7)), 300, 900)
	first := Build(nodes, edges)
	r := rand.New(rand.NewSource(11))
	for range 5 {
		shuffled := append([]Edge(nil), edges...)
		r.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		if again := Build(nodes, shuffled); !reflect.DeepEqual(first, again) {
			t.Fatal("the same graph laid out differently")
		}
	}
}

func TestBuildPlacesEveryBlockingEdgeDownward(t *testing.T) {
	nodes, edges := randomDAG(rand.New(rand.NewSource(3)), 400, 1600)
	start := time.Now()
	l := Build(nodes, edges)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("a 400-task graph took %v", took)
	}
	for _, e := range edges {
		if e.Layered && l.Layer[e.From] >= l.Layer[e.To] {
			t.Fatalf("edge %s → %s does not descend: %d, %d", e.From, e.To, l.Layer[e.From], l.Layer[e.To])
		}
	}
	// Longest path: every node below layer 0 has a prerequisite one layer up.
	for _, id := range nodes {
		if l.Layer[id] == 0 {
			continue
		}
		found := false
		for _, e := range edges {
			found = found || (e.Layered && e.To == id && l.Layer[e.From] == l.Layer[id]-1)
		}
		if !found {
			t.Fatalf("%s at layer %d has no prerequisite in the layer above", id, l.Layer[id])
		}
	}
	total := 0
	for _, layer := range l.Layers {
		total += len(layer)
	}
	if total != len(nodes) {
		t.Fatalf("%d nodes placed, want %d", total, len(nodes))
	}
}

func TestBuildTerminatesOnAMalformedGraph(t *testing.T) {
	done := make(chan Layout, 1)
	go func() {
		done <- Build([]string{"a", "b", "c", "d"}, []Edge{
			{From: "a", To: "b", Layered: true}, {From: "b", To: "a", Layered: true}, // cycle
			{From: "b", To: "c", Layered: true}, // behind the cycle
			{From: "d", To: "d", Layered: true}, // self-edge, ignored
			{From: "d", To: "d", Layered: true}, // duplicate
			{From: "c", To: "ghost", Layered: true},
		})
	}()
	select {
	case l := <-done:
		if !reflect.DeepEqual(l.Cyclic, []string{"a", "b", "c", "ghost"}) {
			t.Fatalf("cyclic %v", l.Cyclic)
		}
		if l.Layer["d"] != 0 || l.Layer["a"] != 1 || l.Layer["ghost"] != 1 || len(l.Layers) != 2 {
			t.Fatalf("layout %+v", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("layout of a cyclic graph did not terminate")
	}
}

func TestBuildEmptyGraph(t *testing.T) {
	if l := Build(nil, nil); len(l.Layers) != 0 || len(l.Cyclic) != 0 {
		t.Fatalf("%+v", l)
	}
}

// randomDAG draws edges only from a lower to a higher index, so the graph is
// acyclic; a third of them are relates edges.
func randomDAG(r *rand.Rand, n, m int) ([]string, []Edge) {
	nodes := make([]string, n)
	for i := range nodes {
		nodes[i] = fmt.Sprintf("t%03d", i)
	}
	edges := []Edge{}
	for range m {
		a, b := r.Intn(n), r.Intn(n)
		if a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		edges = append(edges, Edge{From: nodes[a], To: nodes[b], Layered: r.Intn(3) != 0})
	}
	return nodes, edges
}
