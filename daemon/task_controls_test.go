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

func TestTaskOrderingUsesPriorityThenStableID(t *testing.T) {
	d := &Daemon{jobs: map[string]controlplane.JobSnapshot{}}
	for id, priority := range map[string]int{"a": 10, "b": 40, "c": 40} {
		raw, _ := json.Marshal(envelope{Admission: AdmitParams{Priority: priority}})
		d.jobs[id] = controlplane.JobSnapshot{Payload: raw}
	}
	ids, err := d.orderedTaskIDs()
	if err != nil || !reflect.DeepEqual(ids, []string{"b", "c", "a"}) {
		t.Fatal(ids, err)
	}
	d.jobs["bad"] = controlplane.JobSnapshot{Payload: json.RawMessage(`invalid`)}
	if _, err = d.orderedTaskIDs(); err == nil {
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
	d.pendingCheckpoint = nil
	d.jobs = map[string]controlplane.JobSnapshot{}
	if _, err := d.taskControl(context.Background(), "Cancel", json.RawMessage(`{"id":"missing"}`), controlplane.Audit{}); err == nil {
		t.Fatal("unknown task accepted")
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
	if e.Job.InFlight != nil || e.Job.OperatorStage != "" || e.Job.Reason != "UncertainDispatchEscalated" {
		t.Fatal("held uncertainty not resolved")
	}
}
