package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// FAE-VER-010 (pane loss) at the adapter: a Herdr pane that vanishes before
// reporting its exit status ends the turn at once, as a process failure naming
// the lost pane, with the pane and its private server retired; the turn does
// not wait out its bound. A pane that reports its status is unaffected
// (TestMultiplexerConformance).
func TestMultiplexerLostPaneEndsTurn(t *testing.T) {
	m, l := muxFixture(t, "wait")
	m.Direct.Timeout = 5 * time.Minute
	root := m.Direct.Root
	var out Collected
	done := make(chan error, 1)
	go func() {
		var e error
		out, e = m.Run(context.Background(), l)
		done <- e
	}()
	pane := 0
	for deadline := time.Now().Add(time.Minute); pane == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the bridge never ran in its pane")
		}
		for pid, p := range rootProcesses(root) {
			if strings.Contains(p, "TestBridgeHelper") {
				// The bridge runs in the pane shell's process group.
				pane, _ = syscall.Getpgid(pid)
			}
		}
	}
	if err := syscall.Kill(-pane, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case e := <-done:
		var turn *TurnError
		if !errors.As(e, &turn) || turn.Class != "process-failed" || !strings.Contains(e.Error(), "herdr pane lost") {
			t.Fatal("a lost pane did not end its turn as a process failure", e)
		}
		if !out.ProcessesRetired {
			t.Fatal("the lost pane's processes were not proved retired")
		}
	case <-time.After(time.Minute):
		t.Fatal("a lost pane waited out the turn bound")
	}
	if left := rootProcesses(root); len(left) != 0 {
		t.Fatal("the lost pane's server or processes survived", left)
	}
}

// rootProcesses lists live processes naming root in their command line or
// running inside it.
func rootProcesses(root string) map[int]string {
	out := map[int]string{}
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, e := strconv.Atoi(entry.Name())
		if e != nil || pid == os.Getpid() {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		if stat, e := os.ReadFile(filepath.Join(dir, "stat")); e != nil || strings.Contains(string(stat[strings.LastIndexByte(string(stat), ')')+1:]), " Z ") {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		cwd, _ := os.Readlink(filepath.Join(dir, "cwd"))
		if cmdline := strings.ReplaceAll(string(raw), "\x00", " "); strings.Contains(cmdline, root) || cwd == root || strings.HasPrefix(cwd, root+"/") {
			out[pid] = cmdline
		}
	}
	return out
}
