package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/loops"
	"github.com/xleelz/fae/processowner"
	agentruntime "github.com/xleelz/fae/runtime"
)

// Runtime tests exercise native Herdr panes. Here a real registered child plus
// deterministic injected census isolates daemon scheduling, SQL audit and events.
func TestDaemonIdleMaintenanceAuditAndStarvation(t *testing.T) {
	for _, kind := range []string{"sheddable", "protected", "foreign", "pending", "loop-active", "loop-pending"} {
		t.Run(kind, func(t *testing.T) {
			cfg, ctx, _ := liveDaemonFixture(t)
			binary := testHerdr(t)
			cfg.Live.Adapter = "multiplexer"
			cfg.Live.HerdrBinary = binary
			cfg.Live.PaneLimit = 60
			var socket string
			cfg.ConfigureIdleSessions = func(_ context.Context, pool *agentruntime.OwnedIdleSessions, owner string) error {
				cmd := exec.Command("/bin/sleep", "300")
				cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				record, err := processowner.StartLinked(cmd, filepath.Join(owner, ".processes"), owner, "idle-spec")
				if err != nil {
					return err
				}
				socket = filepath.Join(owner, "idle-spec.sock")
				err = pool.Register(agentruntime.IdleRegistration{ID: "idle-spec", Socket: socket, Role: "planner", Global: true, PreCode: true, Protected: kind == "protected", Records: []*processowner.Record{record}, Context: json.RawMessage(`{"summary":"retained"}`), Reap: func() error {
					err := cmd.Wait()
					var exit *exec.ExitError
					if errors.As(err, &exit) {
						return nil
					}
					return err
				}}, time.Now())
				if err != nil {
					_ = processowner.Terminate(ctx, record, owner)
					_ = cmd.Wait()
					_ = processowner.Retire(record)
				}
				return err
			}
			d, err := Start(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer stop(t, d)
			// A stopped drive runs no maintenance round of its own, including one it
			// began at start, whose census runs unlocked and would interleave here.
			stopDrive(t, d)
			d.mu.Lock()
			defer d.mu.Unlock()
			d.live.guard.inventory = func(context.Context) (map[string]int, error) {
				if kind == "foreign" {
					return map[string]int{socket: 1, "/foreign.sock": 59}, nil
				}
				return map[string]int{socket: 60}, nil
			}
			for _, id := range []string{"parked", "memory-pressure", "ready"} {
				job, err := engine.NewLive(id, 1, defaults.Workflow(), "tree")
				if err != nil {
					t.Fatal(err)
				}
				job.Disposition = domain.Disposition{Mode: domain.Local}
				if id == "parked" || id == "memory-pressure" {
					var dispatch *engine.Dispatch
					for dispatch == nil {
						job, dispatch, _, err = engine.Prepare(job)
						if err != nil {
							t.Fatal(err)
						}
					}
					reason := "host admission: PaneLimit"
					if id == "memory-pressure" {
						reason = "host admission: HostPressure"
					}
					job, _, err = engine.Complete(job, dispatch.ID, engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: id, Tree: dispatch.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC(), Class: "infrastructure-deferred", Detail: reason})
					if err != nil {
						t.Fatal(err)
					}
				}
				if kind == "pending" && id == "ready" {
					for step := 0; step < 8 && job.InFlight == nil; step++ {
						job, _, _, err = engine.Prepare(job)
						if err != nil {
							t.Fatal(err)
						}
					}
					if job.InFlight == nil {
						t.Fatal("fixture did not prepare pending turn")
					}
				}
				raw, _ := json.Marshal(envelope{Job: job, Admission: AdmitParams{ID: id}})
				snapshot, err := d.store.AdmitJob(ctx, controlplane.Audit{Actor: "test", Action: "admit idle fixture", Outcome: "ok"}, id, controlplane.JobSnapshot{ID: id, WorkflowID: job.Document.ID, WorkflowRevision: 1, TargetID: cfg.Workspace, TargetRevision: 1, State: string(job.State), Payload: raw})
				if err != nil {
					t.Fatal(err)
				}
				d.jobs[id] = snapshot
			}
			if kind == "loop-active" {
				d.loopActive["loop"] = func() {}
			}
			if kind == "loop-pending" {
				d.loops["loop"] = loops.Schedule{InFlight: true}
			}
			for round := 0; round < 3; round++ {
				if err := d.maintainIdleSessions(ctx); err != nil {
					t.Fatal(err)
				}
				if round < 2 && d.idleSessions.Count() != 1 {
					t.Fatal("shed before third round")
				}
			}
			wantCount := 1
			if kind == "sheddable" {
				wantCount = 0
			}
			if got := d.idleSessions.Count(); got != wantCount {
				t.Fatalf("sessions=%d want=%d", got, wantCount)
			}
			handoff, err := d.idleSessions.Handoff("idle-spec")
			if err != nil || !json.Valid(handoff) {
				t.Fatal("handoff lost", string(handoff), err)
			}
			for id, snapshot := range d.jobs {
				var e envelope
				if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
					t.Fatal(err)
				}
				wantEscalated := kind == "protected" && id == "parked"
				if (e.Job.Reason == "PaneCapStarvation") != wantEscalated {
					t.Fatalf("%s unexpectedly changed: state=%s reason=%s", id, e.Job.State, e.Job.Reason)
				}
			}
			var events, audits int
			expectedKind := "idle-session-shed"
			if kind == "protected" {
				expectedKind = "PaneCapStarvation"
			}
			err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect idle records", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
				if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE kind=?", expectedKind).Scan(&events); err != nil {
					return err
				}
				return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit WHERE action=?", expectedKind).Scan(&audits)
			})
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "sheddable" || kind == "protected" {
				want = 1
			}
			if events != want || audits != want {
				t.Fatalf("unpaired idle action events=%d audits=%d want=%d", events, audits, want)
			}
		})
	}
}

