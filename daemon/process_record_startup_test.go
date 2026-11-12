package daemon

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// beforeThisTestProcess is a file time earlier than this test process's
// start, which recovery requires of an empty process record it removes
// (processowner.clearEmptyRecord). It is fixed when the package initializes:
// the package runs for longer than an hour, so "an hour before now" is not
// before the process started in a test that runs late.
var beforeThisTestProcess = time.Now().Add(-time.Hour)

// FAE-CP-003: an empty process record a pre-wave-9 binary's crash left in the
// plane registry no longer blocks every start: it names no process, so the
// start removes it and the instance comes up. A record the start cannot read
// is refused with its file named, and kept.
func TestStartClearsAnEmptyProcessRecordAndNamesAnUnreadableOne(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	layout, err := Paths(cfg.Root, cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(layout.Run, "plane-processes")
	plant := func(name, body string) string {
		t.Helper()
		path := filepath.Join(registry, name)
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, beforeThisTestProcess, beforeThisTestProcess); err != nil {
			t.Fatal(err)
		}
		return path
	}
	empty := plant("process-empty.json", "")
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal("an empty process record refused the start:", err)
	}
	stop(t, d)
	if _, err = os.Lstat(empty); !os.IsNotExist(err) {
		t.Fatal("empty process record kept", err)
	}
	garbled := plant("process-garbled.json", `{"PID":`)
	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("an unreadable process record was accepted")
	}
	if !strings.Contains(err.Error(), "recover owned Dolt") || !strings.Contains(err.Error(), garbled) {
		t.Fatalf("the refusal does not name the record: %v", err)
	}
	if _, statErr := os.Lstat(garbled); statErr != nil {
		t.Fatal("refused record removed", statErr)
	}
}

// FAE-ROB-014: the start's removal of an empty process record leaves a trace.
// Before, the file was deleted with no audit row and no daemon line. Now each
// removal writes a supervision log line naming the file at once, before Dolt
// starts for the plane's own registry, and an audit row with its path once
// the plane is up; the UI registry, recovered with the plane up, is handled
// the same way.
func TestStartLogsAndAuditsEachEmptyProcessRecordItRemoves(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	layout, err := Paths(cfg.Root, cfg.Instance)
	if err != nil {
		t.Fatal(err)
	}
	earlier := beforeThisTestProcess
	var planted []string
	for _, registry := range []string{filepath.Join(layout.Run, "plane-processes"), filepath.Join(layout.Run, "ui-processes")} {
		if err = os.MkdirAll(registry, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(registry, "process-empty.json")
		if err = os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err = os.Chtimes(path, earlier, earlier); err != nil {
			t.Fatal(err)
		}
		planted = append(planted, path)
	}
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("an empty process record refused the start:", err)
	}
	defer stop(t, d)
	log, err := os.ReadFile(layout.Log)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range planted {
		if _, err = os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("empty process record kept", path, err)
		}
		if !strings.Contains(string(log), "removed empty process record "+path) {
			t.Errorf("no daemon log line names the removed record %s", path)
		}
		var n int
		if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "count removal audits", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='daemon' AND action=? AND inputs LIKE ?`, "empty-process-record-removed", "%"+path+"%").Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("%d audit rows for the removed record %s, want 1", n, path)
		}
	}
}
