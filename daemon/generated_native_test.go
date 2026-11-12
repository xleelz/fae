package daemon

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-VER-002, FAE-VER-003: the generated-graph corpus's properties hold on the
// daemon path too. Generated AgentTurn chains with a closing gate (the shape of
// TestGeneratedDurableWorkflowParity10000, with workflow-unique stage ids) are
// installed through WorkflowInstall and driven by the daemon's scripted executor:
// each reaches its generated outcome, dispatches every stage exactly once,
// audits every transition and never passes a gate without evidence.
func TestDaemonGeneratedWorkflows(t *testing.T) {
	cfg, ctx := fixture(t)
	rng := rand.New(rand.NewSource(60163))
	schema := json.RawMessage(`{"type":"object","required":["ok"],"additionalProperties":false,"properties":{"ok":{"type":"boolean"}}}`)
	script := map[string][]engine.ScriptStep{}
	type generated struct {
		doc  workflow.Document
		size int
		want domain.State
	}
	var cases []generated
	for n := 0; n < 8; n++ {
		size, pass := 1+rng.Intn(3), n%3 != 2
		d := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: fmt.Sprintf("generated-%d", n), Start: fmt.Sprintf("g%ds0", n), Limits: workflow.Limits{Attempts: 6, TestRuns: 2, ScopeWidens: 1}, Roles: map[string]workflow.Role{"worker": {Instructions: "Fixture", Briefs: map[string]string{"default": "fixture"}, Routes: []workflow.Binding{{Provider: "scripted", Model: "fixture"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Report"}}}, Artifacts: map[string]workflow.ArtifactType{"Report": {Version: 1, Schema: schema}}, Stages: map[string]workflow.Stage{}})
		gate, stop := fmt.Sprintf("g%dgate", n), fmt.Sprintf("g%dstop", n)
		for i := 0; i < size; i++ {
			id := fmt.Sprintf("g%ds%d", n, i)
			s := workflow.Stage{Primitive: workflow.AgentTurn, Role: "worker", Output: "Report"}
			if i > 0 {
				s.Inputs = []workflow.Input{{Name: "prior", Type: "Report", Stage: fmt.Sprintf("g%ds%d", n, i-1), Version: 1}}
			}
			d.Stages[id] = s
			next := fmt.Sprintf("g%ds%d", n, i+1)
			if i == size-1 {
				next = gate
			}
			d.Edges = append(d.Edges, workflow.Edge{From: id, To: next})
			script[id] = []engine.ScriptStep{{Output: json.RawMessage(fmt.Sprintf(`{"ok":%t}`, pass))}}
		}
		x := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: fmt.Sprintf("g%ds%d", n, size-1), Path: []string{"ok"}}, {Op: "literal", Value: true}}}
		d.Stages[gate] = workflow.Stage{Primitive: workflow.Gate, Expr: &x, Failure: stop}
		d.Stages[stop] = workflow.Stage{Primitive: workflow.Escalate}
		d.Edges = append(d.Edges, workflow.Edge{From: gate, To: "Done"}, workflow.Edge{From: gate, To: stop})
		d = currentWorkflowFixture(d)
		if issues := workflow.Validate(d); len(issues) != 0 {
			t.Fatal(n, issues)
		}
		want := domain.Done
		if !pass {
			want = domain.Escalated
		}
		cases = append(cases, generated{d, size, want})
	}
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	for _, c := range cases {
		if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: c.doc.ID, Document: c.doc, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err = call(t, d, "Admit", AdmitParams{ID: c.doc.ID, Tree: "tree", WorkflowID: c.doc.ID, AuthorizingMessage: "message"}, c.doc.ID); err != nil {
			t.Fatal(c.doc.ID, err)
		}
	}
	for _, c := range cases {
		awaitState(t, d, c.doc.ID, string(c.want))
		e := liveEnvelope(t, d, c.doc.ID)
		if e.Job.Sequence != c.size || len(e.Job.Provenance) != c.size {
			t.Fatalf("%s: %d dispatches and %d artifacts for %d stages", c.doc.ID, e.Job.Sequence, len(e.Job.Provenance), c.size)
		}
		for _, h := range e.Job.History {
			if h.Kind == "transition" && (h.Attempt < 1 || h.Cause == "") {
				t.Fatalf("%s: transition without audit context %+v", c.doc.ID, h)
			}
			if h.Gate != nil && h.Gate.Passed && len(h.Gate.EvidenceIDs) == 0 {
				t.Fatalf("%s: vacuous gate %+v", c.doc.ID, h)
			}
		}
	}
}

// FAE-WF-070, FAE-VER-002: content that weakens a kernel floor is refused at
// the daemon's install path, naming the floor's rule, and installs nothing.
func TestDaemonRefusesFloorWeakenedWorkflows(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	weakenings := map[string]struct {
		message string
		mutate  func(*workflow.Document)
	}{
		"bypass": {"without independent review and passing test gate", func(w *workflow.Document) {
			w.Edges = append(w.Edges, workflow.Edge{From: "format-gate", To: "commit"})
		}},
		"loop-cap":    {"finite positive cap required", func(w *workflow.Document) { s := w.Stages["red-loop"]; s.MaxIterations = 0; w.Stages["red-loop"] = s }},
		"self-review": {"reviewer must differ from every change producer", func(w *workflow.Document) { s := w.Stages["reviews"]; s.Role = "coder"; w.Stages["reviews"] = s }},
		"vacuous-test-gate": {"executed_count >= 1", func(w *workflow.Document) {
			s, x := w.Stages["qa-gate"], workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "qa", Path: []string{"exit_code"}}, {Op: "literal", Value: 0}}}
			s.Expr = &x
			w.Stages["qa-gate"] = s
		}},
		"attempt-limit": {"must be positive", func(w *workflow.Document) { w.Limits.Attempts = 0 }},
		"kernel-edge":   {"kernel edges cannot be overridden", func(w *workflow.Document) { w.Edges = append(w.Edges, workflow.Edge{From: "Done", To: "intake"}) }},
	}
	for name, w := range weakenings {
		doc := defaults.Workflow()
		doc.ID = "weak-" + name
		w.mutate(&doc)
		_, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, "")
		if err == nil || !strings.Contains(err.Error(), w.message) {
			t.Fatalf("%s: floor-weakened content not refused by its rule: %v", name, err)
		}
		if _, err = call(t, d, "Admit", AdmitParams{ID: "admit-" + name, Tree: "tree", WorkflowID: doc.ID, AuthorizingMessage: "message"}, "admit-"+name); err == nil {
			t.Fatalf("%s: refused content was installed", name)
		}
	}
}
