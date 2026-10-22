package execution

import (
	"context"
	"errors"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"testing"
)

type stalledRuntime struct{ retired bool }

func (s stalledRuntime) Run(context.Context, runtime.Launch) (runtime.Collected, error) {
	return runtime.Collected{ProcessesRetired: s.retired}, &runtime.TurnError{Class: "stalled", Cause: errors.New("stall")}
}
func TestStallReplayRequiresContractRetirementAndTree(t *testing.T) {
	for _, tt := range []struct {
		name                      string
		trusted, retired, changed bool
	}{{"safe", true, true, false}, {"untrusted", false, true, false}, {"alive", true, false, false}, {"changed", true, true, true}} {
		t.Run(tt.name, func(t *testing.T) {
			x, d, _ := fixture(t)
			x.Runtime = stalledRuntime{tt.retired}
			x.StallSafeReplay = tt.trusted
			calls := 0
			x.Tree = func(context.Context, Binding) (string, error) {
				calls++
				if tt.changed && calls > 1 {
					return "changed", nil
				}
				return "tree", nil
			}
			r, e := x.Execute(context.Background(), d)
			if e == nil {
				t.Fatal("missing stall")
			}
			want := "uncertain"
			if tt.name == "safe" {
				want = "stall-retry"
			}
			if r.Class != want {
				t.Fatal(r.Class, want)
			}
		})
	}
}

func TestStallHealthFailureCannotAuthorizeReplay(t *testing.T) {
	x, d, _ := fixture(t)
	x.Runtime = stalledRuntime{true}
	x.StallSafeReplay = true
	x.Observe = func(context.Context, engine.Dispatch, engine.Session, providerhealth.Outcome) error {
		return errors.New("health commit unavailable")
	}
	r, e := x.Execute(context.Background(), d)
	if e == nil || r.Class != "uncertain" {
		t.Fatalf("%+v %v", r, e)
	}
}

func TestStallRecordsTimeoutHealth(t *testing.T) {
	x, d, _ := fixture(t)
	x.Runtime = stalledRuntime{true}
	x.StallSafeReplay = true
	count := 0
	x.Observe = func(_ context.Context, _ engine.Dispatch, _ engine.Session, o providerhealth.Outcome) error {
		count++
		if o != providerhealth.Timeout {
			t.Fatalf("stall outcome %s", o)
		}
		return nil
	}
	r, _ := x.Execute(context.Background(), d)
	if count != 1 || r.Class != "stall-retry" {
		t.Fatal(count, r.Class)
	}
}
