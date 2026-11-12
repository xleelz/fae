package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

func TestNestedFanNativeWorktreeJoinsBeforeObserverAndRetainsWrites(t *testing.T) {
	repo, base := initFanOutTestRepo(t)
	ctx := context.Background()
	manager := &worktree.Manager{Repository: repo, Root: filepath.Join(t.TempDir(), "trees")}
	broker := &effects.Broker{Manager: manager}
	parent, err := manager.Ensure(ctx, "run", "task", base)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := broker.Tree(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	cmd := func(name, scope string) workflow.Stage {
		return workflow.Stage{Primitive: workflow.HostCommand, Output: "Report", Command: []string{"sh", "-c", "printf data > " + name + "; printf '{}'"}, Generate: &workflow.GenerateSpec{Writes: []string{scope}}, Weight: 1, TimeoutSeconds: 5}
	}
	doc := workflow.Document{SchemaVersion: 2, ID: "nested-native", Start: "outer", Limits: workflow.Limits{Attempts: 20, TestRuns: 8, ScopeWidens: 3}, TargetRequirements: &workflow.TargetRequirements{}, Artifacts: map[string]workflow.ArtifactType{"Report": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}}, Stages: map[string]workflow.Stage{
		"outer": {Primitive: workflow.FanOut, Parallel: true, Join: "outerjoin", Branches: []workflow.FanBranch{{ID: "deep", Start: "inner", WriteScope: []string{"pkg/alpha/**"}}, {ID: "sibling", Start: "sibling", WriteScope: []string{"pkg/beta/**"}}}},
		"inner": {Primitive: workflow.FanOut, Parallel: true, Join: "innerjoin", Branches: []workflow.FanBranch{{ID: "a", Start: "a", WriteScope: []string{"pkg/alpha/a.go"}}, {ID: "b", Start: "b", WriteScope: []string{"pkg/alpha/b.go"}}}},
		"a":     cmd("pkg/alpha/a.go", "pkg/alpha/a.go"), "b": cmd("pkg/alpha/b.go", "pkg/alpha/b.go"), "sibling": cmd("pkg/beta/b.go", "pkg/beta/b.go"),
		"observe":   cmd("pkg/alpha/after", "pkg/alpha/after"),
		"innerjoin": {Primitive: workflow.FanIn, Rule: workflow.JoinAll, Failure: "stop"}, "outerjoin": {Primitive: workflow.FanIn, Rule: workflow.JoinAll, Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "failed"},
	}, Edges: []workflow.Edge{{From: "outer", To: "inner"}, {From: "outer", To: "sibling"}, {From: "inner", To: "a"}, {From: "inner", To: "b"}, {From: "a", To: "innerjoin"}, {From: "b", To: "innerjoin"}, {From: "innerjoin", To: "observe"}, {From: "innerjoin", To: "stop"}, {From: "observe", To: "outerjoin"}, {From: "sibling", To: "outerjoin"}, {From: "outerjoin", To: "Done"}, {From: "outerjoin", To: "stop"}}}
	doc.Stages["verify"] = workflow.Stage{Primitive: workflow.HostCommand, Output: "Report", Command: []string{"printf", `{"count":1}`}, Check: &workflow.CheckSpec{Format: "json", CountField: "count"}, Weight: 1, TimeoutSeconds: 5}
	for i := range doc.Edges {
		if doc.Edges[i].From == "outerjoin" && doc.Edges[i].To == "Done" {
			doc.Edges[i].To = "verify"
		}
	}
	doc.Edges = append(doc.Edges, workflow.Edge{From: "verify", To: "Done"})
	doc.Roles = map[string]workflow.Role{"author": {Instructions: "record", Briefs: map[string]string{"default": "record"}, Routes: []workflow.Binding{{Provider: "script", Model: "recorded"}}, Scope: "task-bound", Cap: 1, Outputs: []string{"Report"}}}
	for _, stage := range doc.Stages {
		if len(stage.Command) > 0 {
			doc.TargetRequirements.Commands = append(doc.TargetRequirements.Commands, stage.Command)
		}
	}
	broker.Allowed = doc.TargetRequirements.Commands
	job, err := engine.NewLive("task", 1, doc, tree)
	if err != nil {
		t.Fatal(err)
	}
	job.Base = base
	daemon := &Daemon{live: &liveExecutor{manager: manager, broker: broker}}
	observed := false
	for n := 0; n < 30 && job.State != domain.Done; n++ {
		var ds []*engine.Dispatch
		if job.Fan == nil {
			var dispatch *engine.Dispatch
			job, dispatch, _, err = engine.Prepare(job)
			if dispatch != nil {
				ds = append(ds, dispatch)
			}
		} else {
			job, ds, _, err = engine.PrepareParallelFanOut(job, engine.Options{})
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, dispatch := range ds {
			if dispatch.BranchID == "" {
				run, e := broker.Run(ctx, parent, effects.Command{Argv: dispatch.Config.Command, Check: dispatch.Config.Check, Timeout: 5 * time.Second})
				if e != nil {
					t.Fatal(e)
				}
				payload, _ := json.Marshal(run.CheckPayload)
				job, _, err = engine.Complete(job, dispatch.ID, engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: job.TaskID, Tree: dispatch.Tree, Session: dispatch.Sessions[0].ID, Output: payload, ProducedAt: time.Now().UTC()})
				if err != nil {
					t.Fatal(err)
				}
				continue
			}
			bw, e := branchWorktree(ctx, broker, parent, job, dispatch.BranchID)
			if e != nil {
				t.Fatalf("branch %s stage %s: %v state=%s frame=%+v", dispatch.BranchID, dispatch.Stage, e, job.State, job.Fan)
			}
			if dispatch.Stage == "observe" {
				for _, file := range []string{"pkg/alpha/a.go", "pkg/alpha/b.go"} {
					body, e := os.ReadFile(filepath.Join(bw.Binding.Path, file))
					if e != nil || string(body) != "data" {
						t.Fatalf("observer missing inner output %s: %q %v", file, body, e)
					}
				}
				observed = true
			}
			run, e := broker.Run(ctx, bw.Binding, effects.Command{Argv: dispatch.Config.Command, Timeout: 5 * time.Second, Generate: dispatch.Config.Generate, OutputSchema: json.RawMessage(`{"type":"object"}`)})
			if e != nil {
				t.Fatal(e)
			}
			committed, e := broker.CommitBranchWorktree(ctx, bw, nil, "native nested generator")
			if e != nil {
				t.Fatal(e)
			}
			payload, _ := json.Marshal(map[string]any{"exit_code": run.ExitCode, "timed_out": run.TimedOut, "changed": run.Changed, "changed_count": run.ChangedCount})
			result := engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: job.TaskID, Tree: dispatch.Tree, NewTree: committed.Tree, BranchCommit: committed.CommitSHA, BranchRef: committed.Ref, BranchBaseTree: bw.BaseTree, Session: dispatch.Sessions[0].ID, Output: payload, ProducedAt: time.Now().UTC()}
			before := job.Fan
			job, _, err = engine.Complete(job, dispatch.ID, result)
			if err != nil {
				t.Fatal(err)
			}
			if err = daemon.integrateFanBranchesAtJoin(ctx, "task", "run", before, &job); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(job)
			if err = json.Unmarshal(raw, &job); err != nil {
				t.Fatal(err)
			}
		}
		if job.State == domain.Escalated {
			t.Fatal(job.Reason, job.EscalationDetail)
		}
	}
	if job.State != domain.Done || !observed {
		t.Fatalf("state=%s observed=%v", job.State, observed)
	}
	for _, file := range []string{"pkg/alpha/a.go", "pkg/alpha/b.go", "pkg/alpha/after", "pkg/beta/b.go"} {
		body, e := os.ReadFile(filepath.Join(parent.Path, file))
		if e != nil || string(body) != "data" {
			t.Fatalf("root missing nested output %s: %q %v", file, body, e)
		}
	}
}
