package daemon

import (
	"context"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/worktree"
)

func TestTerminalScratchCleanupKeepsOwnedWorkAndRefusesLinks(t *testing.T) {
	repo, base := initFanOutTestRepo(t)
	ctx := context.Background()
	manager := &worktree.Manager{Repository: repo, Root: filepath.Join(t.TempDir(), "trees")}
	binding, err := manager.Ensure(ctx, "run", "task", base)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{live: &liveExecutor{manager: manager, broker: &effects.Broker{Manager: manager}}}
	scratch := filepath.Join(binding.Path, "scratch")
	if err = os.Mkdir(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(scratch, "out"), []byte("temporary"), 0600); err != nil {
		t.Fatal(err)
	}
	job := engine.Job{TaskID: "task", State: domain.Done, Document: workflow.Document{Roles: map[string]workflow.Role{"author": {Scratch: []string{"scratch/**"}}}}}
	d.mu.Lock()
	done := make(chan struct{})
	go func() { d.cleanupTaskDirectories(job, AdmitParams{RunID: "run"}); close(done) }()
	select {
	case <-done:
		d.mu.Unlock()
	case <-time.After(2 * time.Second):
		d.mu.Unlock()
		t.Fatal("terminal cleanup reacquired writer mutex")
	}

	if _, err = os.Stat(filepath.Join(scratch, "out")); !os.IsNotExist(err) {
		t.Fatal("scratch retained", err)
	}
	if _, err = os.Stat(filepath.Join(binding.Path, "pkg/alpha/a.go")); err != nil {
		t.Fatal("task work removed", err)
	}
	foreign := filepath.Join(t.TempDir(), "retained")
	if err = os.WriteFile(foreign, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(foreign, filepath.Join(scratch, "link")); err != nil {
		t.Fatal(err)
	}
	if err = d.cleanupScratchBinding(ctx, binding, engine.Job{}, []string{"scratch/**"}); err == nil {
		t.Fatal("link accepted")
	}
	if raw, err := os.ReadFile(foreign); err != nil || string(raw) != "foreign" {
		t.Fatal("foreign content changed")
	}
}
