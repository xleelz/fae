package daemon

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// plantReadOnlyScratch leaves at dir what a test leaves in its TMPDIR when it
// points GOMODCACHE there: Go's read-only module cache (0555 directories,
// 0444 files), and a directory nobody may list. os.RemoveAll cannot remove it.
func plantReadOnlyScratch(t *testing.T, dir string) {
	t.Helper()
	module := filepath.Join(dir, "gopath", "pkg", "mod", "example.com", "m@v1.0.0")
	for _, d := range []string{filepath.Join(module, "sub"), filepath.Join(dir, "locked", "inner")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(module, "go.mod"), []byte("module m\n"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(filepath.Join(dir, "gopath"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			err = os.Chmod(p, 0555) // still listable, so the walk goes on
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "locked"), 0000); err != nil {
		t.Fatal(err)
	}
	// Leave nothing the fixtures' cleanup cannot remove, whatever the test does.
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, _ error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0700)
			}
			return nil
		})
		_ = os.RemoveAll(dir)
	})
}

// FAE-HOST-020: the start sweep removes a killed run's scratch, and a killed
// landing's gate TMPDIR, even when a test left a read-only tree there, and
// keeps the landing's own output. Before, the sweep returned os.RemoveAll's
// error, which failed Start.
func TestStartSweepRemovesReadOnlyScratch(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	d := &Daemon{layout: Layout{Root: root, Run: filepath.Join(root, "run"), Worktrees: filepath.Join(root, "worktrees")}}
	landing := filepath.Join(root, "landings", "land-1")
	for _, dir := range []string{filepath.Join(d.layout.Worktrees, "task-a"), landing} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(landing, "gate-output-1"), []byte("kept\n"), 0600); err != nil {
		t.Fatal(err)
	}
	plantReadOnlyScratch(t, filepath.Join(d.layout.Worktrees, "command-tmp-1"))
	plantReadOnlyScratch(t, filepath.Join(d.layout.Worktrees, "command-policy-2"))
	plantReadOnlyScratch(t, filepath.Join(landing, "tmp"))
	if err := d.sweepCommandScratch(nil); err != nil {
		t.Fatal("a read-only scratch tree failed the start sweep:", err)
	}
	entries, _ := os.ReadDir(d.layout.Worktrees)
	if len(entries) != 1 || entries[0].Name() != "task-a" {
		t.Fatal("read-only scratch not swept, or a worktree removed", entries)
	}
	if entries, _ = os.ReadDir(landing); len(entries) != 1 || entries[0].Name() != "gate-output-1" {
		t.Fatal("a landing's gate TMPDIR not swept, or its kept output removed", entries)
	}
}

// FAE-HOST-020 at a real start (Dolt): a daemon restarted over a killed
// run's scratch, and a killed landing's gate TMPDIR, holding read-only trees
// starts and removes them. Before, Start failed with the removal's error.
func TestLiveRestartOverReadOnlyScratchStarts(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	instance := filepath.Join(cfg.Root, cfg.Instance)
	left, landing := filepath.Join(instance, "worktrees", "command-tmp-9"), filepath.Join(instance, "landings", "land-1", "tmp")
	plantReadOnlyScratch(t, left)
	plantReadOnlyScratch(t, landing)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal("a read-only scratch tree stopped the daemon starting:", err)
	}
	stop(t, d)
	for _, dir := range []string{left, landing} {
		if _, err = os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatal("the start sweep left a read-only scratch tree", dir, err)
		}
	}
}
