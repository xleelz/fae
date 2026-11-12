// Package dag lays out the backlog's task graph in layers by dependency
// depth, deterministically, for the TUI's graph view.
package dag

import (
	"slices"
	"sort"
)

// Edge is a directed edge From → To. Only edges with Layered set (blocking
// edges) decide depth; the others (relates edges) are drawn but never move a
// node.
type Edge struct {
	From, To string
	Layered  bool
}

// Layout places every node in a layer and a row.
type Layout struct {
	// Layers holds the node ids of each layer, in row order. Layer 0 holds
	// the nodes no layered edge enters.
	Layers [][]string
	// Layer and Row give each node's position.
	Layer map[string]int
	Row   map[string]int
	// Cyclic lists nodes on or behind a layered cycle. The daemon refuses
	// cycles, so a well-formed graph has none; a malformed one still lays
	// out, with these nodes in one last layer.
	Cyclic []string
}

// Build lays out nodes and edges. Endpoints missing from nodes are added
// after them. The order of nodes breaks every tie, so the same input always
// yields the same layout. It runs in O((V+E) log V) and terminates on any
// input, cycles and self-edges included.
func Build(nodes []string, edges []Edge) Layout {
	rank := map[string]int{}
	order := []string{}
	add := func(id string) {
		if _, seen := rank[id]; !seen {
			rank[id] = len(order)
			order = append(order, id)
		}
	}
	for _, n := range nodes {
		add(n)
	}
	for _, e := range edges {
		add(e.From)
		add(e.To)
	}
	succ := map[string][]string{}
	pred := map[string][]string{}
	indegree := map[string]int{}
	seen := map[[2]string]bool{}
	for _, e := range edges {
		key := [2]string{e.From, e.To}
		if !e.Layered || e.From == e.To || seen[key] {
			continue
		}
		seen[key] = true
		succ[e.From] = append(succ[e.From], e.To)
		pred[e.To] = append(pred[e.To], e.From)
		indegree[e.To]++
	}
	byRank := func(ids []string) {
		sort.SliceStable(ids, func(i, j int) bool { return rank[ids[i]] < rank[ids[j]] })
	}
	// Longest-path layering by Kahn's algorithm: a node's layer is one past
	// its deepest prerequisite's.
	l := Layout{Layer: map[string]int{}, Row: map[string]int{}}
	ready := []string{}
	for _, id := range order {
		l.Layer[id] = 0
		if indegree[id] == 0 {
			ready = append(ready, id)
		}
	}
	placed := 0
	deepest := 0
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		placed++
		deepest = max(deepest, l.Layer[id])
		next := succ[id]
		byRank(next)
		for _, s := range next {
			l.Layer[s] = max(l.Layer[s], l.Layer[id]+1)
			if indegree[s]--; indegree[s] == 0 {
				ready = append(ready, s)
			}
		}
	}
	if placed < len(order) {
		for _, id := range order {
			if indegree[id] > 0 {
				l.Cyclic = append(l.Cyclic, id)
			}
		}
		last := deepest + 1
		for _, id := range l.Cyclic {
			l.Layer[id] = last
		}
	}
	layers := 0
	for _, id := range order {
		layers = max(layers, l.Layer[id]+1)
	}
	l.Layers = make([][]string, layers)
	for _, id := range order {
		l.Layers[l.Layer[id]] = append(l.Layers[l.Layer[id]], id)
	}
	// Order each layer by the mean row of its prerequisites in the layers
	// above (the barycentre heuristic), which keeps edges short; ties and
	// nodes without placed prerequisites keep the input order.
	for i, layer := range l.Layers {
		if i > 0 {
			centre := map[string]float64{}
			for _, id := range layer {
				sum, n := 0.0, 0
				for _, p := range pred[id] {
					if l.Layer[p] < i {
						sum += float64(l.Row[p])
						n++
					}
				}
				if n > 0 {
					centre[id] = sum / float64(n)
				} else {
					centre[id] = float64(len(order) + rank[id])
				}
			}
			slices.SortStableFunc(layer, func(a, b string) int {
				switch {
				case centre[a] < centre[b]:
					return -1
				case centre[a] > centre[b]:
					return 1
				}
				return rank[a] - rank[b]
			})
		}
		for row, id := range layer {
			l.Row[id] = row
		}
	}
	return l
}
