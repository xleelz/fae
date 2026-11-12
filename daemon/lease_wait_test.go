package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

// FAE-OBS-003: a heavy-op lease wait becomes one task-history entry (and one
// log line) whether it ends granted or refused; an immediate grant is no wait.
func TestLiveAdmissionLeaseWaitIsTraced(t *testing.T) {
	for _, outcome := range []string{"granted", "refused", "immediate"} {
		t.Run(outcome, func(t *testing.T) {
			a := admissionFixture(t, 1)
			path := filepath.Join(t.TempDir(), "daemon.log")
			f, e := os.Create(path)
			if e != nil {
				t.Fatal(e)
			}
			defer f.Close()
			a.daemon.log = f
			var recorded []leaseWait
			a.daemon.obs.leaseWaits = func(w leaseWait) { recorded = append(recorded, w) }
			ctx := context.Background()
			// A live holder: an owner proven dead would be reclaimed (FAE-HOST-031).
			other := a.owner
			if outcome != "immediate" {
				if e = a.host.Enqueue(ctx, scheduler.Request{ID: "existing", Owner: other, Weight: 1, Queued: time.Now()}); e != nil {
					t.Fatal(e)
				}
				if _, _, e = a.host.Advance(ctx, time.Now(), map[scheduler.Owner]scheduler.Liveness{}); e != nil {
					t.Fatal(e)
				}
			}
			if outcome == "granted" {
				go func() {
					time.Sleep(250 * time.Millisecond)
					_ = a.host.Release(context.Background(), "existing", other, "done")
				}()
			}
			wait, cancel := context.WithTimeout(ctx, 2*time.Second)
			if outcome == "refused" {
				wait, cancel = context.WithTimeout(ctx, 250*time.Millisecond)
			}
			defer cancel()
			release, e := a.Acquire(wait, engine.Dispatch{TaskID: "task", Stage: "implement", ID: "dispatch", Config: workflow.Stage{Primitive: workflow.HostCommand, Weight: 1}}, execution.Binding{}, engine.Session{ID: "session"})
			if (outcome == "refused") != (e != nil) {
				t.Fatal(e)
			}
			if outcome == "refused" && !errors.Is(e, context.DeadlineExceeded) {
				t.Fatal(e)
			}
			if release != nil {
				if e = release(); e != nil {
					t.Fatal(e)
				}
			}
			raw, _ := os.ReadFile(path)
			if outcome == "immediate" {
				if len(recorded) != 0 || strings.Contains(string(raw), "lease wait") {
					t.Fatalf("immediate grant traced as a wait: %+v %s", recorded, raw)
				}
				return
			}
			if len(recorded) != 1 || recorded[0].Task != "task" || recorded[0].Stage != "implement" || recorded[0].Dispatch != "dispatch" || recorded[0].Lease != "heavy-op" || recorded[0].Outcome != outcome || recorded[0].Wait < 100*time.Millisecond {
				t.Fatalf("lease wait %+v", recorded)
			}
			if strings.Count(string(raw), "lease wait lease=heavy-op outcome="+outcome) != 1 || !strings.Contains(string(raw), `task="task" stage="implement" turn="dispatch"`) {
				t.Fatalf("lease wait line: %s", raw)
			}
		})
	}
}

// A role-slot wait is traced the same way, into the waiting task's history.
func TestLiveAdmissionRoleLeaseWaitIsTaskEvent(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	a := admissionFixture(t, 4)
	a.daemon = d
	b := execution.Binding{Document: workflow.Document{ID: "lease-wait", Roles: map[string]workflow.Role{"coder": {Scope: "workflow-global", Cap: 1}}}}
	release, e := a.Acquire(ctx, engine.Dispatch{TaskID: "first"}, b, engine.Session{ID: "one", Role: "coder"})
	if e != nil {
		t.Fatal(e)
	}
	defer release()
	wait, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
	defer cancel()
	if _, e = a.Acquire(wait, engine.Dispatch{TaskID: "second", Stage: "implement", ID: "dispatch-2"}, b, engine.Session{ID: "two", Role: "coder"}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	ev, e := d.store.LastJobEvent(ctx, "second", leaseWaitEventKind)
	if e != nil || !strings.Contains(string(ev.Payload), `"lease":"role:coder"`) || !strings.Contains(string(ev.Payload), `"outcome":"refused"`) || !strings.Contains(string(ev.Payload), `"dispatch_id":"dispatch-2"`) {
		t.Fatalf("role lease wait event %s %v", ev.Payload, e)
	}
	if _, e = d.store.LastJobEvent(ctx, "first", leaseWaitEventKind); e == nil {
		t.Fatal("an immediate grant was traced as a wait")
	}
}
