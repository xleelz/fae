package commandshim

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/hostguard"
)

// FAE-OBS-001: every line heavy-op run writes starts with an RFC 3339
// timestamp with its zone, the instance and the component, as the daemon's
// supervision log lines do. A role session's shim run names the instance
// whose alert spool its policy names. Before, the lines were
// "fae heavy-op: ..." with neither.
func TestHeavyOpLinesCarryTimeInstanceAndComponent(t *testing.T) {
	spool := filepath.Join(t.TempDir(), "inst-a", "run", "host-alerts")
	if err := os.MkdirAll(spool, 0700); err != nil {
		t.Fatal(err)
	}
	loaded, _ := healthyHost(context.Background())
	loaded.LoadPerCore.Value = 4.5
	reads := 0
	observe := func(ctx context.Context) (hostguard.Sample, error) {
		if reads++; reads == 1 {
			return loaded, nil
		}
		return healthyHost(ctx)
	}
	var stderr bytes.Buffer
	clock := &heavyOpClock{now: time.Date(2026, 10, 29, 1, 2, 3, 0, time.UTC)}
	code, _ := RunHeavyOp(context.Background(), HeavyOp{Ledger: heavyLedger(t, 2), Weight: 1, Class: "role", Argv: []string{"/bin/true"}, Observe: observe, Clock: clock, Policy: hostguard.DefaultPolicy(), AlertSpool: spool, Stderr: &stderr})
	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if code != HeavyOpRefused || len(lines) != 2 {
		t.Fatalf("want a held reading and the timeout's, exit 75: exit %d %q", code, lines)
	}
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) < 4 {
			t.Fatalf("line %q", l)
		}
		at, err := time.Parse(time.RFC3339Nano, fields[0])
		if err != nil || fields[1] != "instance=inst-a" || fields[2] != "component=heavy-op" || fields[3] != "gate" || time.Since(at) > time.Hour {
			t.Errorf("line %q lacks its time, instance or component: %v", l, err)
		}
	}
}
