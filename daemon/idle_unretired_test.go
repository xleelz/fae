package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/processowner"
	agentruntime "github.com/xleelz/fae/runtime"
)

// FAE-OBS-006: when an idle-maintenance refusal changes cause, the previous
// cause's refusals left unrecorded are recorded with the new cause's row,
// event and line, never dropped.
func TestIdleRefusalCauseChangeRecordsPreviousCount(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	d.obs.mu.Lock()
	d.obs.idleRefusal = idleRefusal{}
	d.obs.mu.Unlock()
	logged, _ := os.ReadFile(d.layout.Log)
	for i := 0; i < 3; i++ {
		if err = d.idleRefused(ctx, "census", "pane census unreadable"); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.idleRefused(ctx, "round", "registered idle socket missing from live census"); err != nil {
		t.Fatal(err)
	}
	events, err := d.store.Events(ctx, 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	for _, ev := range events {
		if ev.Kind == "idle-maintenance-refused" {
			var row map[string]any
			if err = json.Unmarshal(ev.Payload, &row); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, row)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("refusal rows %v, want one per cause", rows)
	}
	if rows[0]["cause"] != "census" || rows[0]["previous_cause"] != nil {
		t.Fatalf("first refusal %v", rows[0])
	}
	if rows[1]["cause"] != "round" || rows[1]["unrecorded_refusals"] != float64(0) || rows[1]["previous_cause"] != "census" || rows[1]["previous_unrecorded_refusals"] != float64(2) {
		t.Fatalf("the census refusals left unrecorded were dropped at the cause change: %v", rows[1])
	}
	raw, _ := os.ReadFile(d.layout.Log)
	if !strings.Contains(string(raw[len(logged):]), "idle maintenance refused cause=round reason=\"registered idle socket missing from live census\" unrecorded_refusals=0 previous_cause=census previous_unrecorded_refusals=2") {
		t.Fatalf("refusal line lacks the previous cause's count:\n%s", raw[len(logged):])
	}
}

// Each resident transport whose retirement is unproven keeps a pane beyond its
// role's cap until it is retired; status reports how many there are.
func TestStatusReportsUnretiredTransports(t *testing.T) {
	root, err := os.MkdirTemp("", "fae-unretired-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if root, err = filepath.EvalSymlinks(root); err != nil {
		t.Fatal(err)
	}
	pool, err := agentruntime.NewOwnedIdleSessions(filepath.Join(root, "idle-context"), root)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	record, err := processowner.StartLinked(cmd, filepath.Join(root, ".processes"), root, "resident-unproven")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = processowner.Terminate(ctx, record, root)
		_ = cmd.Wait()
		_ = processowner.Retire(record)
	})
	proven := false
	reap := func() error {
		if !proven {
			return errors.New("pane ledger release failed")
		}
		var exit *exec.ExitError
		if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
			return err
		}
		return nil
	}
	if err = pool.Register(agentruntime.IdleRegistration{ID: "resident-unproven", Socket: filepath.Join(root, "resident.sock"), Role: "planner", Global: true, Records: []*processowner.Record{record}, Context: json.RawMessage(`{}`), Reap: reap}, time.Now()); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{idleSessions: pool}
	unretired := func() any { return d.schedulerStatus()["unretired_transports"] }
	if n := unretired(); n != 0 {
		t.Fatal("unretired transports before any retirement", n)
	}
	if err = pool.Retire(context.Background(), "resident-unproven"); err == nil {
		t.Fatal("retirement proven despite a failed reap")
	}
	if n := unretired(); n != 1 {
		t.Fatal("unproven retirement not reported", n)
	}
	proven = true
	if err = pool.Retire(context.Background(), "resident-unproven"); err != nil {
		t.Fatal(err)
	}
	if n := unretired(); n != 0 || pool.Count() != 0 {
		t.Fatal("retired transport still reported", n, pool.Count())
	}
}
