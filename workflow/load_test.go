package workflow_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-093 (operator decision, 2026-10-30): a stored revision falls in one
// of two classes. Unreadable: Load fails (another schema, an unknown or
// mistyped field, not JSON), and the daemon refuses to start. Unrunnable:
// Load reads it but Parse, the rule admission and the engine rely on,
// refuses it; the daemon refuses to start while an active task is pinned to
// it, or quarantines those tasks under the serve flag, and lists it
// otherwise. The old default's `main` brief keys are unrunnable, and so is
// every structural gap the engine would dereference (a Gate without an
// expression, a start stage the document lacks, an edge to a missing stage,
// an unknown primitive, a Loop without iterations, zero limits); Load itself
// refuses none of them, so a document the binary can read is never called
// unreadable.
func TestLoadAcceptsWhatOnlyLaterRulesRefuse(t *testing.T) {
	old := defaults.Workflow()
	for name, role := range old.Roles {
		briefs := map[string]string{}
		for _, text := range role.Briefs {
			briefs["main"] = text
		}
		role.Briefs = briefs
		old.Roles[name] = role
	}
	raw, _ := json.Marshal(old)
	if _, err := workflow.Parse(raw); err == nil || !strings.Contains(err.Error(), "brief key must be default") {
		t.Fatal("the old default passes current validation", err)
	}
	if d, err := workflow.Load(raw); err != nil || d.ID != old.ID {
		t.Fatal("the old default does not load", err)
	}
	mutate := func(f func(*workflow.Document)) []byte {
		d := defaults.Shipped()
		f(&d)
		raw, _ := json.Marshal(d)
		return raw
	}
	unrunnable := map[string][]byte{
		"start: nowhere (minimal)": []byte(`{"schema_version":1,"id":"x","start":"nowhere"}`),
		"start: nowhere":           mutate(func(d *workflow.Document) { d.Start = "nowhere" }),
		"edge to a missing stage": mutate(func(d *workflow.Document) {
			d.Edges = append(d.Edges, workflow.Edge{From: d.Start, To: "ghost"})
		}),
		"unknown primitive": mutate(func(d *workflow.Document) {
			s := d.Stages[d.Start]
			s.Primitive = "Bogus"
			d.Stages[d.Start] = s
		}),
		"gate without an expression": mutate(func(d *workflow.Document) {
			for k, s := range d.Stages {
				if s.Primitive == workflow.Gate {
					s.Expr = nil
					d.Stages[k] = s
				}
			}
		}),
		"loop without iterations": mutate(func(d *workflow.Document) {
			for k, s := range d.Stages {
				if s.Primitive == workflow.Loop {
					s.MaxIterations = 0
					d.Stages[k] = s
				}
			}
		}),
		"zero limits": mutate(func(d *workflow.Document) { d.Limits.Attempts, d.Limits.TestRuns = 0, 0 }),
		"no roles":    mutate(func(d *workflow.Document) { d.Roles = map[string]workflow.Role{} }),
	}
	for name, raw := range unrunnable {
		if _, err := workflow.Load(raw); err != nil {
			t.Errorf("%s: a structural or semantic issue refused the load (unreadable), want unrunnable: %v", name, err)
		}
		if _, err := workflow.Parse(raw); err == nil {
			t.Errorf("%s: Parse accepts it, so the engine would run it", name)
		}
	}
	for _, c := range []struct{ raw, want string }{
		{`{"schema_version":3,"id":"x","start":"s"}`, "schema_version: must be 1 or 2"},
		{`{"schema_version":1,"id":"x","from_a_later_binary":true}`, "from_a_later_binary: unknown field"},
		{`{"schema_version":"1","id":"x"}`, "schema_version"},
		{`not json`, "invalid"},
	} {
		if _, err := workflow.Load([]byte(c.raw)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: loaded or unnamed: %v", c.raw, err)
		}
	}
}
