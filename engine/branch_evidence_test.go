package engine_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// FAE-WF-073: a live implementation changes the tree, yet the default's
// green-route still reads the spec's pre-code no-red declaration; any other
// stale-tree evidence stays invisible to Branch arms.
func TestBranchReadsNoRedDeclarationAcrossTrees(t *testing.T) {
	for _, noRed := range []bool{true, false} {
		script := defaults.HappyScript()
		var spec map[string]any
		_ = json.Unmarshal(script["spec"][0].Output, &spec)
		spec["no_red"] = noRed
		script["spec"] = []engine.ScriptStep{{Output: raw(spec)}}
		script["run-suite"] = script["qa"]
		j, err := engine.NewLive("task", 1, defaults.Workflow(), "tree-0")
		if err != nil {
			t.Fatal(err)
		}
		executor := engine.NewScripted(script)
		for i := 0; i < 200 && j.State != domain.Escalated && !j.State.Terminal() && string(j.State) != "format"; i++ {
			next, d, _, err := engine.Prepare(j)
			if err != nil {
				t.Fatal(err)
			}
			if j = next; d == nil {
				continue
			}
			r, err := executor.Execute(context.Background(), *d)
			if err != nil {
				t.Fatal(err)
			}
			r.ExecutionKind = "live"
			if d.Config.ChangeProducer {
				r.NewTree = "tree-" + d.Stage
			}
			if j, _, err = engine.Complete(j, d.ID, r); err != nil {
				t.Fatal(err)
			}
		}
		want := "run-tests"
		if noRed {
			want = "run-suite"
		}
		if string(j.State) != "format" || !transitioned(j, "green-route", want) {
			t.Fatal(noRed, j.State, j.Reason)
		}
	}
}

func transitioned(j engine.Job, from, to string) bool {
	for _, e := range j.History {
		if e.Kind == "transition" && string(e.From) == from && string(e.To) == to {
			return true
		}
	}
	return false
}
