package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// FAE-HOST-020: GateRunScratch publishes the owner record before the scratch
// exists and its remove removes both; the sweep keeps a dead gate run's
// scratch while a process still runs with it as TMPDIR (a command the killed
// gate run left running) and removes it once that process is gone; a record
// left without its scratch is removed once its owner is dead.
func TestGateRunScratchOwnerRecordAndHolder(t *testing.T) {
	root := t.TempDir()
	log := &hostScratchLog{}
	d := &Daemon{log: log, layout: Layout{Root: root}}
	dir, remove, err := GateRunScratch(root, "gate-run-own")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(dir + ".owner")
	var owner gateRunOwner
	if err != nil || json.Unmarshal(raw, &owner) != nil || owner.PID != os.Getpid() || owner.Birth != mustBirth(t, os.Getpid()) {
		t.Fatalf("owner record %s %v", raw, err)
	}
	d.sweepGateRunScratch()
	if _, err = os.Stat(dir); err != nil {
		t.Fatal("a live gate run's scratch was swept", err)
	}
	if err = remove(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, dir + ".owner"} {
		if _, err = os.Lstat(p); !os.IsNotExist(err) {
			t.Fatal("left after remove:", p, err)
		}
	}

	held := plantGateRunScratch(t, root, "gate-run-held", deadGateRunOwner(t))
	holder := exec.Command("sleep", "30")
	holder.Env = append(os.Environ(), "TMPDIR="+held)
	if err = holder.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })
	d.sweepGateRunScratch()
	if _, err = os.Stat(held); err != nil {
		t.Fatal("a scratch still used as a TMPDIR was swept", err)
	}
	if !strings.Contains(log.String(), "process "+strconv.Itoa(holder.Process.Pid)+" still runs with it as its temporary directory") {
		t.Fatalf("no line names the holder:\n%s", log.String())
	}
	_ = holder.Process.Kill()
	_ = holder.Wait()
	if err = os.WriteFile(filepath.Join(root, "gate-runs", "gate-run-gone.owner"), deadGateRunOwner(t), 0600); err != nil {
		t.Fatal(err)
	}
	d.sweepGateRunScratch()
	for _, p := range []string{held, held + ".owner", filepath.Join(root, "gate-runs", "gate-run-gone.owner")} {
		if _, err = os.Lstat(p); !os.IsNotExist(err) {
			t.Fatal("not swept:", p, err)
		}
	}
}
