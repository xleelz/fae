package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

func viewJob(t *testing.T, id string, priority int, doc workflow.Document, revision int, state domain.State) controlplane.JobSnapshot {
	t.Helper()
	raw, e := json.Marshal(envelope{Job: engine.Job{TaskID: id, Revision: revision, Document: doc, State: state}, Admission: AdmitParams{Priority: priority}})
	if e != nil {
		t.Fatal(e)
	}
	return controlplane.JobSnapshot{ID: id, WorkflowID: doc.ID, WorkflowRevision: int64(revision), State: string(state), Payload: raw, MoleculeID: testMolecule(doc, revision)}
}

// testMolecule is the molecule identity the plane binds for a revision whose
// content digest is a fixed fixture value.
func testMolecule(doc workflow.Document, revision int) string {
	id, _ := controlplane.MoleculeID(doc.ID, int64(revision), controlplane.Digest([]byte(doc.ID)))
	return id
}

func capsDoc(id string, global, bound int) workflow.Document {
	return workflow.Document{ID: id, Roles: map[string]workflow.Role{"oracle": {Scope: "workflow-global", Cap: global}, "coder": {Scope: "task-bound", Cap: bound}}}
}

func agentTurn(task string) engine.Dispatch {
	return engine.Dispatch{TaskID: task, Config: workflow.Stage{Primitive: workflow.AgentTurn}}
}

// The drive's per-round view: task priorities, molecules (one per workflow
// revision) and the workflow-global caps of revisions with non-terminal tasks.
func TestScheduleViewFromJobs(t *testing.T) {
	jobs := map[string]controlplane.JobSnapshot{
		"a": viewJob(t, "a", 30, capsDoc("wf", 1, 4), 1, "spec"),
		"b": viewJob(t, "b", 10, capsDoc("wf", 2, 4), 2, domain.Pending),
		"c": viewJob(t, "c", 10, capsDoc("wf", 3, 4), 3, domain.Done),
	}
	v, e := newScheduleView(jobs)
	if e != nil {
		t.Fatal(e)
	}
	if v.priority["a"] != 30 || v.molecule["b"] != testMolecule(capsDoc("wf", 2, 4), 2) || v.molecule["c"] != testMolecule(capsDoc("wf", 3, 4), 3) || v.revision["c"] != 3 {
		t.Fatal(v.priority, v.molecule)
	}
	choices := v.roleCaps()
	if len(choices) != 1 || choices[0].Workflow != "wf" || choices[0].Role != "oracle" || choices[0].Revision != 2 || choices[0].Cap != 2 || len(choices[0].Live) != 2 || choices[0].Live["1"] != 1 {
		t.Fatalf("%+v", choices)
	}
	jobs["bad"] = controlplane.JobSnapshot{Payload: json.RawMessage(`invalid`)}
	if _, e = newScheduleView(jobs); e == nil {
		t.Fatal("malformed job accepted")
	}
}

