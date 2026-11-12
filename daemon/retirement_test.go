package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func TestRetirementReleaseAndContainmentGuard(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			if CanRetireContained(a, b) != (a && b) {
				t.Fatal(a, b)
			}
		}
	}
	for _, reason := range []string{"contained abc", "superseded-by next", "obsolete ruling", "bad"} {
		raw, _ := json.Marshal(envelope{Job: engine.Job{Reason: reason}})
		if prerequisiteSatisfied(controlplane.JobSnapshot{State: "Retired", Payload: raw}) != (reason == "contained abc") {
			t.Fatal(reason)
		}
	}
}

func TestRetirementNativeAtomicTransferCycleHoldAndReads(t *testing.T) {
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
	items := []BacklogItemParams{}
	for _, id := range []string{"old", "next", "dependent", "cycle"} {
		items = append(items, BacklogItemParams{AdmitParams: AdmitParams{ID: id, Tree: "tree", Priority: 10, Content: "accept " + id}})
	}
	_, err = call(t, d, "BacklogAdmit", BacklogParams{Items: items, Edges: []controlplane.BacklogEdge{{From: "old", To: "dependent", Kind: "blocking"}, {From: "next", To: "dependent", Kind: "blocking"}, {From: "dependent", To: "cycle", Kind: "blocking"}}, AuthorizingMessage: "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	control := func(id, method, reason string) error {
		_, err := call(t, d, method, TaskControlParams{ID: id, Reason: reason, AuthorizingMessage: "message"}, "")
		return err
	}
	if err = control("old", "Retire", "obsolete ruling"); err == nil {
		t.Fatal("non escalated accepted")
	}
	if err = control("old", "Escalate", "review"); err != nil {
		t.Fatal(err)
	}
	version := d.jobs["old"].Version
	for _, reason := range []string{"", "bad", "superseded-by missing", "superseded-by old", "superseded-by cycle", "contained deadbeef"} {
		if err = control("old", "Retire", reason); err == nil {
			t.Fatal("accepted", reason)
		}
		if d.jobs["old"].Version != version {
			t.Fatal("refusal mutated")
		}
	}
	if err = control("old", "Retire", "superseded-by next"); err != nil {
		t.Fatal(err)
	}
	g, err := d.store.Backlog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	transferred := false
	for _, edge := range g.Edges {
		if edge.From == "old" {
			t.Fatal("old edge retained")
		}
		if edge.From == "next" && edge.To == "dependent" {
			transferred = true
		}
	}
	if !transferred {
		t.Fatal("not transferred")
	}
	for _, i := range g.Items {
		if i.ID == "old" && i.State != "Retired" {
			t.Fatal("mirror not atomic")
		}
	}
	if _, err = call(t, d, "History", map[string]any{"id": "old"}, ""); err == nil {
		t.Fatal("retired history exposed")
	}
	if _, err = call(t, d, "History", map[string]any{"id": "old", "include_retired": true}, ""); err != nil {
		t.Fatal(err)
	}
	rows, err := d.store.VisibleEvents(ctx, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range rows {
		if event.JobID == "old" {
			t.Fatal("retired event exposed")
		}
	}

	// A later task superseded by an already-superseded identity must follow the
	// durable chain to the current successor, not create a permanent dead edge.
	_, err = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{{AdmitParams: AdmitParams{ID: "late", Tree: "tree", Priority: 10, Content: "accept late"}}}, Edges: []controlplane.BacklogEdge{{From: "late", To: "dependent", Kind: "blocking"}}, AuthorizingMessage: "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = control("late", "Escalate", "review"); err != nil {
		t.Fatal(err)
	}
	if err = control("late", "Retire", "superseded-by old"); err != nil {
		t.Fatal(err)
	}
	g, err = d.store.Backlog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, edge := range g.Edges {
		if edge.From == "old" || edge.From == "late" {
			t.Fatal("retired chain retained", edge)
		}
		if edge.From == "next" && edge.To == "dependent" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("chain did not deduplicate", g.Edges)
	}
	if err = control("next", "Escalate", "review"); err != nil {
		t.Fatal(err)
	}
	if err = control("next", "Retire", "superseded-by old"); err == nil {
		t.Fatal("supersession cycle accepted")
	}
	if err = control("next", "Retire", "obsolete ruling/decision"); err != nil {
		t.Fatal(err)
	}
	g, err = d.store.Backlog(ctx)
	if err != nil || len(blockedBacklog(g, d.jobs)["dependent"]) != 1 {
		t.Fatal("obsolete did not hold", err)
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	// Retired tasks are read from the plane, not materialised (FAE-CP-009).
	for _, id := range []string{"old", "next"} {
		if j, err := d.store.GetJob(ctx, id); err != nil || j.State != "Retired" {
			t.Fatal("retirement not durable", id, err)
		}
	}
}

// Use real resolving repository objects with independently recorded per-task
// receipts. A foreign ancestor deliberately resolves but must never suffice.
func TestRetirementContainedRequiresOwnResolvingReceipt(t *testing.T) {
	repo := t.TempDir()
	liveGit(t, repo, "init")
	liveGit(t, repo, "config", "user.email", "test@example.com")
	liveGit(t, repo, "config", "user.name", "Test")
	liveGit(t, repo, "commit", "--allow-empty", "-m", "unrelated ancestor")
	foreign := liveGit(t, repo, "rev-parse", "HEAD")
	liveGit(t, repo, "commit", "--allow-empty", "-m", "task own commit")
	own := liveGit(t, repo, "rev-parse", "HEAD")
	liveGit(t, repo, "commit", "--allow-empty", "-m", "task landing after reconciliation")
	landed := liveGit(t, repo, "rev-parse", "HEAD")
	j, err := engine.NewLive("owned", 1, defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	j.Base, j.Disposition = foreign, domain.Disposition{Mode: domain.Local}
	executor := engine.NewScripted(defaults.HappyScript())
	committed := false
	for n := 0; n < 200; n++ {
		next, dispatch, _, err := engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		j = next
		if dispatch == nil {
			continue
		}
		result, err := executor.Execute(context.Background(), *dispatch)
		if err != nil {
			t.Fatal(err)
		}
		result.ExecutionKind = "live"
		if dispatch.Config.Primitive == workflow.VcsCommit {
			result.Output, _ = json.Marshal(map[string]any{"sha": own, "files": []string{"answer.go"}})
			committed = true
		}
		if dispatch.Config.Primitive == workflow.Integrate {
			result.Output, _ = json.Marshal(map[string]any{"landed": true, "sha": landed})
		}
		j, _, err = engine.Complete(j, dispatch.ID, result)
		if err != nil {
			t.Fatal(err)
		}
		if committed {
			held := j
			held.State = domain.Escalated
			d := &Daemon{config: Config{Workspace: repo}}
			if err = d.validateRetirement(context.Background(), envelope{Job: held}, "contained "+own); err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{foreign, "HEAD", own[:8], "deadbeef"} {
				if err = d.validateRetirement(context.Background(), envelope{Job: held}, "contained "+bad); err == nil {
					t.Fatal("foreign/unresolved identity accepted", bad)
				}
			}
			if dispatch.Config.Primitive == workflow.Integrate {
				if err = d.validateRetirement(context.Background(), envelope{Job: held}, "contained "+landed); err != nil {
					t.Fatal("own landing rejected", err)
				}
				retired, _, err := engine.ApplyControl(held, engine.Control{Method: "Retire", Reason: "contained " + landed}, time.Now())
				if err != nil || retired.State != domain.Retired {
					t.Fatal(err)
				}
				return
			}
		}
	}
	t.Fatal("no integration evidence")
}

func TestRetirementBoardAndStatusOptIn(t *testing.T) {
	d := &Daemon{}
	d.status.Store(statusSnapshot{Tasks: []BoardItem{{JobIdentity: JobIdentity{ID: "a", State: "Retired"}}, {JobIdentity: JobIdentity{ID: "b", State: "Pending"}}}, Counts: map[string]int{"Retired": 1, "Pending": 1}, Updated: time.Now()})
	hidden, err := d.board(context.Background(), json.RawMessage(`{"limit":1}`))
	if err != nil || len(hidden.Items) != 1 || hidden.Items[0].ID != "b" {
		t.Fatal(hidden, err)
	}
	all, err := d.board(context.Background(), json.RawMessage(`{"include_retired":true,"limit":1}`))
	if err != nil || len(all.Items) != 1 || all.Items[0].ID != "a" || all.NextCursor != "a" {
		t.Fatal(all, err)
	}
	var normal, full map[string]any
	b, _ := json.Marshal(d.statusSummary())
	_ = json.Unmarshal(b, &normal)
	b, _ = json.Marshal(d.statusSummary(true))
	_ = json.Unmarshal(b, &full)
	if normal["total"] != float64(1) || full["total"] != float64(2) {
		t.Fatal(normal, full)
	}
	// Opt-in must not mutate the atomically shared snapshot for subsequent reads.
	again, err := d.board(context.Background(), json.RawMessage(`{}`))
	if err != nil || len(again.Items) != 1 {
		t.Fatal(again, err)
	}
}
