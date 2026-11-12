package defaults

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-080, Appendix C step 20: the shipped default carries the Normal
// code-review sub-workflow; Local tasks pin exactly Workflow().
func TestShippedNormalSubWorkflow(t *testing.T) {
	d := Shipped()
	if issues := workflow.Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	local, err := workflow.LocalProjection(d)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(local)
	b, _ := json.Marshal(Workflow())
	if string(a) != string(b) {
		t.Fatal("Local projection of the shipped default differs from Workflow()")
	}
	core := Workflow()
	subjects := []string{}
	for id, s := range d.Stages {
		if _, ok := core.Stages[id]; ok {
			continue
		}
		if !s.NormalOnly {
			t.Errorf("stage %s added outside Normal mode", id)
		}
		if s.Primitive == workflow.ExternalEffect {
			if !workflow.IsTargetPinned(s.Pins) || s.Role != "planner" || s.IdempotenceKey != id || s.Output != "ExternalReceipt" {
				t.Errorf("stage %s is not a daemon-pinned planner effect: %+v", id, s)
			}
			subjects = append(subjects, s.Subject)
		}
	}
	sort.Strings(subjects)
	want := append(NormalSubjects(), TicketUpdate)
	sort.Strings(want)
	if strings.Join(subjects, ",") != strings.Join(want, ",") {
		t.Fatal("subjects", subjects, want)
	}
	if d.Stages["review-create"].Failure != "review-revision" || d.Stages["review-read"].Failure != "review-remediation-loop" {
		t.Fatal("same-review revision or remediation route missing")
	}
	loop := d.Stages["review-remediation-loop"]
	if loop.Primitive != workflow.Loop || loop.MaxIterations != 5 {
		t.Fatal("remediation loop is not bounded at the Appendix A cap of 5", loop)
	}
	edges := map[workflow.Edge]bool{}
	for _, e := range d.Edges {
		edges[e] = true
	}
	if !edges[workflow.Edge{From: "review-remediation-loop", To: "implement"}] || !edges[workflow.Edge{From: "integration-gate", To: "ticket-open"}] || edges[workflow.Edge{From: "integration-gate", To: "Done"}] || !edges[workflow.Edge{From: "ticket-closed", To: "Done"}] {
		t.Fatal("sub-workflow is not between integration and Done, or remediation misses the implementer")
	}
	// Normal mode runs the sub-workflow after integration; Local reaches Done.
	script := simulationScript(t, "task", HappyScript())
	for _, id := range NormalStages {
		script[id] = []workflow.ScriptStep{{Evidence: []workflow.Evidence{{ID: id, TaskID: "task", Type: "ExternalReceipt", Version: 1, Stage: id, Provenance: true, Fields: map[string]any{"tool": d.Stages[id].Subject, "argument_digest": "d", "external_id": "x", "reconciliation_state": "dry-run", "idempotency_key": id}}}}}
	}
	normal, err := engine.Simulate(d, "task", script, workflow.SimulationOptions{Normal: true})
	if err != nil {
		t.Fatal(err)
	}
	tail := "integrate,integration-gate," + strings.Join(NormalStages, ",") + ",Done"
	if normal.State != domain.Done || !strings.HasSuffix(strings.Join(normal.Path, ","), tail) {
		t.Fatal("Normal path", normal.Path, normal.Reason)
	}
	for _, id := range NormalStages {
		delete(script, id)
	}
	localRun, err := engine.Simulate(d, "task", script, workflow.SimulationOptions{})
	if err != nil || localRun.State != domain.Done || !strings.HasSuffix(strings.Join(localRun.Path, ","), "integrate,integration-gate,Done") {
		t.Fatal("Local path", localRun.Path, err)
	}
}

// shippedGenerations pins the content hash of every shipped generation of the
// default workflow (FAE-WF-080); generation 1 predates pins. An instance may
// have installed any of them, and MayInstallShippedDefault upgrades only to a
// higher generation, so an entry here is never edited: re-recording one in
// place strands every instance that installed it. When Shipped() changes, bump
// defaults.Generation instead and add the new generation here and to
// testdata/shipped-generation.json.
var shippedGenerations = map[int]string{
	4: "a881b789c939c0e9be2221027b5fcc5d5cc72d1c723bc8154d81d5ad9caac6af",
	2: "f22412627001e85307c5db3c005215da8a21a4e4757b2f1e45d698c00c057e85",
	3: "2e663e9270518d7598a2b0d6b9b6ab4b7b93adff60f81ae4b61c48a41d0f4ddc",
}

// Generation must move with the shipped content, or instances holding an
// older builtin revision are never upgraded (FAE-WF-080). The testdata pin is
// the history of every generation's content hash, oldest first, and must agree
// with shippedGenerations entry for entry, so neither can be re-recorded alone.
func TestShippedGenerationPinsContent(t *testing.T) {
	raw, err := os.ReadFile("testdata/shipped-generation.json")
	if err != nil {
		t.Fatal(err)
	}
	var pinned struct {
		Generations []struct {
			Generation int    `json:"generation"`
			Hash       string `json:"hash"`
			Content    string `json:"content,omitempty"`
		} `json:"generations"`
	}
	if err = json.Unmarshal(raw, &pinned); err != nil || len(pinned.Generations) == 0 {
		t.Fatal("generation history", err)
	}
	hash, err := workflow.Hash(Shipped())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for i, g := range pinned.Generations {
		if g.Generation != i+2 {
			t.Fatalf("history entry %d records generation %d; generations are consecutive from 2", i, g.Generation)
		}
		if other, ok := seen[g.Hash]; ok {
			t.Fatalf("generation %d repeats the content of generation %d", g.Generation, other)
		}
		seen[g.Hash] = g.Generation
		if g.Content == "" {
			continue
		}
		content, err := os.ReadFile(filepath.Join("testdata", g.Content))
		if err != nil {
			t.Fatal(err)
		}
		if h, err := domain.ContentHash(content); err != nil || h != g.Hash {
			t.Fatalf("generation %d content %s hashes to %s", g.Generation, g.Content, h)
		}
	}
	// Generation 2 is the wave-2 default some instances installed; its content
	// is kept for the upgrade test.
	if pinned.Generations[0].Content == "" {
		t.Fatal("generation 2 lost its content")
	}
	if len(pinned.Generations) != len(shippedGenerations) {
		t.Fatalf("the testdata history records %d generations; shippedGenerations pins %d", len(pinned.Generations), len(shippedGenerations))
	}
	for _, g := range pinned.Generations {
		if g.Hash != shippedGenerations[g.Generation] {
			t.Fatalf("generation %d was re-recorded: testdata hash %s, pinned %s; bump Generation instead", g.Generation, g.Hash, shippedGenerations[g.Generation])
		}
	}
	if pin, ok := shippedGenerations[Generation]; !ok || pin != hash || Generation != len(shippedGenerations)+1 {
		t.Fatalf("shipped default changed: Generation %d, content hash %s, pinned %q; bump Generation and pin the new hash", Generation, hash, pin)
	}
}
