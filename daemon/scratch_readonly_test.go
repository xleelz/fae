package daemon

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// FAE-HOST-020 (Dolt): scratch cleanup removes a scratch instance that holds
// a read-only tree a test left (Go's module cache: 0555 directories, 0444
// files). Before, the final removal failed with permission denied, and every
// retry failed the same way.
func TestScratchCleanupRemovesAReadOnlyTree(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Scratch = true
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	module := filepath.Join(d.layout.Root, "scratch-quarantine", "command-tmp-1-x", "command-tmp-1", "gopath", "pkg", "mod", "example.com", "m@v1.0.0")
	if err = os.MkdirAll(module, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(module, "go.mod"), []byte("module m\n"), 0444); err != nil {
		t.Fatal(err)
	}
	for dir := module; dir != filepath.Join(d.layout.Root, "scratch-quarantine"); dir = filepath.Dir(dir) {
		if err = os.Chmod(dir, 0555); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { // leave nothing the fixture's cleanup cannot remove
		_ = filepath.WalkDir(cfg.Root, func(p string, entry fs.DirEntry, _ error) error {
			if entry != nil && entry.IsDir() {
				_ = os.Chmod(p, 0700)
			}
			return nil
		})
	})
	running := d
	done := make(chan error, 1)
	go func() { done <- running.Run(ctx) }()
	result, err := CleanupScratch(ctx, ScratchCleanupParams{Home: cfg.Root, Instance: cfg.Instance, DoltBinary: cfg.DoltBinary, AuthorizingMessage: "message", Apply: true})
	if err != nil {
		t.Fatal("scratch cleanup of an instance holding a read-only tree:", err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	d = nil
	if !result.Completed {
		t.Fatal(result)
	}
	if _, err = os.Stat(result.Identity.Root); !os.IsNotExist(err) {
		t.Fatal("root retained", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(cfg.Root, ".scratch-removing")); len(entries) != 0 {
		t.Fatal("staged instance left", entries)
	}
}
