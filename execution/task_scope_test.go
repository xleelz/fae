package execution

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// Every agent turn, review turns included, carries the task's resolved scope
// as the task_scope block right after the stage block: files sorted,
// deduplicated and never null, and the RED test file once there is one.
func TestTaskScopeBlockOnEveryAgentTurn(t *testing.T) {
	x, d, a := fixture(t)
	rec := &launches{inner: x.Runtime}
	x.Runtime = rec
	resolve := x.Resolve
	scope := TaskScope{Files: []string{"pkg/b.go", "pkg/a_test.go", "pkg/b.go"}, TestFile: "pkg/a_test.go"}
	x.Resolve = func(ctx context.Context, d engine.Dispatch) (Binding, error) {
		b, err := resolve(ctx, d)
		b.Scope = scope
		return b, err
	}
	block := func(i int) string {
		var blocks []json.RawMessage
		if err := json.Unmarshal(rec.seen[i].Brief, &blocks); err != nil || len(blocks) < 3 {
			t.Fatal(err, string(rec.seen[i].Brief))
		}
		var stage struct{ Kind string }
		if json.Unmarshal(blocks[1], &stage) != nil || stage.Kind != "stage" {
			t.Fatal("task_scope does not follow the stage block", string(rec.seen[i].Brief))
		}
		return string(blocks[2])
	}
	if _, err := x.Execute(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if got, want := block(0), `{"files":["pkg/a_test.go","pkg/b.go"],"kind":"task_scope","test_file":"pkg/a_test.go"}`; got != want {
		t.Fatalf("agent turn task_scope %s, want %s", got, want)
	}
	if len(scope.Files) != 3 || scope.Files[0] != "pkg/b.go" {
		t.Fatal("block normalisation mutated the binding's scope", scope.Files)
	}
	review := d
	review.ID = "review"
	review.Config.Primitive = workflow.Review
	review.Sessions = []engine.Session{{ID: "r1", Role: "reviewer"}, {ID: "r2", Role: "reviewer"}}
	scope = TaskScope{}
	if _, err := x.Execute(context.Background(), review); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 2; i++ {
		if got, want := block(i), `{"files":[],"kind":"task_scope","test_file":""}`; got != want {
			t.Fatalf("review turn %d task_scope %s, want %s", i, got, want)
		}
	}
	// A scope that cannot hold the turn is a typed refusal before launch,
	// which charges nothing (FAE-RT-004).
	x.Resolve = func(context.Context, engine.Dispatch) (Binding, error) {
		return Binding{}, &ScopeRefusal{Detail: "unsafe path"}
	}
	launched := len(rec.seen)
	r, err := x.Execute(context.Background(), d)
	var refusal *ScopeRefusal
	if !errors.As(err, &refusal) || r.Class != engine.ClassRefusedBeforeLaunch || r.Detail != "task scope refused: unsafe path" || len(rec.seen) != launched || a.acquired != 3 {
		t.Fatal("scope refusal", r, err, len(rec.seen), a.acquired)
	}
}
