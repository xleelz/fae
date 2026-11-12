package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
)

func TestVcsDispositionGuards(t *testing.T) {
	for bits := 0; bits < 8; bits++ {
		interrupted, proven, retried := bits&1 != 0, bits&2 != 0, bits&4 != 0
		want := "escalate"
		switch {
		case interrupted:
			want = "uncertain"
		case proven:
			want = "reverify"
		case !retried:
			want = "retry"
		}
		if got := IntegrateDisposition(interrupted, proven, retried); got != want {
			t.Fatal(bits, got, want)
		}
	}
	for bits := 0; bits < 16; bits++ {
		interrupted, committed, unchanged, retried := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0
		want := "escalate"
		switch {
		case interrupted:
			want = "uncertain"
		case committed:
			want = "committed"
		case unchanged && !retried:
			want = "retry"
		}
		if got := CommitDisposition(interrupted, committed, unchanged, retried); got != want {
			t.Fatal(bits, got, want)
		}
	}
}

// FAE-VCS-009: an integrate failure that is not a proven target move (here a
// held ref lock) never rests in the committing stage: the idempotent
// integration is retried once, then the task escalates with a typed reason,
// and a restart keeps exactly that disposition.
func TestDaemonIntegrateWriteFailureEscalatesTyped(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	lock := filepath.Join(cfg.Workspace, ".git", "refs", "heads", "fae", "integration.lock")
	defer os.Remove(lock)
	var locked atomic.Bool
	watch, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for watch.Err() == nil {
			if s, e := d.store.GetJob(watch, "locked"); e == nil && s.State == "post-merge-qa" {
				locked.Store(os.WriteFile(lock, nil, 0600) == nil)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if _, err = call(t, d, "Admit", AdmitParams{ID: "locked", AuthorizingMessage: "message"}, "locked"); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		e := snapshotEnvelope(t, awaitState(t, d, "locked", "Escalated"))
		if !locked.Load() || e.Job.Parked || e.Job.EscalationKind != domain.IntegrationConflict || !strings.Contains(e.Job.EscalationDetail, "integration refused after one retry") || !strings.Contains(e.Job.EscalationDetail, "lock") {
			t.Fatalf("integrate failure not typed: parked=%v kind=%s detail=%s", e.Job.Parked, e.Job.EscalationKind, e.Job.EscalationDetail)
		}
		escalations := 0
		for _, h := range e.Job.History {
			if h.Kind == "transition" && h.To == domain.Escalated {
				escalations++
			}
		}
		if escalations != 1 || e.Job.Counts["integrate"] != 1 {
			t.Fatal("disposition not taken exactly once", escalations, e.Job.Counts)
		}
		if liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef) != base {
			t.Fatal("refused integration moved the target")
		}
	}
	check()
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		d.signal()
		time.Sleep(100 * time.Millisecond)
	}
	check()
}
