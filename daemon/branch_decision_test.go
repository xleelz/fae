package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"sync"
	"testing"
	"time"
)

func branchDecisionFixture() workflow.Document {
	d := nestedStatsFixture()
	d.ID = "branch-decisions"
	d.Signature = &workflow.Signature{Inputs: map[string]json.RawMessage{"choices": json.RawMessage(`{"type":"array","items":{"type":"object","required":["id"]},"default":[{"id":"one"},{"id":"two"}]}`)}}
	for _, id := range []string{"a", "b"} {
		d.Stages[id] = workflow.Stage{Primitive: workflow.HumanDecision, Question: "Approve each item?", Output: "Note", OnTimeout: workflow.StayWaiting, Options: []workflow.DecisionOption{{ID: "accept", Next: "innerjoin"}, {ID: "reject", Next: "stop"}}, ItemsFrom: &workflow.Projection{Input: "choices"}, ItemOptions: []workflow.DecisionOption{{ID: "accept", Description: "keep"}, {ID: "reject", Description: "omit"}}}
		d.Edges = append(d.Edges, workflow.Edge{From: id, To: "stop"})
	}
	return d
}
func completeItemChoices() []workflow.ItemDecision {
	return []workflow.ItemDecision{{ID: "one", Option: "accept", Rationale: "first checked"}, {ID: "two", Option: "reject", Rationale: "second checked"}}
}
func TestNestedBranchDecisionsAreDistinctDurableAndRequireEveryItem(t *testing.T) {
	doc := branchDecisionFixture()
	j, err := engine.New("task", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	j, err = engine.BindTaskInputs(j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5 && j.Fan == nil; i++ {
		j, _, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
	}
	j, dispatches, _, err := engine.PrepareParallelFanOut(j, engine.Options{})
	if err != nil || len(dispatches) != 1 {
		t.Fatal(dispatches, err)
	}
	requests := engine.PendingDecisions(j)
	if len(requests) != 2 || requests[0].ID == requests[1].ID || requests[0].Branch == requests[1].Branch {
		t.Fatal(requests)
	}
	if _, _, _, err = engine.Decide(j, engine.Decision{Option: "accept", Items: completeItemChoices(), Message: "m"}); err == nil {
		t.Fatal("ambiguous task decision accepted")
	}
	if _, _, _, err = engine.Decide(j, engine.Decision{ID: requests[0].ID, Option: "accept", Items: completeItemChoices()[:1], Message: "m"}); err == nil {
		t.Fatal("incomplete item decision accepted")
	}
	j, _, rec, err := engine.Decide(j, engine.Decision{ID: requests[0].ID, Option: "accept", Items: completeItemChoices(), Message: "m"})
	if err != nil || rec.Branch != requests[0].Branch {
		t.Fatal(rec, err)
	}
	raw, _ := json.Marshal(j)
	_ = json.Unmarshal(raw, &j)
	pending := engine.PendingDecisions(j)
	if len(pending) != 1 || pending[0].ID != requests[1].ID {
		t.Fatal("decision identity lost on restart", pending)
	}
	if _, _, _, err = engine.Decide(j, engine.Decision{ID: requests[0].ID, Option: "accept", Items: completeItemChoices(), Message: "m"}); err == nil {
		t.Fatal("stale decision applied twice")
	}
	j, _, _, err = engine.Decide(j, engine.Decision{Branch: pending[0].Branch, Option: "accept", Items: completeItemChoices(), Message: "m"})
	if err != nil {
		t.Fatal(err)
	}
	script := engine.NewScripted(map[string][]engine.ScriptStep{"sibling": {{Output: json.RawMessage(`{}`)}}})
	r, err := script.Execute(context.Background(), *dispatches[0])
	if err != nil {
		t.Fatal(err)
	}
	j, _, err = engine.CompleteBranch(j, dispatches[0].BranchID, dispatches[0].ID, r)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8 && j.State != domain.Done; i++ {
		j, _, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
	}
	if j.State != domain.Done {
		t.Fatal(j.State, j.Reason)
	}
	branches := map[string]bool{}
	for _, p := range j.Provenance {
		if p.Primitive == workflow.HumanDecision {
			branches[p.BranchID] = true
		}
	}
	if !branches[requests[0].Branch] || !branches[requests[1].Branch] {
		t.Fatal("decision artifacts lost branch ownership", branches)
	}
}
func TestBranchDecideNativeWhileSiblingRunsAndAcrossRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	cfg.Executor = funcExecutor(func(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
		started <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return engine.Result{}, ctx.Err()
		}
		return engine.NewScripted(map[string][]engine.ScriptStep{"sibling": {{Output: json.RawMessage(`{}`)}}}).Execute(ctx, d)
	})
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
	doc := branchDecisionFixture()
	governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
	if _, err = call(t, d, "Admit", AdmitParams{ID: "branch-task", WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, "branch-task"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("sibling not dispatched")
	}
	load := func() engine.Job {
		row, e := d.store.GetJob(ctx, "branch-task")
		if e != nil {
			t.Fatal(e)
		}
		var envelope envelope
		if e = decodeEnvelope(row.Payload, &envelope); e != nil {
			t.Fatal(e)
		}
		return envelope.Job
	}
	requests := engine.PendingDecisions(load())
	if len(requests) != 2 {
		t.Fatal(requests)
	}
	if _, err = call(t, d, "Decide", DecideParams{ID: "branch-task", Decision: requests[0].ID, Option: "accept", Items: completeItemChoices(), AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal("decision blocked by independent sibling", err)
	}
	unblock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		active := d.active["branch-task"]
		d.mu.Unlock()
		if !active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sibling did not collect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	pending := engine.PendingDecisions(load())
	if len(pending) != 1 || pending[0].ID != requests[1].ID {
		t.Fatal(pending)
	}
	response, err := call(t, d, "Inbox", map[string]any{"task": "branch-task", "kinds": []string{"decisions"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	var inbox struct {
		Decisions []InboxTask `json:"decisions"`
	}
	_ = json.Unmarshal(response.Result, &inbox)
	if len(inbox.Decisions) != 1 || inbox.Decisions[0].Decision.Branch != pending[0].Branch {
		t.Fatal(string(response.Result))
	}
	if _, err = call(t, d, "Decide", DecideParams{ID: "branch-task", Branch: pending[0].Branch, Option: "accept", Items: completeItemChoices(), AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "branch-task", "Done")
}
