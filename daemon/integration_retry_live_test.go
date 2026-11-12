package daemon

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
)

// FAE-VCS-009 integration retry on the production drive: the integration ref moves
// while the task is at post-merge QA, so Integrate refuses with a known no-write
// target move (kernel.CanRetryIntegration), routes through the bounded retry
// loop to a fresh reconciliation and lands on the moved tip.
func TestDaemonLiveIntegrationTargetMovedRetries(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	foreign := liveGit(t, cfg.Workspace, "commit-tree", base+"^{tree}", "-p", base, "-m", "foreign integration")
	var moved atomic.Bool
	watch, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for watch.Err() == nil {
			s, e := d.store.GetJob(watch, "retry-one")
			if e == nil && s.State == "post-merge-qa" {
				cmd := exec.Command("git", "update-ref", cfg.Live.IntegrationRef, foreign, base)
				cmd.Dir, cmd.Env = cfg.Workspace, cleanGitEnv()
				moved.Store(cmd.Run() == nil)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if _, err = call(t, d, "Admit", AdmitParams{ID: "retry-one", AuthorizingMessage: "message"}, "retry-one"); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitState(t, d, "retry-one", "Done")
	if !moved.Load() {
		t.Fatal("integration ref never moved")
	}
	var env envelope
	if err = json.Unmarshal(snapshot.Payload, &env); err != nil {
		t.Fatal(err)
	}
	retried := false
	for _, e := range env.Job.History {
		retried = retried || e.Kind == "integration-target-moved" && e.Stage == "integrate"
	}
	if !retried || env.Job.Counts["integration-retry-loop"] != 1 || env.Job.Counts["integrate"] != 2 || env.Job.State != domain.Done {
		t.Fatal("no bounded retry after the target moved", env.Job.Counts)
	}
	tip := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if parents := liveGit(t, cfg.Workspace, "rev-list", "--parents", "-n", "1", tip); !strings.Contains(parents, foreign) {
		t.Fatal("landed commit is not on the moved tip", parents, foreign)
	}
}
