package execution

import (
	"context"
	"github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/scheduler"
	"testing"
)

type residentRefusal struct{ proof, retired bool }

func (r residentRefusal) Run(context.Context, runtime.Launch) (runtime.Collected, error) {
	return runtime.Collected{NonExecution: r.proof, ProcessesRetired: r.retired}, &scheduler.AdmissionRefusal{Reason: "pane limit"}
}
func TestResidentPaneRefusalRequiresNonexecutionAndRetirement(t *testing.T) {
	for _, tc := range []struct{ proof, retired, changed bool }{{true, true, false}, {false, true, false}, {true, false, false}, {true, true, true}} {
		x, d, a := fixture(t)
		x.Runtime = residentRefusal{tc.proof, tc.retired}
		calls := 0
		x.Tree = func(context.Context, Binding) (string, error) {
			calls++
			if tc.changed && calls > 1 {
				return "changed", nil
			}
			return "tree", nil
		}
		r, e := x.Execute(context.Background(), d)
		want := "uncertain"
		if tc.proof && tc.retired && !tc.changed {
			want = "infrastructure-deferred"
		}
		if e == nil || r.Class != want || a.released != 1 {
			t.Fatalf("%+v: %+v %v", tc, r, e)
		}
	}
}
