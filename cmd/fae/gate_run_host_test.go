package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/daemon"
)

// FAE-OBS-001: every line gate run writes while it waits starts with an
// RFC 3339 timestamp with its zone, the instance and the component, as the
// daemon's supervision log lines do. Before, the lines were "fae: ..." with
// neither.
func TestGateRunLinesCarryTimeInstanceAndComponent(t *testing.T) {
	ledger, alerts := gateTestHost(t, 12) // I/O pressure holds the gate
	home := t.TempDir()
	var out bytes.Buffer
	e := execute(context.Background(), []string{"gate", "run", "--home", home, "--instance", "gates", "--host-ledger", ledger, "--max-wait", "1m", "--", "true"}, &out)
	if exitCode(e) != daemon.GateRunTimeoutExit {
		t.Fatal("I/O pressure did not time the wait out", e)
	}
	lines := strings.Split(strings.TrimSpace(alerts.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("want the held reading and the timeout: %q", lines)
	}
	for _, l := range lines {
		fields := strings.Fields(l)
		if len(fields) < 4 {
			t.Fatalf("line %q", l)
		}
		at, err := time.Parse(time.RFC3339Nano, fields[0])
		if err != nil || fields[1] != "instance=gates" || fields[2] != "component=gate-run" || fields[3] != "gate" || time.Since(at) > time.Hour {
			t.Errorf("line %q lacks its time, instance or component: %v", l, err)
		}
	}
}

// FAE-HOST-020: gate run removes its scratch even when the command left a
// read-only tree in it (Go's module cache under a test's own GOPATH).
// Before, os.RemoveAll's error was ignored and the tree stayed under the
// instance root.
func TestGateRunRemovesAReadOnlyScratch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	ledger, _ := gateTestHost(t, 1)
	home := t.TempDir()
	runs := filepath.Join(home, "gates", "gate-runs")
	t.Cleanup(func() {
		_ = filepath.WalkDir(runs, func(p string, d fs.DirEntry, _ error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0700)
			}
			return nil
		})
	})
	var out bytes.Buffer
	script := `m="$TMPDIR/gopath/pkg/mod/example.com/m@v1.0.0" && mkdir -p "$m/sub" && echo module m > "$m/go.mod" && chmod -R a-w "$TMPDIR/gopath" && echo "tmp=$TMPDIR"`
	if e := execute(context.Background(), []string{"gate", "run", "--home", home, "--instance", "gates", "--host-ledger", ledger, "--", "sh", "-c", script}, &out); e != nil {
		t.Fatal(e, out.String())
	}
	tmp := strings.TrimSpace(strings.TrimPrefix(out.String(), "tmp="))
	if !strings.HasPrefix(tmp, runs+"/") {
		t.Fatal("gate run scratch not under the instance root", tmp)
	}
	if _, e := os.Lstat(tmp); !os.IsNotExist(e) {
		t.Fatal("gate run left a scratch holding a read-only tree", e)
	}
	// Its owner record, which lets a start sweep it after a SIGKILL, goes too.
	if left, _ := os.ReadDir(runs); len(left) != 0 {
		t.Fatal("gate run left entries in gate-runs", left)
	}
}
