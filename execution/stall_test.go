package execution

import (
	"context"
	"errors"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
	"strings"
	"testing"
)

type stalledRuntime struct{ retired bool }

func (s stalledRuntime) Run(context.Context, runtime.Launch) (runtime.Collected, error) {
	return runtime.Collected{ProcessesRetired: s.retired}, &runtime.TurnError{Class: "stalled", Cause: errors.New("stall")}
}

// FAE-RT-005: a positively detected stall is retried by default when the
// turn's effective profile proves restricted effects (handoff, read, edit under
// the trusted bridge), its processes are retired and its tree is unchanged.
func TestStallReplayRequiresContractRetirementAndTree(t *testing.T) {
	for _, tt := range []struct {
		name                      string
		trusted, retired, changed bool
		capability                string
		want                      string
	}{{"safe", true, true, false, "", "stall-retry"}, {"default", false, true, false, "edit", "stall-retry"}, {"unrestricted", false, true, false, "shell", "uncertain"}, {"alive", false, false, false, "", "uncertain"}, {"changed", false, true, true, "", "uncertain"}, {"trusted-alive", true, false, false, "", "uncertain"}} {
		t.Run(tt.name, func(t *testing.T) {
			x, d, _ := fixture(t)
			x.Runtime = stalledRuntime{tt.retired}
			x.StallSafeReplay = tt.trusted
			if tt.capability != "" {
				resolve := x.Resolve
				x.Resolve = func(ctx context.Context, d engine.Dispatch) (Binding, error) {
					b, e := resolve(ctx, d)
					role := b.Document.Roles["coder"]
					role.Profile.Allowed = []string{"read", tt.capability}
					b.Document.Roles["coder"] = role
					return b, e
				}
			}
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
			if r.Class != tt.want {
				t.Fatal(r.Class, tt.want)
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

func TestRestrictedEffects(t *testing.T) {
	for allowed, want := range map[string]bool{"": true, "handoff": true, "read,edit,handoff": true, "read,shell": false, "network": false} {
		var caps []string
		if allowed != "" {
			caps = strings.Split(allowed, ",")
		}
		if RestrictedEffects(caps) != want {
			t.Fatal(allowed, want)
		}
	}
}
