package execution

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/runtime"
)

// FAE-RT-004: a one-shot herdr-core turn whose pane ledger bind (BeforeStart)
// fails is infrastructure-deferred: no provider observation and no startup
// retry, whose exhaustion would park ProviderUnavailable and charge the binding.
func TestOneShotPaneBindFailureDefersWithoutCharge(t *testing.T) {
	x, d, a := fixture(t)
	direct := x.Runtime.(*runtime.Direct)
	// A short root: under a long $TMPDIR the herdr-core socket path bound would
	// refuse the launch before BeforeStart runs.
	root, e := os.MkdirTemp("/var/tmp", "fae-pbd-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	direct.Root = root
	x.Runtime = &runtime.Multiplexer{Direct: direct, Binary: "/nonexistent/herdr-core", Instance: "fae-test", BeforeStart: func(context.Context, string, string) error {
		return errors.New("pane ledger bind failed")
	}}
	observed := 0
	x.Observe = func(context.Context, engine.Dispatch, engine.Session, providerhealth.Outcome) error {
		observed++
		return nil
	}
	r, e := x.Execute(context.Background(), d)
	if e == nil || r.Class != "infrastructure-deferred" || observed != 0 || a.released != 1 {
		t.Fatalf("class %q err %v observed %d released %d", r.Class, e, observed, a.released)
	}
}