func awaitRoleHolders(t *testing.T, d *Daemon, key string, holders, waiting int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		p, e := d.store.LeaseSnapshot(context.Background(), key)
		if e == nil && len(p.Holders) == holders && len(p.Waiting) == waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("role pool %+v %v, want holders=%d waiting=%d", p, e, holders, waiting)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// FAE-SCH-006: revisions declaring caps 1 and 2 for one workflow-global role
// share a pool using 2 while the newer revision has live tasks, and 1 after.
// FAE-SCH-005: task-bound caps apply per molecule, shared by its tasks.
// FAE-SCH-008: a turn waiting for its role holds no pane and no heavy unit.
func TestRolePoolRevisionCapsMoleculesAndWaits(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	a := admissionFixture(t, 4)
	a.daemon = d
	paneFixture(t, a, 60)
	a.inventory = func(context.Context) (map[string]int, error) { return map[string]int{}, nil }
	old, newer := capsDoc("caps", 1, 1), capsDoc("caps", 2, 1)
	jobs := map[string]controlplane.JobSnapshot{
		"old-a": viewJob(t, "old-a", 10, old, 1, "spec"), "old-b": viewJob(t, "old-b", 10, old, 1, "spec"), "old-c": viewJob(t, "old-c", 10, old, 1, "spec"),
		"new": viewJob(t, "new", 10, newer, 2, "spec"),
	}
	view := func() {
		v, e := newScheduleView(jobs)
		if e != nil {
			t.Fatal(e)
		}
		a.view.Store(v)
	}
	view()
	global := roleTestKey(old, "", "oracle")
	var releases []func() error
	defer func() {
		for _, r := range releases {
			_ = r()
		}
	}()
	for _, task := range []string{"old-a", "old-b"} {
		release, e := a.Acquire(ctx, agentTurn(task), execution.Binding{Document: old}, engine.Session{ID: task + "-oracle", Role: "oracle"})
		if e != nil {
			t.Fatal("newest live revision cap not used", task, e)
		}
		releases = append(releases, release)
	}
	p, e := d.store.LeaseSnapshot(ctx, global)
	if e != nil || p.Capacity != 2 || len(p.Holders) != 2 {
		t.Fatal(p, e)
	}
	// The newer revision's last task finishes: the cap returns to 1 for future
	// grants while both holders keep their slots.
	jobs["new"] = viewJob(t, "new", 10, newer, 2, domain.Done)
	view()
	wait, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if _, e = a.Acquire(wait, agentTurn("old-c"), execution.Binding{Document: old}, engine.Session{ID: "old-c-oracle", Role: "oracle"}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("lowered cap admitted a third holder", e)
	}
	if p, e = d.store.LeaseSnapshot(ctx, global); e != nil || p.AdmissionLimit != 1 || len(p.Holders) != 2 || len(p.Waiting) != 0 {
		t.Fatal(p, e)
	}
	for _, r := range releases {
		if e = r(); e != nil {
			t.Fatal(e)
		}
	}
	releases = nil

	// Task-bound coder cap 1 is per molecule: old-a and old-b share revision
	// 1's slot, while the newer revision's task has its own. While old-b
	// waits for the slot it holds no pane reservation and no heavy unit.
	first, e := a.Acquire(ctx, agentTurn("old-a"), execution.Binding{Document: old}, engine.Session{ID: "old-a-coder", Role: "coder"})
	if e != nil {
		t.Fatal(e)
	}
	releases = append(releases, first)
	type result struct {
		release func() error
		err     error
	}
	waiting := make(chan result, 1)
	go func() {
		r, e := a.Acquire(ctx, agentTurn("old-b"), execution.Binding{Document: old}, engine.Session{ID: "old-b-coder", Role: "coder"})
		waiting <- result{r, e}
	}()
	molecule := moleculeTestKey(old, 1, "coder")
	awaitRoleHolders(t, d, molecule, 1, 1)
	if rows, e := a.panes.Snapshot(ctx); e != nil || len(rows) != 1 {
		t.Fatal("role waiter holds a pane", rows, e)
	}
	assertHostCounts(t, a, 0, 0)
	other, e := a.Acquire(ctx, agentTurn("new"), execution.Binding{Document: newer}, engine.Session{ID: "new-coder", Role: "coder"})
	if e != nil {
		t.Fatal("molecules share a task-bound pool", e)
	}
	releases = append(releases, other)
	if e = first(); e != nil {
		t.Fatal(e)
	}
	releases = releases[1:]
	select {
	case r := <-waiting:
		if r.err != nil {
			t.Fatal("freed molecule slot not taken by the next waiting task", r.err)
		}
		releases = append(releases, r.release)
	case <-time.After(5 * time.Second):
		t.Fatal("waiting task not admitted")
	}
}

func moleculeTestKey(doc workflow.Document, revision int, role string) string {
	return roleKey(doc.ID, "molecule\x00"+testMolecule(doc, revision), role)
}

// FAE-ROB-015: a role lease this daemon holds with no admission of its own
// (the session is gone, no turn in flight) is reclaimed within one round. A
// lease whose launch retirement is unproven, and one held by a live
// admission, are kept.
func TestStaleRoleLeaseSweptWithinRound(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	a := admissionFixture(t, 4)
	a.daemon = d
	if a.journal, e = scheduler.OpenJournal(filepath.Join(a.daemon.layout.Root, "proofs")); e != nil {
		t.Fatal(e)
	}
	doc := capsDoc("sweep", 3, 1)
	b := execution.Binding{Document: doc}
	live, e := a.Acquire(ctx, agentTurn("live"), b, engine.Session{ID: "live-oracle", Role: "oracle"})
	if e != nil {
		t.Fatal(e)
	}
	key := roleTestKey(doc, "", "oracle")
	audit := controlplane.Audit{Actor: "test", Action: "plant lease", Outcome: "ok"}
	for _, id := range []string{"orphan", "launching"} {
		if e = d.store.EnqueueLease(ctx, audit, key, scheduler.Request{ID: id, Owner: a.owner, Weight: 1, Queued: time.Now()}); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = d.store.AdvanceLeases(ctx, audit, key, time.Now(), map[scheduler.Owner]scheduler.Liveness{a.owner: scheduler.Alive}); e != nil {
		t.Fatal(e)
	}
	if e = a.journal.Put(scheduler.OperationProof{ID: "launching", Session: "launching", Instance: a.daemon.layout.Root, Owner: a.owner, Birth: a.birth, State: "launching"}); e != nil {
		t.Fatal(e)
	}
	a.sweepRoleLeases(ctx)
	p, e := d.store.LeaseSnapshot(ctx, key)
	if e != nil || len(p.Holders) != 2 || poolHolds(p, "orphan", a.owner) || !poolHolds(p, "launching", a.owner) || !poolHolds(p, a.proofID("live-oracle"), a.owner) {
		t.Fatal("stale lease not swept, or a live one was", p, e)
	}
	if e = live(); e != nil {
		t.Fatal(e)
	}
	// The slot freed by the sweep is taken by the next admission.
	next, e := a.Acquire(ctx, agentTurn("next"), b, engine.Session{ID: "next-oracle", Role: "oracle"})
	if e != nil {
		t.Fatal(e)
	}
	if e = next(); e != nil {
		t.Fatal(e)
	}
}
