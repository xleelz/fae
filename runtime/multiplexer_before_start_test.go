package runtime

import (
	"context"
	"errors"
	"os"
	"testing"
)

// FAE-RT-004: a one-shot pane whose BeforeStart (the daemon's pane ledger
// bind) fails starts neither Herdr nor the bridge. The turn is a typed
// admission deferral with positive non-execution and retired processes, so
// the executor defers it as infrastructure instead of charging a startup
// retry to the provider. No Herdr binary is needed: nothing reaches it.
func TestMultiplexerBeforeStartFailureDefers(t *testing.T) {
	d, l := directFixture(t, "")
	// A short root, as muxFixture uses: under a long $TMPDIR the socket
	// path bound would refuse the launch before BeforeStart runs.
	root, e := os.MkdirTemp("/var/tmp", "fae-mbs-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	d.Root = root
	cause := errors.New("pane ledger bind failed")
	m := &Multiplexer{Direct: d, Binary: "/nonexistent/herdr", Instance: "fae-test", BeforeStart: func(context.Context, string, string) error { return cause }}
	out, e := m.Run(context.Background(), l)
	var turn *TurnError
	var deferral interface{ AdmissionDeferred() bool }
	if !errors.As(e, &turn) || turn.Class != "startup-not-executed" || !errors.As(e, &deferral) || !deferral.AdmissionDeferred() || !errors.Is(e, cause) {
		t.Fatalf("want a typed admission deferral wrapping the bind failure, got %v", e)
	}
	if !out.NonExecution || !out.ProcessesRetired || len(m.Live()) != 0 {
		t.Fatalf("collected %+v live %v", out, m.Live())
	}
}
