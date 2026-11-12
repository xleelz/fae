package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/processowner"
)

// deadGateRunOwner is the owner record of a process that ran and exited.
func deadGateRunOwner(t *testing.T) []byte {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	birth, err := processowner.Birth(cmd.Process.Pid)
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"pid": cmd.Process.Pid, "birth": birth})
	return raw
}

// plantGateRunScratch makes gate-runs/<id> holding a read-only tree, with an
// owner record when owner is not nil.
func plantGateRunScratch(t *testing.T, root, id string, owner []byte) string {
	t.Helper()
	dir := filepath.Join(root, "gate-runs", id)
	if err := os.MkdirAll(filepath.Join(dir, "mod", "cache"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mod", "cache", "f"), []byte("x"), 0400); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"mod/cache", "mod"} {
		if err := os.Chmod(filepath.Join(dir, sub), 0555); err != nil {
			t.Fatal(err)
		}
	}
	if owner != nil {
		if err := os.WriteFile(dir+".owner", owner, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(dir, "mod"), 0700)
		_ = os.Chmod(filepath.Join(dir, "mod", "cache"), 0700)
	})
	return dir
}

// FAE-HOST-020 at a real start (Dolt): the start removes the scratch a
// SIGKILLed `fae gate run` left under gate-runs, read-only trees and all,
// once its owner record proves the gate run dead. Before, nothing swept
// gate-runs. A scratch whose gate run is alive, or that has no owner record
// (an older binary's), stays; the one without a record is logged.
func TestStartSweepsTheScratchOfAKilledGateRun(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	instance := filepath.Join(cfg.Root, cfg.Instance)
	alive, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "birth": mustBirth(t, os.Getpid())})
	dead := plantGateRunScratch(t, instance, "gate-run-killed", deadGateRunOwner(t))
	running := plantGateRunScratch(t, instance, "gate-run-running", alive)
	unowned := plantGateRunScratch(t, instance, "gate-run-unowned", nil)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	if _, err = os.Lstat(dead); !os.IsNotExist(err) {
		t.Fatal("a killed gate run's scratch was not swept:", err)
	}
	if _, err = os.Lstat(dead + ".owner"); !os.IsNotExist(err) {
		t.Fatal("a swept scratch's owner record stayed:", err)
	}
	for _, kept := range []string{running, running + ".owner", unowned} {
		if _, err = os.Lstat(kept); err != nil {
			t.Fatal("swept", kept, err)
		}
	}
	log, _ := os.ReadFile(filepath.Join(instance, "supervision.log"))
	if !strings.Contains(string(log), "gate run scratch "+unowned+" left: it has no owner record") || !strings.Contains(string(log), "removed 1 gate run scratch directories left by killed gate runs") {
		t.Fatalf("sweep lines missing:\n%s", log)
	}
}

func mustBirth(t *testing.T, pid int) string {
	t.Helper()
	birth, err := processowner.Birth(pid)
	if err != nil {
		t.Fatal(err)
	}
	return birth
}

// FAE-HOST-020 at a real start (Dolt): what scratch-quarantine holds is
// reported in status (startup_checks.scratch_quarantine: entries and bytes)
// and raised as an audited warning alert, so it cannot grow unseen. Before,
// a quarantined tree was only logged. A later start that finds the same
// trees raises no second alert.
func TestStartReportsAndAlertsWhatScratchQuarantineHolds(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := lookup(statusBody(t, d, nil), "startup_checks.scratch_quarantine.entries"); entries != float64(0) {
		t.Fatal("an empty quarantine reported", entries)
	}
	stop(t, d)
	instance := filepath.Join(cfg.Root, cfg.Instance)
	left := filepath.Join(instance, "worktrees", "command-tmp-9", "x")
	if err = os.MkdirAll(left, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(left, "data"), make([]byte, 1000), 0600); err != nil {
		t.Fatal(err)
	}
	failHostScratchRemoval(t)
	quarantineAlerts := func() []map[string]any {
		var out []map[string]any
		for _, a := range rpcResult[AlertsPage](t, d, "Alerts", AlertsParams{}).Alerts {
			if a.Source == "scratch-quarantine" {
				var detail map[string]any
				_ = json.Unmarshal(a.Detail, &detail)
				detail["severity"] = a.Severity
				out = append(out, detail)
			}
		}
		return out
	}
	for start := 0; start < 2; start++ {
		if d, err = Start(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		body := statusBody(t, d, nil)
		entries, _ := lookup(body, "startup_checks.scratch_quarantine.entries")
		size, _ := lookup(body, "startup_checks.scratch_quarantine.bytes")
		if entries != float64(1) || size != float64(1000) {
			t.Fatalf("start %d: scratch_quarantine %v", start, body["startup_checks"])
		}
		alerts := quarantineAlerts()
		if len(alerts) != 1 || alerts[0]["severity"] != "warning" || alerts[0]["entries"] != float64(1) || alerts[0]["path"] != filepath.Join(instance, "scratch-quarantine") {
			t.Fatalf("start %d: scratch-quarantine alerts %v", start, alerts)
		}
		stop(t, d)
	}
}
