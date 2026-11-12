package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/defaults"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// FAE-SCH-003: the ready set is ordered by priority, then admission time;
// task ID only breaks exact ties or an unknown admission time.
func TestTaskOrderingUsesPriorityThenAdmissionTime(t *testing.T) {
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{}}
	for id, priority := range map[string]int{"a": 10, "b": 40, "c": 40, "d": 40, "e": 40} {
		raw, _ := json.Marshal(envelope{Admission: AdmitParams{Priority: priority}})
		d.jobs[id] = controlplane.JobSnapshot{Payload: raw}
	}
	now := time.Now().UTC()
	graph := controlplane.BacklogGraph{Items: []controlplane.BacklogItem{{ID: "a", Admitted: now}, {ID: "b", Admitted: now.Add(time.Minute)}, {ID: "c", Admitted: now}, {ID: "d", Admitted: now}}}
	ids, err := d.orderedTaskIDs(graph)
	if err != nil || !reflect.DeepEqual(ids, []string{"c", "d", "b", "e", "a"}) {
		t.Fatal(ids, err)
	}
	d.jobs["bad"] = controlplane.JobSnapshot{Payload: json.RawMessage(`invalid`)}
	if _, err = d.orderedTaskIDs(graph); err == nil {
		t.Fatal("malformed cache accepted")
	}
}

