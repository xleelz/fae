package daemon

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestParallelAnyJoinCancelsUnfinishedSessionsImmediately(t *testing.T) {
	doc := workflow.Document{SchemaVersion: 1, ID: "cancel", Start: "fan", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3}, Roles: map[string]workflow.Role{"worker": {Instructions: "work", Briefs: map[string]string{"default": "work"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 3, Outputs: []string{"Note"}}}, Artifacts: map[string]workflow.ArtifactType{"Note": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}}, Stages: map[string]workflow.Stage{
		"fan": {Primitive: workflow.FanOut, Parallel: true, Join: "join", Branches: []workflow.FanBranch{{ID: "a", Start: "a"}, {ID: "b", Start: "b"}, {ID: "c", Start: "c"}}},
		"a":   {Primitive: workflow.AgentTurn, Role: "worker", Output: "Note"}, "b": {Primitive: workflow.AgentTurn, Role: "worker", Output: "Note"}, "c": {Primitive: workflow.AgentTurn, Role: "worker", Output: "Note"},
		"join": {Primitive: workflow.FanIn, Rule: workflow.JoinAny, Failure: "stop"}, "stop": {Primitive: workflow.Escalate},
	}, Edges: []workflow.Edge{{From: "fan", To: "a"}, {From: "fan", To: "b"}, {From: "fan", To: "c"}, {From: "a", To: "join"}, {From: "b", To: "join"}, {From: "c", To: "join"}, {From: "join", To: "Done"}, {From: "join", To: "stop"}}}
	j, err := engine.New("cancel", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	var started, cancelled atomic.Int32
	x := funcExecutor(func(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
		started.Add(1)
		r := engine.Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now(), Output: json.RawMessage(`{}`)}
		if d.Stage == "a" {
			for started.Load() < 3 {
				select {
				case <-ctx.Done():
					return r, ctx.Err()
				case <-time.After(time.Millisecond):
				}
			}
			return r, nil
		}
		select {
		case <-ctx.Done():
			cancelled.Add(1)
			return r, ctx.Err()
		case <-time.After(10 * time.Second):
			return r, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	start := time.Now()
	j, events, err := DriveParallelFanOut(ctx, j, x, engine.Options{})
	if err != nil || j.State != domain.Done || time.Since(start) > time.Second || cancelled.Load() != 2 {
		t.Fatal(err, j.State, time.Since(start), cancelled.Load())
	}
	n := 0
	for _, e := range events {
		if e.Kind == "branch-cancelled" {
			n++
		}
	}
	if n != 2 {
		t.Fatal("missing session cancellation events", n)
	}
}
