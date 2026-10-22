package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/cron"
	"github.com/xleelz/fae/defaults"
	"testing"
	"time"
)

func cronRead(t *testing.T, d *Daemon, id string) cron.Schedule {
	t.Helper()
	res, e := call(t, d, "Schedule", ScheduleParams{Action: "inspect", ID: id}, "")
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(res.Result)
	var s cron.Schedule
	if e = json.Unmarshal(raw, &s); e != nil {
		t.Fatal(e)
	}
	return s
}
func cronSpec(id string) cron.Spec {
	return cron.Spec{ID: id, Kind: "cron", Anchor: time.Now().UTC().Add(time.Hour), IntervalSeconds: 60, Template: cron.Template{WorkflowID: "default", Tree: "tree", Content: "scheduled item"}}
}
func TestCronPinningCatchUpAndRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	p := cronSpec("bounded")
	p.End = p.Anchor.Add(121 * time.Second)
	p.CatchUpLimit = 2
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Cron: &p}, ""); e == nil {
		t.Fatal("unauthorized cron")
	}
	authorize(t, d)
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Cron: &p, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	s := cronRead(t, d, p.ID)
	if s.Spec.Template.Revision != 1 {
		t.Fatal("not pinned", s)
	}
	doc := defaults.Workflow()
	role := doc.Roles["spec"]
	role.Instructions += " New template revision."
	doc.Roles["spec"] = role
	if _, e = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	e = d.dispatchCrons(p.Anchor.Add(time.Hour))
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	s = cronRead(t, d, p.ID)
	if s.State != "completed" || s.Admitted != 2 || s.Skipped != 1 || s.Next != 3 {
		t.Fatal(s)
	}
	for _, i := range []int64{1, 2} {
		j, e := d.store.GetJob(ctx, s.TaskID(i))
		if e != nil || j.WorkflowRevision != 1 {
			t.Fatal("unpinned job", j, e)
		}
	}
	if _, e = d.store.GetJob(ctx, s.TaskID(0)); e != sql.ErrNoRows {
		t.Fatal("old catchup not skipped", e)
	}
	var n int
	e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "count scheduled admissions", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit WHERE action='cron occurrence admitted'").Scan(&n)
	})
	if e != nil || n != 2 {
		t.Fatal(n, e)
	}
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s = cronRead(t, d, p.ID)
	if s.State != "completed" || s.Admitted != 2 {
		t.Fatal(s)
	}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Cron: &p, AuthorizingMessage: "message"}, ""); e == nil {
		t.Fatal("duplicate id")
	}
}
func TestCronAdmissionCheckpointCrashGap(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	p := cronSpec("replay")
	p.End = p.Anchor.Add(time.Second)
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Cron: &p, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	before := cronRead(t, d, p.ID)
	d.mu.Lock()
	e = d.dispatchCrons(p.Anchor)
	if e == nil {
		d.crons[p.ID] = before
		e = d.saveCrons(ctx, controlplane.Audit{Actor: "test", Action: "simulate committed admission before cursor", Outcome: "ok"})
	}
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	task := before.TaskID(0)
	awaitState(t, d, task, "Done")
	completed, e := d.store.GetJob(ctx, task)
	if e != nil {
		t.Fatal(e)
	}
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	e = d.dispatchCrons(p.Anchor)
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	s := cronRead(t, d, p.ID)
	if s.Admitted != 1 || s.Next != 1 || s.State != "completed" {
		t.Fatal(s)
	}
	j, e := d.store.GetJob(ctx, task)
	if e != nil || j.Version != completed.Version || j.State != "Done" {
		t.Fatal("replay rewound durable job", j, e)
	}
	var n int
	e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "count gap replay", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit WHERE action='cron occurrence admitted'").Scan(&n)
	})
	if e != nil || n != 1 {
		t.Fatal("replayed admission", n, e)
	}
}
func TestCronLifecycleAndRoundBound(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	p := cronSpec("lifecycle")
	p.CatchUpLimit = 100
	p.IntervalSeconds = 1
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Cron: &p, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "pause", ID: p.ID, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	e = d.dispatchCrons(p.Anchor.Add(30 * time.Second))
	d.mu.Unlock()
	if e != nil || cronRead(t, d, p.ID).Admitted != 0 {
		t.Fatal("paused cron dispatched", e)
	}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "resume", ID: p.ID, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	e = d.dispatchCrons(p.Anchor.Add(30 * time.Second))
	s := d.crons[p.ID]
	d.mu.Unlock()
	if e != nil || s.Admitted != cronRoundLimit {
		t.Fatal("unbounded round", s, e)
	}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "retire", ID: p.ID, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	e = d.dispatchCrons(p.Anchor.Add(40 * time.Second))
	d.mu.Unlock()
	if e != nil || cronRead(t, d, p.ID).Admitted != cronRoundLimit {
		t.Fatal("retired cron dispatched", e)
	}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "resume", ID: p.ID, AuthorizingMessage: "message"}, ""); e == nil {
		t.Fatal("retired schedule resumed")
	}
}

func TestCronGrantSurvivesMessageAgeAndRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	done := cronSpec("historical")
	done.End = done.Anchor.Add(time.Second)
	active := cronSpec("ongoing")
	active.Anchor = done.Anchor.Add(time.Hour)
	active.End = active.Anchor.Add(time.Second)
	for _, p := range []cron.Spec{done, active} {
		if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Cron: &p, AuthorizingMessage: "message"}, ""); e != nil {
			t.Fatal(e)
		}
	}
	d.mu.Lock()
	e = d.dispatchCrons(done.Anchor)
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "age schedule authority", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE operator_messages SET created_at=? WHERE message_id='message'", time.Now().UTC().Add(-48*time.Hour))
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal("historical authority bricked startup", e)
	}
	if cronRead(t, d, "historical").State != "completed" {
		t.Fatal("lost completed schedule")
	}
	d.mu.Lock()
	e = d.dispatchCrons(active.Anchor)
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	if s := cronRead(t, d, "ongoing"); s.Admitted != 1 || s.State != "completed" {
		t.Fatal("authorized recurrence expired with message", s)
	}
}
func TestCronFairnessAcrossCatchUpBacklogs(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	first := cronSpec("aaa")
	first.IntervalSeconds = 1
	first.CatchUpLimit = 100
	second := first
	second.ID = "zzz"
	for _, p := range []cron.Spec{first, second} {
		if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", Cron: &p, AuthorizingMessage: "message"}, ""); e != nil {
			t.Fatal(e)
		}
	}
	d.mu.Lock()
	for i := 0; i < 2; i++ {
		if e = d.dispatchCrons(first.Anchor.Add(99 * time.Second)); e != nil {
			break
		}
	}
	a, b := d.crons[first.ID], d.crons[second.ID]
	d.mu.Unlock()
	if e != nil || a.Admitted != 10 || b.Admitted != 10 {
		t.Fatal("one backlog monopolized every round", a, b, e)
	}
}