func TestTaskControlsNativeAuditsRestartAndRefusals(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "control", Tree: "tree", AuthorizingMessage: "message"}, "control"); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	control := func(method string, p TaskControlParams) {
		t.Helper()
		p.ID = "control"
		p.AuthorizingMessage = "message"
		if _, err := call(t, d, method, p, ""); err != nil {
			t.Fatal(method, err)
		}
		counts[method]++
	}
	control("Defer", TaskControlParams{})
	control("Undefer", TaskControlParams{})
	control("Defer", TaskControlParams{Until: time.Now().Add(time.Hour)})
	control("Reprioritize", TaskControlParams{Priority: 100})
	control("SetAttemptLimit", TaskControlParams{AttemptLimit: 31})
	control("RestartWithInformation", TaskControlParams{Information: "add acceptance case"})
	control("ResumeWithContext", TaskControlParams{Context: "later briefs context"})
	saved := d.jobs["control"]
	var before envelope
	if err = decodeEnvelope(saved.Payload, &before); err != nil {
		t.Fatal(err)
	}
	// A real owner/plane restart restores every scheduling/context control.
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	var after envelope
	if err = decodeEnvelope(d.jobs["control"].Payload, &after); err != nil {
		t.Fatal(err)
	}
	if !after.Job.Deferred || after.Admission.Priority != 100 || after.Job.AttemptLimit() != 31 || !reflect.DeepEqual(after.Job.ContextEntries, before.Job.ContextEntries) {
		t.Fatal("controls lost on restart")
	}
	control("Escalate", TaskControlParams{Reason: "operator review"})
	control("RestartWithInformation", TaskControlParams{Information: "review complete"})
	control("Block", TaskControlParams{Reason: "dependency unavailable"})
	if _, err = call(t, d, "Resume", TaskControlParams{ID: "control", AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("no-route block resumed")
	}
	// Prepare an actual declared remediation route and an attempt-cap checkpoint.
	d.mu.Lock()
	s := d.jobs["control"]
	var e envelope
	_ = decodeEnvelope(s.Payload, &e)
	for id, stage := range e.Job.Document.Stages {
		if _, ok := e.Job.Document.Stages[stage.Failure]; ok {
			e.Job.BlockedFrom = id
			e.Job.BlockedRoute = stage.Failure
			break
		}
	}
	s, err = d.persist(ctx, s, e, nil)
	d.jobs[s.ID] = s
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	control("Resume", TaskControlParams{})
	control("Escalate", TaskControlParams{Reason: "stop again"})
	d.mu.Lock()
	s = d.jobs["control"]
	_ = decodeEnvelope(s.Payload, &e)
	e.Job.Reason = "AttemptCap"
	e.Job.EscalationKind, e.Job.EscalationDetail = domain.AttemptCap, ""
	e.Job.Attempts = 31
	e.Job.AttemptCapStage = e.Job.Document.Start
	s, err = d.persist(ctx, s, e, nil)
	d.jobs[s.ID] = s
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	control("SetAttemptLimit", TaskControlParams{AttemptLimit: 32})
	control("Resume", TaskControlParams{})
	control("Block", TaskControlParams{Reason: "cancel held work"})
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "seed held edge", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO backlog_edges(from_id,to_id,kind,created_by,created_at) VALUES(?,?,?,?,?)`, "control", "dependent", "blocking", "test", time.Now().UTC())
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := call(t, d, "Cancel", TaskControlParams{ID: "control", Reason: "superseded", AuthorizingMessage: "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	counts["Cancel"]++
	body, _ := json.Marshal(response.Result)
	if !strings.Contains(string(body), `"held_dependents":["dependent"]`) {
		t.Fatal("missing held dependents", string(body))
	}
	held, err := d.store.HeldDependents(ctx, "control")
	if err != nil || !reflect.DeepEqual(held, []string{"dependent"}) {
		t.Fatal("cancel released dependency", held, err)
	}
	for method := range counts {
		if _, err = call(t, d, method, TaskControlParams{ID: "control", Reason: "reason", Information: "info", Context: "context", Priority: 10, AttemptLimit: 100, AuthorizingMessage: "message"}, ""); err == nil {
			t.Fatal("terminal accepted", method)
		}
	}
	var audits map[string]int = map[string]int{}
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect task control audits", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT action,COUNT(*) FROM audit WHERE message_id='message' GROUP BY action")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			var n int
			if err = rows.Scan(&a, &n); err != nil {
				return err
			}
			audits[a] = n
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	for method, want := range counts {
		if audits[method] != want {
			t.Fatalf("%s audits %d want %d", method, audits[method], want)
		}
	}
	if d.jobs["control"].State != string(domain.Cancelled) {
		t.Fatal("cancel not durable")
	}
	events, err := d.store.Events(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range events {
		if e.Kind == "task-control" {
			found++
		}
	}
	expected := 0
	for _, n := range counts {
		expected += n
	}
	if found != expected {
		t.Fatal("missing committed events", found, expected)
	}
}

func TestTaskControlCheckpointRefusesBeforeMutation(t *testing.T) {
	d := &Daemon{pendingCheckpoint: &jobCheckpoint{}}
	if _, err := d.taskControl(context.Background(), "Cancel", nil, controlplane.Audit{}); err == nil {
		t.Fatal("pending checkpoint accepted")
	}
	// An unknown task is refused only after the plane lookup of a terminal
	// row startup did not materialise (FAE-CP-009), so this half needs a plane.
	cfg, ctx := fixture(t)
	n, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, n) }()
	authorize(t, n)
	if _, err = call(t, n, "Cancel", TaskControlParams{ID: "missing", Reason: "r", AuthorizingMessage: "message"}, ""); err == nil || !strings.Contains(err.Error(), "unknown task") {
		t.Fatal("unknown task accepted", err)
	}
}

func TestTaskControlNativeLostAcknowledgementRetainsProposal(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "context", Tree: "tree", AuthorizingMessage: "message"}, "context"); err != nil {
		t.Fatal(err)
	}
	original := d.jobs["context"]
	p := TaskControlParams{ID: "context", Context: "append exactly once", AuthorizingMessage: "message"}
	if _, err = call(t, d, "ResumeWithContext", p, ""); err != nil {
		t.Fatal(err)
	}
	committed := d.jobs["context"]
	d.mu.Lock()
	defer d.mu.Unlock()
	d.jobs["context"] = original
	realStore := d.store
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	m.ExpectBegin().WillReturnError(errors.New("lost acknowledgement"))
	m.ExpectQuery("SELECT job_id").WillReturnError(errors.New("lost reconciliation read"))
	d.store = controlplane.New(db, controlplane.Options{Attempts: 1})
	raw, _ := json.Marshal(p)
	_, controlErr := d.taskControl(ctx, "ResumeWithContext", raw, controlplane.Audit{Actor: "operator", Action: "ResumeWithContext", Inputs: raw, MessageID: "message", Outcome: "ok"})
	d.store = realStore
	if err = m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if controlErr == nil || d.pendingCheckpoint == nil || !d.pendingCheckpoint.Control || !sameJobCheckpoint(d.jobs["context"], original) {
		t.Fatal("uncertain control forgotten")
	}
	if err = d.reconcileJobCheckpoint(ctx); err != nil {
		t.Fatal(err)
	}
	if d.pendingCheckpoint != nil || !sameJobCheckpoint(d.jobs["context"], committed) {
		t.Fatal("exact committed proposal not adopted")
	}
	var e envelope
	_ = decodeEnvelope(d.jobs["context"].Payload, &e)
	if len(e.Job.ContextEntries) != 1 {
		t.Fatal("duplicated append")
	}
}

type controlGateExecutor struct {
	started  chan engine.Dispatch
	release  chan struct{}
	delegate *engine.Scripted
}

func (x *controlGateExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	select {
	case x.started <- d:
	case <-ctx.Done():
		return engine.Result{}, ctx.Err()
	}
	select {
	case <-x.release:
	case <-ctx.Done():
		return engine.Result{}, ctx.Err()
	}
	return x.delegate.Execute(ctx, d)
}

func TestTaskControlNativeInFlightHoldAndLaterContext(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	script["intake"] = append(script["intake"], script["intake"][0])
	x := &controlGateExecutor{started: make(chan engine.Dispatch, 100), release: make(chan struct{}), delegate: engine.NewScripted(script)}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "held", Tree: "tree", AuthorizingMessage: "message"}, "held"); err != nil {
		t.Fatal(err)
	}
	var flight engine.Dispatch
	select {
	case flight = <-x.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, err = call(t, d, "Cancel", TaskControlParams{ID: "held", Reason: "unsafe", AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("cancelled active task")
	}
	if _, err = call(t, d, "ResumeWithContext", TaskControlParams{ID: "held", Context: "next turn only", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Escalate", TaskControlParams{ID: "held", Reason: "inspect completed turn", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	var e envelope
	_ = decodeEnvelope(d.jobs["held"].Payload, &e)
	d.mu.Unlock()
	if e.Job.InFlight.OperatorContext != flight.OperatorContext || e.Job.InFlight.ID != flight.ID || e.Job.State != domain.Escalated {
		t.Fatal("in-flight turn replaced")
	}
	close(x.release)
	deadline := time.Now().Add(10 * time.Second)
	collected := false
	for time.Now().Before(deadline) {
		e = envelope{}
		d.mu.Lock()
		_ = decodeEnvelope(d.jobs["held"].Payload, &e)
		d.mu.Unlock()
		if e.Job.InFlight == nil {
			collected = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !collected || e.Job.State != domain.Escalated || len(e.Job.Provenance) != 1 {
		t.Fatalf("held turn outcome lost collected=%v state=%s reason=%s flight=%v provenance=%d drive=%v cause=%v history=%+v", collected, e.Job.State, e.Job.Reason, e.Job.InFlight, len(e.Job.Provenance), d.driveAlive.Load(), d.driveCause.Load(), e.Job.History)
	}
	if _, err = call(t, d, "RestartWithInformation", TaskControlParams{ID: "held", Information: "continue now", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case next := <-x.started:
		if !strings.Contains(next.OperatorContext, "next turn only") || !strings.Contains(next.OperatorContext, "continue now") {
			t.Fatal("later turn lacks context")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	awaitState(t, d, "held", "Done")
}

func TestTaskControlsTicketTransitionsNeverEmitHeldCompletion(t *testing.T) {
	j, err := engine.New("task", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	cfg := TicketWorkflowConfig{Actions: map[string]TicketAction{}}
	for _, state := range []string{"Pending", "Blocked", "Cancelled", "Escalated", "Done"} {
		cfg.Actions[state] = TicketAction{Tool: "ticket", Action: state}
	}
	j.Disposition = domain.Disposition{Mode: domain.Normal, Live: true, TicketRoom: "room", Destination: "target", PushNamespace: "refs/fae"}
	j.ExecutionKind = "live"
	// Stage control events are replayed once, in their actual durable order.
	j.State = domain.State(j.Document.Start)
	j, _, err = engine.ApplyControl(j, engine.Control{Method: "Block", Reason: "blocked"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	j, _, err = engine.ApplyControl(j, engine.Control{Method: "Cancel", Reason: "cancel"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rows := ticketTransitions(j, cfg)
	if len(rows) != 3 || rows[0].Step != "Pending" || rows[1].Step != "Blocked" || rows[2].Step != "Cancelled" {
		t.Fatal("operator ticket order", rows)
	}
	// Complete's suppressed held transition has no destination and cannot close.
	j.History = append(j.History, engine.Event{Kind: "held-completion-transition", Stage: "final", Cause: "done result retained"})
	if rows = ticketTransitions(j, cfg); len(rows) != 3 {
		t.Fatal("held turn emitted ticket close", rows)
	}
}

func TestTaskControlNativeScriptedHeldUncertaintyEscalates(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "uncertain-held", Tree: "tree", AuthorizingMessage: "message"}, "uncertain-held"); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	s := d.jobs["uncertain-held"]
	var e envelope
	_ = decodeEnvelope(s.Payload, &e)
	var dispatch *engine.Dispatch
	for n := 0; n < 20 && dispatch == nil; n++ {
		e.Job, dispatch, _, err = engine.Prepare(e.Job)
		if err != nil {
			t.Fatal(err)
		}
	}
	e.Job, _, err = engine.ApplyControl(e.Job, engine.Control{Method: "Escalate", Reason: "operator hold"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	e.Job, _, err = engine.Recover(e.Job, false, true)
	if err != nil {
		t.Fatal(err)
	}
	s, err = d.persist(ctx, s, e, nil)
	d.jobs[s.ID] = s
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"redispatch", "accept"} {
		if _, err = call(t, d, "ResolveUncertain", ResolveUncertainParams{ID: s.ID, DispatchID: dispatch.ID, Action: action, AuthorizingMessage: "message"}, ""); err == nil {
			t.Fatal("held dispatch replay/adoption accepted", action)
		}
	}
	if _, err = call(t, d, "ResolveUncertain", ResolveUncertainParams{ID: s.ID, DispatchID: dispatch.ID, Action: "escalate", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	e = envelope{}
	_ = decodeEnvelope(d.jobs[s.ID].Payload, &e)
	if e.Job.InFlight != nil || e.Job.OperatorStage != "" || e.Job.EscalationKind != domain.OperatorEscalate {
		t.Fatal("held uncertainty not resolved")
	}
}

// FAE-WF-021, FAE-ROB-042: an operator checkpoint validates its row before
// saving it, as the drive, rearm and start do; an invalid row (here the
// wave-4 escalated task that kept its carry entry) is refused with nothing
// saved and no checkpoint pending.
func TestOperatorCheckpointRefusesAnInvalidTask(t *testing.T) {
	j, err := engine.New("invalid", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	j.State, j.EscalationKind, j.Reason, j.CarryEntry = domain.Escalated, domain.OperatorEscalate, "OperatorEscalate", "red-loop"
	raw, err := json.Marshal(envelope{Job: j})
	if err != nil {
		t.Fatal(err)
	}
	s := controlplane.JobSnapshot{ID: "invalid", State: string(j.State), Payload: raw}
	d := &Daemon{}
	if _, err = d.saveOperatorCheckpoint(context.Background(), controlplane.Audit{}, s, s); err == nil || !strings.Contains(err.Error(), "carry entry") || d.pendingCheckpoint != nil {
		t.Fatal("invalid operator checkpoint accepted", err)
	}
}

// FAE-OPS-010, FAE-ROB-001: an uncertainty resolution, live or a scripted
// operator hold, commits through saveOperatorCheckpoint, so an invalid result
// is refused before any save (the zero daemon has no store to save to) and
// leaves no checkpoint pending and the cache unchanged.
func TestUncertainResolutionRefusesAnInvalidRowBeforeSaving(t *testing.T) {
	j, err := engine.New("invalid", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	original := controlplane.JobSnapshot{ID: "invalid", State: string(j.State)}
	j.State, j.EscalationKind, j.Reason, j.CarryEntry = domain.Escalated, domain.OperatorEscalate, "OperatorEscalate", "red-loop"
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{}}
	p := ResolveUncertainParams{ID: "invalid", DispatchID: "dispatch", Action: "escalate"}
	if _, err = d.saveUncertainResolution(context.Background(), controlplane.Audit{}, p, original, envelope{}, j, nil); err == nil || !strings.Contains(err.Error(), "operator checkpoint refused") || !strings.Contains(err.Error(), "carry entry") || d.pendingCheckpoint != nil || len(d.jobs) != 0 {
		t.Fatal("invalid uncertainty resolution accepted", err)
	}
}

// FAE-WF-021, FAE-WF-086, FAE-OPS-010 (Dolt): an operator Escalate of a carried
// task still Pending stores a valid row without its carry entry. The drive
// and a restart load it without quarantine, and Retire, an Escalated out-edge,
// is accepted.
func TestDaemonEscalatedCarriedTaskRetiresWithoutQuarantine(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	script["first/test-first"] = []engine.ScriptStep{{Class: "execution-error"}, {Class: "execution-error"}, {Class: "execution-error"}, {Class: "execution-error"}}
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "first", Tree: "tree", Content: "make answer 42", AuthorizingMessage: "message"}, "first"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "first", "Escalated")
	// Hold the drive so the carried task stays Pending, as one waiting for
	// release does.
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "second", Tree: "tree", Content: "make answer 42", CarryFrom: "first", AuthorizingMessage: "message"}, "second"); err != nil {
		t.Fatal(err)
	}
	if e := liveEnvelope(t, d, "second"); e.Job.State != domain.Pending || e.Job.CarryEntry != "red-loop" {
		t.Fatal("fixture: carried task not Pending at its entry", e.Job.State, e.Job.CarryEntry)
	}
	if _, err = call(t, d, "Escalate", TaskControlParams{ID: "second", Reason: "operator review", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	e := liveEnvelope(t, d, "second")
	if e.Job.State != domain.Escalated || e.Job.CarryEntry != "" || engine.Validate(e.Job) != nil {
		t.Fatal("escalated carried row", e.Job.State, e.Job.CarryEntry, engine.Validate(e.Job))
	}
	// A restart validates every stored row, and the drive validates the
	// escalated row in every round it runs while another task finishes.
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	quarantined := func() string {
		d.mu.Lock()
		defer d.mu.Unlock()
		return d.quarantined["second"]
	}
	if reason := quarantined(); reason != "" {
		t.Fatal("restart quarantined the escalated carried task:", reason)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "third", Tree: "tree", Content: "another item", AuthorizingMessage: "message"}, "third"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "third", "Done")
	if reason := quarantined(); reason != "" {
		t.Fatal("the drive quarantined the escalated carried task:", reason)
	}
	if _, err = call(t, d, "Retire", TaskControlParams{ID: "second", Reason: "obsolete ruling/carried-task", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal("escalated carried task not retired:", err)
	}
	if e = liveEnvelope(t, d, "second"); e.Job.State != domain.Retired {
		t.Fatal("retired carried task", e.Job.State)
	}
}

// FAE-OPS-010, FAE-WF-074, FAE-CLI-013 (Dolt; operator ruling: keep per-task
// limits, no up-front refusal): resuming a raised AttemptCap task, and
// restarting an escalated task with information, reply with the budgets the
// new pass carries, each with its limit. Other controls reply as before.
func TestResumeReplyStatesCarriedBudgets(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "spent", Tree: "tree", RunID: "budgets", AuthorizingMessage: "message"}, "spent"); err != nil {
		t.Fatal(err)
	}
	// An AttemptCap row whose test runs and spec-loop are already spent.
	if _, err = call(t, d, "Escalate", TaskControlParams{ID: "spent", Reason: "fixture", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	s := d.jobs["spent"]
	var e envelope
	_ = decodeEnvelope(s.Payload, &e)
	e.Job.Reason, e.Job.EscalationKind, e.Job.EscalationDetail = "AttemptCap", domain.AttemptCap, ""
	e.Job.AttemptCapStage = e.Job.Document.Start
	e.Job.Attempts, e.Job.TestRuns, e.Job.Counts["spec-loop"] = 20, 8, 3
	s, err = d.persist(ctx, s, e, nil)
	d.jobs[s.ID] = s
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "SetAttemptLimit", TaskControlParams{ID: "spent", AttemptLimit: 25, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	type budget struct{ Used, Limit int }
	type budgets struct {
		Attempts budget            `json:"attempts"`
		TestRuns budget            `json:"test_runs"`
		Loops    map[string]budget `json:"loops"`
		Requests map[string]budget `json:"requests"`
	}
	want := func(b *budgets) bool {
		return b != nil && b.Attempts == (budget{20, 25}) && b.TestRuns == (budget{8, 8}) && b.Loops["spec-loop"] == (budget{3, 3}) && b.Requests["scope-widen"] == (budget{0, 3})
	}
	r, err := call(t, d, "ResumeRun", ResumeRunParams{Task: "spent", AuthorizingMessage: "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var run struct {
		Resumed []struct {
			Stage   string   `json:"stage"`
			Carried *budgets `json:"carried"`
		} `json:"resumed"`
	}
	if err = json.Unmarshal(r.Result, &run); err != nil || len(run.Resumed) != 1 || !want(run.Resumed[0].Carried) {
		t.Fatalf("resume reply %s %v", r.Result, err)
	}
	// Restart with information from an operator escalation states them too.
	if r, err = call(t, d, "Escalate", TaskControlParams{ID: "spent", Reason: "operator review", AuthorizingMessage: "message"}, ""); err != nil || strings.Contains(string(r.Result), `"carried"`) {
		t.Fatalf("escalate reply %s %v", r.Result, err)
	}
	if r, err = call(t, d, "RestartWithInformation", TaskControlParams{ID: "spent", Information: "smaller change", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	var restart struct {
		Carried *budgets `json:"carried"`
	}
	if err = json.Unmarshal(r.Result, &restart); err != nil || !want(restart.Carried) {
		t.Fatalf("restart reply %s %v", r.Result, err)
	}
}
