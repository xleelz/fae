//go:build linux

package processowner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func defunctChildren(t *testing.T) []Process {
	t.Helper()
	kids, err := directChildren()
	if err != nil {
		t.Fatal(err)
	}
	var out []Process
	for _, k := range kids {
		if k.Defunct() {
			out = append(out, k)
		}
	}
	return out
}

func awaitNoDefunct(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(defunctChildren(t)) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("defunct children remain: %+v", defunctChildren(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// FAE-HOST-023: adopted orphans (a foreign group's member, a session leader
// and one in this process's own group) are reaped promptly while own forks,
// including one that made itself a session leader, keep their exit statuses
// for exec.Cmd.
func TestReaperReapsAdoptedOrphansWithoutStealingStatuses(t *testing.T) {
	if err := StartReaper(300 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	reaper.Lock()
	reaper.grace = 300 * time.Millisecond
	before := reaper.reaped
	reaper.Unlock()
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			argv := []string{"/bin/sh", "-c", "exit 7"}
			want := 7
			if i%4 == 0 {
				argv, want = []string{"setsid", "/bin/sh", "-c", "sleep 0.4; exit 4"}, 4
			}
			err := exec.Command(argv[0], argv[1:]...).Run()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != want {
				errs <- errors.New(strings.Join(argv, " ") + ": " + strings.TrimSpace(err.Error()))
			}
		}(i)
	}
	// Orphans in a foreign group, in a new session and in this process's own
	// group (indistinguishable from an own fork, so reaped after the grace).
	for i, script := range []string{"sleep 0.2 & exit 0", "setsid sleep 0.2 & exit 0", "sleep 0.2 & exit 0"} {
		cmd := exec.Command("/bin/sh", "-c", script)
		if i < 2 {
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error("own fork status stolen:", err)
	}
	awaitNoDefunct(t)
	reaper.Lock()
	reaped := reaper.reaped - before
	reaper.Unlock()
	if reaped < 3 {
		t.Fatalf("adopted orphans reaped %d, want 3", reaped)
	}
}

// FAE-HOST-021 acceptance: after shutdown no inventory process remains, an
// escaped (setsid) descendant is killed as an adopted child, no zombie is left
// and a process the instance does not own is never signalled.
func TestShutdownLeavesNoInventoryProcessAndNoZombie(t *testing.T) {
	if err := StartReaper(ReaperGrace); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dir := filepath.Join(root, "processes")
	escapee := filepath.Join(root, "escapee")
	foreign := exec.Command("sleep", "60")
	if err := foreign.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = foreign.Process.Kill(); _ = foreign.Wait() }()
	owner := "instance-owner"
	var cmds []*exec.Cmd
	var records []*Record
	for role, script := range map[string]string{"agent-session": "setsid sleep 60 & echo $! > " + escapee + "; sleep 60", "host-command": "sleep 60"} {
		cmd := exec.Command("/bin/sh", "-c", script)
		r, err := StartRole(cmd, dir, owner, "session-"+role, role)
		if err != nil {
			t.Fatal(err)
		}
		cmds, records = append(cmds, cmd), append(records, r)
	}
	inventory, err := Inventory(dir)
	if err != nil || len(inventory) != 2 {
		t.Fatal(inventory, err)
	}
	roles := map[string]bool{}
	for _, r := range inventory {
		roles[r.Role] = r.PID > 1 && r.Birth != "" && r.ParentPID == os.Getpid() && r.ParentBirth != ""
	}
	if !roles["agent-session"] || !roles["host-command"] {
		t.Fatalf("inventory lacks identities or roles: %+v", inventory)
	}
	var escaped int
	for deadline := time.Now().Add(5 * time.Second); escaped == 0; time.Sleep(10 * time.Millisecond) {
		raw, _ := os.ReadFile(escapee)
		escaped, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
		if time.Now().After(deadline) {
			t.Fatal("escapee not started")
		}
	}
	// Shutdown: retire every owned group, then the adopted children.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i, r := range records {
		if err = Terminate(ctx, r, owner); err != nil {
			t.Fatal(err)
		}
		_ = cmds[i].Wait()
		disown(r.PID)
	}
	if killed, err := KillAdopted(ctx); err != nil || killed < 1 {
		t.Fatal(killed, err)
	}
	if inventory, err = Inventory(dir); err != nil || len(inventory) != 0 {
		t.Fatal("inventory process remains", inventory, err)
	}
	for _, r := range records {
		if !ProvedDead(r.PID, r.Birth) {
			t.Fatalf("%s %d survived shutdown", r.Role, r.PID)
		}
	}
	if syscall.Kill(escaped, 0) != syscall.ESRCH {
		t.Fatalf("escaped descendant %d survived shutdown", escaped)
	}
	// A child the reaper or the Go runtime is still collecting can show as
	// defunct for an instant; one never reaped fails after the bound.
	awaitNoDefunct(t)
	if err = foreign.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("a process the instance does not own was signalled:", err)
	}
}