// idleSleeperConfig is a live multiplexer configuration whose idle pool
// registers one pre-code planner session, backed by a sleeping process, at
// start; the census is injected by each test.
func idleSleeperConfig(t *testing.T) (Config, context.Context, *exec.Cmd, *string) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.Live.Adapter = "multiplexer"
	cfg.Live.HerdrBinary = testHerdr(t)
	cfg.Live.PaneLimit = 60
	cmd := exec.Command("/bin/sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	socket := new(string)
	cfg.ConfigureIdleSessions = func(_ context.Context, pool *agentruntime.OwnedIdleSessions, owner string) error {
		record, err := processowner.StartLinked(cmd, filepath.Join(owner, ".processes"), owner, "idle-spec")
		if err != nil {
			return err
		}
		*socket = filepath.Join(owner, "idle-spec.sock")
		reap := func() error {
			var exit *exec.ExitError
			if err := cmd.Wait(); err != nil && !errors.As(err, &exit) {
				return err
			}
			return nil
		}
		if err = pool.Register(agentruntime.IdleRegistration{ID: "idle-spec", Socket: *socket, Role: "planner", Global: true, PreCode: true, Records: []*processowner.Record{record}, Context: json.RawMessage(`{}`), Reap: reap}, time.Now()); err != nil {
			_ = processowner.Terminate(ctx, record, owner)
			_ = cmd.Wait()
			_ = processowner.Retire(record)
		}
		return err
	}
	return cfg, ctx, cmd, socket
}

// FAE-RT-012: maintenance reconciles an idle slot whose pane was removed and
// whose process is gone, and records that reconciliation as an audited event.
func TestIdleMaintenanceRecordsReconciledSlot(t *testing.T) {
	cfg, ctx, sleeper, socket := idleSleeperConfig(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	// A stopped drive runs no maintenance round of its own, including one it
	// began at start, whose census runs unlocked and would interleave here.
	stopDrive(t, d)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.live.guard.inventory = func(context.Context) (map[string]int, error) { return map[string]int{*socket: 0}, nil }
	if err = syscall.Kill(-sleeper.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; {
		if all, e := processowner.Census(); e == nil {
			gone := true
			for _, p := range all {
				if p.PGID == sleeper.Process.Pid && !p.Defunct() {
					gone = false
				}
			}
			if gone {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("idle session process survived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err = d.maintainIdleSessions(ctx); err != nil {
		t.Fatal("reconciled idle slot not recorded:", err)
	}
	if d.idleSessions.Count() != 0 {
		t.Fatal("removed idle slot not reconciled")
	}
	var events int
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect idle records", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM events WHERE kind='idle-session-reconciled'").Scan(&events)
	})
	if err != nil || events != 1 {
		t.Fatal("reconciliation event", events, err)
	}
}

// FAE-ROB-021: idle-session maintenance runs its Herdr census, any retirement
// and its audit rows without the writer lock, so a mutating RPC during a slow
// census returns as soon as it is durable.
func TestAdmitDoesNotWaitForIdleMaintenance(t *testing.T) {
	cfg, ctx, _, socket := idleSleeperConfig(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	censusing, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	d.mu.Lock()
	d.live.guard.inventory = func(ctx context.Context) (map[string]int, error) {
		select {
		case censusing <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return map[string]int{*socket: 1}, nil
	}
	d.mu.Unlock()
	select {
	case <-censusing:
	case <-time.After(30 * time.Second):
		t.Fatal("idle maintenance never ran")
	}
	admitted := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := call(t, d, "Admit", AdmitParams{ID: "during-idle-maintenance", AuthorizingMessage: "message"}, "during-idle-maintenance")
		admitted <- err
	}()
	select {
	case err = <-admitted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		unblock()
		<-admitted
		t.Fatal("admit waited for idle maintenance's Herdr census")
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatal("admit waited for idle maintenance", elapsed)
	}
	if _, err = call(t, d, "Cancel", TaskControlParams{ID: "during-idle-maintenance", Reason: "test task", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	unblock()
	if d.idleSessions.Count() != 1 {
		t.Fatal("idle session lost")
	}
}
