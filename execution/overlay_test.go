package execution

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

type launches struct {
	inner interface {
		Run(context.Context, runtime.Launch) (runtime.Collected, error)
	}
	seen []runtime.Launch
}

func (l *launches) Run(ctx context.Context, launch runtime.Launch) (runtime.Collected, error) {
	l.seen = append(l.seen, launch)
	return l.inner.Run(ctx, launch)
}

// FAE-WF-044: a class overlay launches the role at the overlay's effort with
// the overlay's template; other classes keep the declared role.
func TestTaskClassOverlayEffortAndBrief(t *testing.T) {
	x, d, _ := fixture(t)
	rec := &launches{inner: x.Runtime}
	x.Runtime = rec
	resolve := x.Resolve
	x.Resolve = func(ctx context.Context, d engine.Dispatch) (Binding, error) {
		b, err := resolve(ctx, d)
		role := b.Document.Roles["coder"]
		role.Briefs = map[string]string{"default": "declared template"}
		b.Document.Roles["coder"] = role
		b.Document.TaskClasses = map[string]workflow.TaskClass{"docs": {Overlays: map[string]workflow.Overlay{"coder": {Effort: "low", Briefs: map[string]string{"default": "docs template"}}}}}
		return b, err
	}
	d.TaskClass = "docs"
	d.Sessions[0].Route = workflow.Binding{Provider: "provider", Model: "model", Effort: "low"}
	if _, err := x.Execute(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	d.TaskClass = ""
	d.Sessions[0].ID = "second"
	if _, err := x.Execute(context.Background(), d); err == nil {
		t.Fatal("overlay effort accepted outside its class")
	}
	d.Sessions[0].Route = workflow.Binding{}
	d.Sessions[0].ID = "third"
	if _, err := x.Execute(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	template := func(l runtime.Launch) any {
		var blocks []map[string]any
		_ = json.Unmarshal(l.Brief, &blocks)
		return blocks[1]["template"]
	}
	if len(rec.seen) != 2 || rec.seen[0].Binding.Effort != "low" || template(rec.seen[0]) != "docs template" || rec.seen[1].Binding.Effort != "" || template(rec.seen[1]) != "declared template" {
		t.Fatal(rec.seen)
	}
}

// FAE-WF-078: the approved widened file set travels in every later brief.
func TestApprovedScopeInBrief(t *testing.T) {
	x, d, _ := fixture(t)
	rec := &launches{inner: x.Runtime}
	x.Runtime = rec
	d.Scope = []string{"pkg/extra.go"}
	if _, err := x.Execute(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var blocks []struct {
		Kind  string   `json:"kind"`
		Scope []string `json:"approved_scope"`
	}
	if err := json.Unmarshal(rec.seen[0].Brief, &blocks); err != nil || blocks[1].Kind != "stage" || len(blocks[1].Scope) != 1 || blocks[1].Scope[0] != "pkg/extra.go" {
		t.Fatal(blocks, err)
	}
}
