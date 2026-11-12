package daemon

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostScratchLog is a supervision log fixture: lines pass through as written.
type hostScratchLog struct{ bytes.Buffer }

func (*hostScratchLog) Close() error { return nil }

// failHostScratchRemoval makes every start-sweep removal fail until the test
// ends, as a tree the sweep cannot remove even writable would.
func failHostScratchRemoval(t *testing.T) {
	t.Helper()
	saved := removeHostScratch
	t.Cleanup(func() { removeHostScratch = saved })
	removeHostScratch = func(string) error { return errors.New("planted removal failure") }
}

// FAE-HOST-020: a scratch tree the start sweep cannot remove is moved into
// scratch-quarantine under the instance root, out of the worktrees directory
// and the next sweep's way, and logged; one it can neither remove nor move
// stays, logged. Neither fails the sweep, so a leftover scratch tree never
// stops a daemon starting.
func TestStartSweepQuarantinesScratchItCannotRemove(t *testing.T) {
	root := t.TempDir()
	log := &hostScratchLog{}
	d := &Daemon{log: log, layout: Layout{Root: root, Run: filepath.Join(root, "run"), Worktrees: filepath.Join(root, "worktrees")}}
	failHostScratchRemoval(t)
	for _, dir := range []string{"task-a", "command-tmp-2/x", "command-policy-3/x"} {
		if err := os.MkdirAll(filepath.Join(d.layout.Worktrees, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.sweepCommandScratch(nil); err != nil {
		t.Fatal("an unremovable scratch tree failed the start sweep:", err)
	}
	if entries, _ := os.ReadDir(d.layout.Worktrees); len(entries) != 1 || entries[0].Name() != "task-a" {
		t.Fatal("an unremovable scratch tree was not moved out of the worktrees", entries)
	}
	for _, dir := range []string{"command-tmp-2", "command-policy-3"} {
		moved, _ := filepath.Glob(filepath.Join(root, "scratch-quarantine", dir+"-*", dir, "x"))
		if len(moved) != 1 || !strings.Contains(log.String(), "scratch "+filepath.Join(d.layout.Worktrees, dir)+" not removed (planted removal failure); quarantined as "+filepath.Dir(moved[0])) {
			t.Fatalf("%s not quarantined and logged: %v\n%s", dir, moved, log.String())
		}
	}
	if info, err := os.Stat(filepath.Join(root, "scratch-quarantine")); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("quarantine not private", info, err)
	}

	// A tree that can be neither removed nor moved stays, logged.
	if err := os.WriteFile(filepath.Join(root, "blocked"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	d.layout.Root = filepath.Join(root, "blocked") // no quarantine can be made under a file
	if err := os.Mkdir(filepath.Join(d.layout.Worktrees, "command-tmp-4"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.sweepCommandScratch(nil); err != nil {
		t.Fatal("an unmovable scratch tree failed the start sweep:", err)
	}
	if _, err := os.Stat(filepath.Join(d.layout.Worktrees, "command-tmp-4")); err != nil || !strings.Contains(log.String(), "scratch "+filepath.Join(d.layout.Worktrees, "command-tmp-4")+" left in place: not removed (planted removal failure) and not quarantined") {
		t.Fatalf("an unmovable tree not left and logged: %v\n%s", err, log.String())
	}
}

// FAE-HOST-020, FAE-OBS-001 at a real start (Dolt): a daemon restarted over
// a scratch tree it cannot remove starts, quarantines the tree and says so
// in a timestamped scratch line of its supervision log.
func TestLiveStartQuarantinesScratchItCannotRemove(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	instance := filepath.Join(cfg.Root, cfg.Instance)
	left := filepath.Join(instance, "worktrees", "command-tmp-9")
	if err = os.MkdirAll(filepath.Join(left, "x"), 0700); err != nil {
		t.Fatal(err)
	}
	failHostScratchRemoval(t)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("an unremovable scratch tree stopped the daemon starting:", err)
	}
	stop(t, d)
	if moved, _ := filepath.Glob(filepath.Join(instance, "scratch-quarantine", "command-tmp-9-*", "command-tmp-9", "x")); len(moved) != 1 {
		t.Fatal("unremovable scratch not quarantined", moved)
	}
	raw, err := os.ReadFile(filepath.Join(instance, "supervision.log"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, " component=scratch scratch "+left+" not removed (planted removal failure); quarantined as ") {
			if !strings.Contains(line, " instance="+cfg.Instance+" ") {
				t.Fatalf("quarantine line %q", line)
			}
			return
		}
	}
	t.Fatalf("no quarantine line in the supervision log:\n%s", raw)
}
