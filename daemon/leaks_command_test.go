package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/processowner"
)

// leakZombieScript leaves six defunct children under a process that never
// waits for them (the shell execs sleep).
const leakZombieScript = `for i in 1 2 3 4 5 6; do true & done; exec sleep 60`

// defunctChildrenOf counts the defunct children of pid.
func defunctChildrenOf(t *testing.T, pid int) int {
	t.Helper()
	all, err := processowner.Census()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, p := range all {
		if p.PPID == pid && p.Defunct() {
			n++
		}
	}
	return n
}

func awaitDefunctChildren(t *testing.T, pid, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for defunctChildrenOf(t, pid) < want {
		if time.Now().After(deadline) {
			t.Fatalf("process %d has %d defunct children, want %d", pid, defunctChildrenOf(t, pid), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// leakCensus runs one own-tree census and returns its report as JSON fields.
func leakCensus(t *testing.T, d *Daemon) map[string]any {
	t.Helper()
	_ = d.detectLeaks(context.Background(), false) // no plane: the alert itself is not recorded
	raw, _ := json.Marshal(d.processReport())
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func defunctLeak(report map[string]any) float64 {
	leaks, _ := report["leaks"].([]any)
	for _, l := range leaks {
		if leak, _ := l.(map[string]any); leak["reason"] == "defunct" {
			n, _ := leak["defunct"].(float64)
			return n
		}
	}
	return 0
}

// settled waits, when the test ends, until every process the test started
// under this one (the commands, their watcher) has exited and been
// collected, so a later test's own-tree baseline does not see them.
func settled(t *testing.T) {
	t.Helper()
	before := map[int]bool{}
	pids, err := processowner.DescendantPIDs(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, pid := range pids {
		before[pid] = true
	}
	t.Cleanup(func() {
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			pids, _ := processowner.DescendantPIDs(os.Getpid())
			if !slices.ContainsFunc(pids, func(pid int) bool { return !before[pid] }) {
				return
			}
		}
		t.Error("processes the test started outlived it")
	})
}

// FAE-HOST-022: defunct processes inside a running brokered command's tree
// are that command's to reap, so for processowner.CommandDefunctGrace
// censuses they raise no process-leak alert and are reported as
// defunct_in_commands. Before wave 12, every defunct process in the daemon's
// tree counted at once, so each self-hosted QA run (a test suite whose
// processes exit before their parents collect them) raised 2-4 process-leak
// error alerts; wave 12 exempted them for as long as the command ran, so a
// command holding unreaped children forever was never alerted. Now the same
// defunct processes under a process that is not a running brokered command
// raise the leak at once, and the running command's raise it once they have
// outlived the grace, naming the command that has not collected them.
func TestLeakCensusLeavesARunningCommandsDefunctProcessesToIt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the own-tree census reads /proc")
	}
	settled(t)
	run, worktrees := t.TempDir(), t.TempDir()
	d := &Daemon{layout: Layout{Run: run, Worktrees: worktrees}}
	baseline := leakCensus(t, d)
	unrelated, _ := baseline["defunct"].(float64)
	d.obs.limits = processowner.LeakLimits{Processes: 1000, RSS: 64 << 30, Defunct: int(unrelated) + 3}

	command := exec.Command("/bin/sh", "-c", leakZombieScript)
	r, err := processowner.Start(command, filepath.Join(run, "effect-processes"), worktrees)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-r.PID, syscall.SIGKILL)
		_ = processowner.Wait(command, r)
		_ = processowner.Retire(r)
	})
	awaitDefunctChildren(t, r.PID, 6)
	report := leakCensus(t, d)
	if n := defunctLeak(report); n != 0 {
		t.Fatalf("a running command's defunct processes raised a defunct leak of %v: %v", n, report)
	}
	if n, _ := report["defunct_in_commands"].(float64); int(n) != d.obs.limits.Defunct {
		t.Fatalf("defunct_in_commands %v, want %d (capped at limits.Defunct): %v", n, d.obs.limits.Defunct, report)
	}

	stray := exec.Command("/bin/sh", "-c", leakZombieScript)
	stray.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = stray.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-stray.Process.Pid, syscall.SIGKILL); _ = stray.Wait() })
	awaitDefunctChildren(t, stray.Process.Pid, 6)
	if n := defunctLeak(leakCensus(t, d)); n < 6 {
		t.Fatalf("defunct processes outside any running command raised a defunct leak of %v, want at least 6", n)
	}
	// The running command's children were first seen defunct two censuses
	// ago: the grace is over, and they count too, with the command named.
	report = leakCensus(t, d)
	if n := defunctLeak(report); n < 12 {
		t.Fatalf("a running command's defunct processes past the grace raised a defunct leak of %v, want at least 12: %v", n, report)
	}
	if n, _ := report["defunct_in_commands"].(float64); n != 0 {
		t.Fatalf("defunct_in_commands %v after the grace: %v", n, report)
	}
	named := false
	parents, _ := report["defunct_parents"].([]any)
	for _, p := range parents {
		parent, _ := p.(map[string]any)
		if int(parent["pid"].(float64)) == r.PID {
			named = parent["command"] == "sleep" && parent["defunct"].(float64) == 6 && parent["running"] == true && parent["rounds"].(float64) == 2
		}
	}
	if !named {
		t.Fatalf("the leak does not name the command holding its children: %v", parents)
	}
}
