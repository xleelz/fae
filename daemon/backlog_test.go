package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
	"time"
)

func TestBacklogReleaseGuardAndKinds(t *testing.T) {
	for _, blocking := range []bool{false, true} {
		for _, done := range []bool{false, true} {
			if CanReleaseDependent(blocking, done) != (!blocking || done) {
				t.Fatal("guard")
			}
		}
	}
	g := controlplane.BacklogGraph{Edges: []controlplane.BacklogEdge{{From: "a", To: "b", Kind: "blocking"}, {From: "c", To: "d", Kind: "relates"}}}
	for _, state := range []string{"Pending", "Escalated", "Cancelled", "Done"} {
		held := blockedBacklog(g, map[string]controlplane.JobSnapshot{"a": {State: state}})
		if (len(held["b"]) == 0) != (state == "Done") || len(held["d"]) != 0 {
			t.Fatal(state, held)
		}
	}
}
func TestBacklogLandingIdentity(t *testing.T) {
	e := envelope{Job: engine.Job{ExecutionKind: "live", TaskID: "own", State: domain.Done, Evidence: []workflow.Evidence{{ID: "land", Fields: map[string]any{"landed": true, "sha": strings.Repeat("a", 40)}}}, Provenance: []engine.Provenance{{ArtifactID: "land", ExecutionKind: "live", TaskID: "other", Primitive: workflow.Integrate}}}}
	if landingSHA(e) != "" {
		t.Fatal("foreign landing")
	}
	e.Job.Provenance[0].TaskID = "own"
	if landingSHA(e) == "" {
		t.Fatal("own missing")
	}
	e.Job.State = domain.Escalated
	if landingSHA(e) != "" {
		t.Fatal("not Done")
	}
}
func TestBacklogNativeAtomicAdmissionEdgesMirroringAndRestart(t *testing.T) {
	cfg, ctx := fixture(t)
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
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	item := func(id string) BacklogItemParams {
		return BacklogItemParams{AdmitParams: AdmitParams{ID: id, Tree: "tree", Priority: 10, Content: "accept " + id}, Source: "ticket/" + id}
	}
	edge := controlplane.BacklogEdge{From: "a", To: "b", Kind: "blocking"}
	admit := func(p BacklogParams) error {
		p.AuthorizingMessage = "message"
		_, err := call(t, d, "BacklogAdmit", p, "")
		return err
	}
	for _, bad := range []BacklogParams{{Items: []BacklogItemParams{item("a"), item("b")}, Edges: []controlplane.BacklogEdge{edge, {From: "b", To: "a", Kind: "blocking"}}}, {Items: []BacklogItemParams{item("a"), item("a")}}, {Items: []BacklogItemParams{item("a")}, Edges: []controlplane.BacklogEdge{edge}}} {
		if err = admit(bad); err == nil {
			t.Fatal("invalid accepted")
		}
		if len(d.jobs) != 0 {
			t.Fatal("partial admission")
		}
		g, e := d.store.Backlog(ctx)
		if e != nil || len(g.Items) != 0 || len(g.Edges) != 0 {
			t.Fatal(g, e)
		}
	}
	unknown := item("unknown")
	unknown.WorkflowID = "missing"
	if err = admit(BacklogParams{Items: []BacklogItemParams{item("a"), unknown}}); err == nil {
		t.Fatal("unknown workflow")
	}
	if len(d.jobs) != 0 {
		t.Fatal("partial selector admission")
	}
	if err = admit(BacklogParams{Items: []BacklogItemParams{item("a"), item("b")}, Edges: []controlplane.BacklogEdge{edge}}); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "BacklogShow", map[string]string{}, ""); err != nil {
		t.Fatal(err)
	}
	g, err := d.store.Backlog(ctx)
	if err != nil || len(g.Items) != 2 || len(g.Edges) != 1 {
		t.Fatal(g, err)
	}
	if len(blockedBacklog(g, d.jobs)["b"]) != 1 {
		t.Fatal("dependent ready")
	}
	up := item("a")
	up.Priority = 30
	up.Content = "updated acceptance"
	if err = admit(BacklogParams{Items: []BacklogItemParams{up}}); err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err = decodeEnvelope(d.jobs["a"].Payload, &e); err != nil || e.Admission.Priority != 30 || e.Admission.Content != up.Content {
		t.Fatal(e, err)
	}
	if _, err = call(t, d, "BacklogEdge", BacklogEdgeParams{BacklogEdge: controlplane.BacklogEdge{From: "b", To: "a", Kind: "blocking"}, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("cycle accepted")
	}
	if _, err = call(t, d, "BacklogEdge", BacklogEdgeParams{BacklogEdge: controlplane.BacklogEdge{From: "a", To: "a", Kind: "relates"}, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("self accepted")
	}
	if _, err = call(t, d, "Cancel", TaskControlParams{ID: "a", Reason: "cancelled prerequisite", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	g, err = d.store.Backlog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if g.Items[0].State != "Cancelled" || len(blockedBacklog(g, d.jobs)["b"]) == 0 {
		t.Fatal("cancel released dependent", g)
	}
	if err = admit(BacklogParams{Items: []BacklogItemParams{up}}); err == nil {
		t.Fatal("terminal upsert accepted")
	}
	if _, err = call(t, d, "Escalate", TaskControlParams{ID: "b", Reason: "hold", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, accept := range []bool{false, true} {
		_, err = call(t, d, "BacklogEdge", BacklogEdgeParams{BacklogEdge: controlplane.BacklogEdge{From: "a", To: "b", Kind: "relates"}, AcceptInFlight: accept, AuthorizingMessage: "message"}, "")
		if (err == nil) != accept {
			t.Fatal(accept, err)
		}
	}
	if _, err = call(t, d, "BacklogEdge", BacklogEdgeParams{BacklogEdge: edge, Remove: true, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "BacklogEdge", BacklogEdgeParams{BacklogEdge: edge, Remove: true, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("absent remove accepted")
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	g, err = d.store.Backlog(ctx)
	if err != nil || len(g.Items) != 2 || len(g.Edges) != 1 || g.Items[0].State != "Cancelled" || g.Items[1].State != "Escalated" {
		t.Fatal(g, err)
	}
}
func TestBacklogNativeDoneReconcileNoopDryRunAndMissingSHA(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
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
	items := []BacklogItemParams{}
	for _, id := range []string{"landed", "missing", "held"} {
		items = append(items, BacklogItemParams{AdmitParams: AdmitParams{ID: id, Tree: "tree", Priority: 10, Content: id}})
	}
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: items, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Escalate", TaskControlParams{ID: "held", Reason: "genuine escalation", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	for _, id := range []string{"landed", "missing"} {
		s := d.jobs[id]
		var e envelope
		_ = decodeEnvelope(s.Payload, &e)
		e.Job.State = domain.Done
		e.Job.ExecutionKind = "live"
		s.State = "Done"
		if id == "landed" {
			fields := map[string]any{"landed": true, "sha": cfg.Live.Base}
			raw, _ := json.Marshal(fields)
			identity, _ := domain.ContentHash(raw)
			e.Job.Evidence = append(e.Job.Evidence, workflow.Evidence{ID: identity, Type: "Integration", Version: e.Job.Document.Artifacts["Integration"].Version, Stage: "integrate", TaskID: id, Tree: e.Job.Tree, Session: "fixture", Role: "host", Provenance: true, Fields: fields})
			e.Job.Provenance = append(e.Job.Provenance, engine.Provenance{ArtifactID: identity, ExecutionKind: "live", TaskID: id, Primitive: workflow.Integrate, Stage: "integrate", Tree: e.Job.Tree, Session: "fixture", Role: "host", DispatchID: "fixture", ProducedAt: time.Now().UTC()})
		}
		if err = engine.Validate(e.Job); err != nil {
			d.mu.Unlock()
			t.Fatal(err)
		}
		s.Payload, _ = json.Marshal(e)
		s, err = d.store.SaveJob(ctx, controlplane.Audit{Actor: "test", Action: "fixture", Outcome: "ok"}, s, s.Version)
		if err != nil {
			d.mu.Unlock()
			t.Fatal(err)
		}
		d.jobs[id] = s
	}
	d.mu.Unlock()
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "simulate historical mirror", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `UPDATE backlog_items SET state='Escalated'`)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false, false} {
		if _, err = call(t, d, "ReconcileDone", map[string]any{"dry_run": dry, "authorizing_message": "message"}, ""); err != nil {
			t.Fatal(err)
		}
		g, e := d.store.Backlog(ctx)
		if e != nil {
			t.Fatal(e)
		}
		for _, i := range g.Items {
			want := "Escalated"
			if i.ID == "landed" && !dry {
				want = "Done"
			}
			if i.State != want {
				t.Fatal(dry, i)
			}
		}
	}
	events, err := d.store.JobEvents(ctx, "landed", 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	repairs := 0
	for _, e := range events {
		if e.Kind == "backlog_reconciled" {
			repairs++
		}
	}
	if repairs != 1 {
		t.Fatal("repair not idempotent", repairs)
	}
	if err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "stale landed mirror at startup", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, `UPDATE backlog_items SET state='Escalated' WHERE item_id='landed'`)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	g, err := d.store.Backlog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range g.Items {
		want := "Escalated"
		if i.ID == "landed" {
			want = "Done"
		}
		if i.ID == "held" {
			continue
		}
		if i.State != want {
			t.Fatal("startup repair", i)
		}
	}

}

func TestBacklogUncertainReadbackBarrier(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	d := &Daemon{store: controlplane.New(db, controlplane.Options{}), backlogUncertain: true, jobs: map[string]controlplane.JobSnapshot{"old": {ID: "old"}}}
	m.ExpectQuery("SELECT job_id").WillReturnError(errors.New("plane unavailable"))
	if err = d.reconcileBacklogCheckpoint(context.Background()); err == nil || !d.backlogUncertain || len(d.jobs) != 1 {
		t.Fatal("unknown checkpoint escaped barrier", err)
	}
	m.ExpectQuery("SELECT job_id").WillReturnRows(sqlmock.NewRows([]string{"job_id", "version", "workflow_id", "workflow_revision", "target_id", "target_revision", "state", "payload_digest", "payload_size"}))
	if err = d.reconcileBacklogCheckpoint(context.Background()); err != nil || d.backlogUncertain || len(d.jobs) != 0 {
		t.Fatal("authoritative rollback not restored", err)
	}
	if !sameBacklogPayload(json.RawMessage(`{"b":2,"a":1}`), json.RawMessage(`{"a":1,"b":2}`)) || sameBacklogPayload(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":2}`)) {
		t.Fatal("payload comparison")
	}
	if err = m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type backlogCheckingExecutor struct {
	good     *engine.Scripted
	store    *controlplane.Store
	failures chan string
}

func (x *backlogCheckingExecutor) Execute(ctx context.Context, dispatch engine.Dispatch) (engine.Result, error) {
	if dispatch.TaskID == "dependent" {
		j, err := x.store.GetJob(ctx, "prerequisite")
		if err != nil || j.State != "Done" {
			select {
			case x.failures <- "dependent dispatched before prerequisite Done":
			default:
			}
		}
	}
	return x.good.Execute(ctx, dispatch)
}
func TestBacklogNativeDispatchWaitsForDone(t *testing.T) {
	cfg, ctx := fixture(t)
	x := &backlogCheckingExecutor{good: engine.NewScripted(defaults.HappyScript()), failures: make(chan string, 1)}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	x.store = d.store
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	ids := []string{"prerequisite", "dependent", "related"}
	items := []BacklogItemParams{}
	for _, id := range ids {
		doc := defaults.Workflow()
		doc.ID = "workflow-" + id
		if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
		items = append(items, BacklogItemParams{AdmitParams: AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", Priority: 10, Content: id}})
	}
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: items, Edges: []controlplane.BacklogEdge{{From: "prerequisite", To: "dependent", Kind: "blocking"}, {From: "prerequisite", To: "related", Kind: "relates"}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Defer", TaskControlParams{ID: "prerequisite", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	x.store = d.store
	// A relates edge must not hold its task while the blocking edge survives restart.
	awaitState(t, d, "related", "Done")
	d.mu.Lock()
	var dependent envelope
	err = decodeEnvelope(d.jobs["dependent"].Payload, &dependent)
	state := d.jobs["dependent"].State
	for _, id := range ids {
		if d.jobs[id].WorkflowID != "workflow-"+id {
			d.mu.Unlock()
			t.Fatal("selector lost", id)
		}
	}
	d.mu.Unlock()
	if err != nil || state != "Pending" || dependent.Job.Sequence != 0 {
		t.Fatal("dependent escaped restarted hold", state, err)
	}
	if _, err = call(t, d, "Undefer", TaskControlParams{ID: "prerequisite", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "prerequisite", "Done")
	awaitState(t, d, "dependent", "Done")
	select {
	case failure := <-x.failures:
		t.Fatal(failure)
	default:
	}
	g, err := d.store.Backlog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range g.Items {
		if i.State != "Done" {
			t.Fatal("mirror stale", i)
		}
	}
}

func TestBacklogUnknownCommitBlocksDrainAndDeployment(t *testing.T) {
	d := &Daemon{backlogUncertain: true, draining: true}
	d.publishStatus()
	s := d.status.Load().(statusSnapshot)
	if s.Drained {
		t.Fatal("unknown commit reported drained")
	}
	if err := d.deploymentHolds(context.Background(), controller.Params{}, &controller.Health{}, controlplane.Audit{}); err == nil {
		t.Fatal("deployment allowed")
	}
	d.backlogUncertain = false
	d.publishStatus()
	if !d.status.Load().(statusSnapshot).Drained {
		t.Fatal("resolved empty daemon not drained")
	}
}
