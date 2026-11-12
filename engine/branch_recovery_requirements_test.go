package engine_test

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
	"time"
)

func branchRecoveryDoc(width int) workflow.Document {
	d := scenarioLeafDoc()
	d.ID = "branch-recovery"
	d.Start = "fan"
	d.Artifacts["Note"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["text"],"properties":{"text":{"type":"string","minLength":2}},"additionalProperties":false}`)}
	d.Artifacts["Joined"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"array","items":{"type":"object"}}`)}
	items := make([]int, width)
	for i := range items {
		items[i] = i
	}
	raw, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{"items": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}}, "required": []string{"items"}, "default": map[string]any{"items": items}})
	d.Signature = &workflow.Signature{Inputs: map[string]json.RawMessage{"batch": raw}}
	retries := 1
	d.Stages = map[string]workflow.Stage{
		"fan":  {Primitive: workflow.FanOut, Parallel: true, Join: "join", BranchesFrom: &workflow.BranchesFromSpec{Input: "batch", Field: "items", Min: 1, Max: width}, BranchTemplate: &workflow.FanBranch{ID: "template", Start: "work", Output: "Note"}},
		"work": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note", SchemaRetries: &retries},
		"join": {Primitive: workflow.FanIn, Output: "Joined", Rule: workflow.JoinSettle, Failure: "stop"},
		"stop": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "join failed"},
	}
	d.Edges = []workflow.Edge{{From: "fan", To: "work"}, {From: "work", To: "join"}, {From: "join", To: "Done"}, {From: "join", To: "stop"}}
	d.Limits.Attempts = 100
	return d
}
func newBranchRecoveryJob(t *testing.T, d workflow.Document) (engine.Job, []*engine.Dispatch) {
	t.Helper()
	j, err := engine.NewLive("root", 1, d, "tree")
	if err != nil {
		t.Fatal(err)
	}
	j, err = engine.BindTaskInputs(j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	j, first, _, err := engine.Prepare(j)
	if err != nil {
		t.Fatal(err)
	}
	if first != nil {
		t.Fatal("fan initial preparation should not dispatch yet")
	}
	j, ds, _, err := engine.PrepareParallelFanOut(j, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return j, ds
}
func branchResult(d *engine.Dispatch, output string, class string) engine.Result {
	return engine.Result{DispatchID: d.ID, TaskID: d.TaskID, ExecutionKind: d.ExecutionKind, Tree: d.Tree, Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: json.RawMessage(output), Class: class}
}

func TestBranchLocalSchemaRetriesAndVisits(t *testing.T) {
	j, ds := newBranchRecoveryJob(t, branchRecoveryDoc(4))
	if len(ds) != 4 {
		t.Fatal(len(ds))
	}
	invalid := []string{`{"text":0}`, `{}`, `{"text":""}`, `{"text":"ok","extra":true}`}
	for i, d := range ds {
		if d.Visit != 1 || len(d.PriorRejection) != 0 {
			t.Fatal("sibling state leaked into first visit", d)
		}
		var err error
		j, _, err = engine.CompleteBranch(j, d.BranchID, d.ID, branchResult(d, invalid[i], ""))
		if err != nil {
			t.Fatal(i, err)
		}
	}
	j, retries, _, err := engine.PrepareParallelFanOut(j, engine.Options{})
	if err != nil || len(retries) != 4 {
		t.Fatal(err, len(retries), j.Reason)
	}
	seen := map[string]bool{}
	for _, d := range retries {
		if d.Visit != 2 || len(d.PriorRejection) == 0 || seen[d.BranchInstance] {
			t.Fatal("retry not local", d)
		}
		seen[d.BranchInstance] = true
		view, ok := engine.BranchJob(j, d.BranchID)
		if !ok || strings.Join(d.PriorRejection, "\n") != strings.Join(view.PriorRejections["work"], "\n") {
			t.Fatal("wrong prior rejection", d)
		}
		j, _, err = engine.CompleteBranch(j, d.BranchID, d.ID, branchResult(d, `{"text":"ok"}`, ""))
		if err != nil {
			t.Fatal(err)
		}
	}
	if j.State != domain.Done || j.Attempts != 5 {
		t.Fatal(j.State, j.Attempts, j.Reason)
	}
	if err = engine.Validate(j); err != nil {
		t.Fatal(err)
	}
}
func TestBranchWidthDoesNotChargeFirstVisits(t *testing.T) {
	j, ds := newBranchRecoveryJob(t, branchRecoveryDoc(32))
	if len(ds) != 32 || j.Attempts != 1 {
		t.Fatal(len(ds), j.Attempts)
	}
	for i, d := range ds {
		raw := `{"text":"ok"}`
		if i == 0 {
			raw = `{"text":0}`
		}
		var err error
		j, _, err = engine.CompleteBranch(j, d.BranchID, d.ID, branchResult(d, raw, ""))
		if err != nil {
			t.Fatal(i, err)
		}
	}
	j, ds, _, err := engine.PrepareParallelFanOut(j, engine.Options{})
	if err != nil || len(ds) != 1 {
		t.Fatal(err, len(ds))
	}
	j, _, err = engine.CompleteBranch(j, ds[0].BranchID, ds[0].ID, branchResult(ds[0], `{"text":"ok"}`, ""))
	if err != nil || j.State != domain.Done || j.Attempts != 2 {
		t.Fatal(err, j.State, j.Attempts)
	}
}
func TestRefundedBranchDoesNotInvalidateSiblingAndCheckpointsAreLocal(t *testing.T) {
	for _, class := range []string{engine.ClassCheckpointInterrupted, engine.ClassTurnBudgetExhausted, "infrastructure-deferred", engine.ClassQuotaCutoff} {
		t.Run(class, func(t *testing.T) {
			doc := branchRecoveryDoc(2)
			if class == engine.ClassTurnBudgetExhausted {
				s := doc.Stages["work"]
				s.TurnBudget = &workflow.TurnBudgetSpec{WallSeconds: 1, OnExhaustion: "resume-once"}
				doc.Stages["work"] = s
			}
			j, ds := newBranchRecoveryJob(t, doc)
			if len(ds) != 2 {
				t.Fatal(len(ds))
			}
			r := branchResult(ds[0], "", class)
			if class == engine.ClassCheckpointInterrupted {
				r.Checkpoint = &engine.TurnCheckpoint{ProviderSession: "checkpoint-branch-0", WorktreeTree: "tree"}
			}
			var err error
			j, _, err = engine.CompleteBranch(j, ds[0].BranchID, ds[0].ID, r)
			if err != nil {
				t.Fatal(err)
			}
			refunded, ok := engine.BranchJob(j, ds[0].BranchID)
			if !ok || refunded.Counts["work"] != 0 || refunded.InFlight != nil {
				t.Fatal("dispatch was not refunded in own frame", refunded.Counts)
			}
			j, _, err = engine.CompleteBranch(j, ds[1].BranchID, ds[1].ID, branchResult(ds[1], `{"text":"sibling"}`, ""))
			if err != nil {
				t.Fatal("sibling rejected after refund", err)
			}
			if class == engine.ClassCheckpointInterrupted {
				j, retry, _, err := engine.PrepareBranch(j, ds[0].BranchID)
				if err != nil || retry == nil || retry.ResumeCheckpoint == nil || retry.ResumeCheckpoint.ProviderSession != "checkpoint-branch-0" {
					t.Fatal(j.Reason, retry, err)
				}
			}
		})
	}
}
func TestParallelBranchCheckpointRecoveryResumesOnlyOwnProvider(t *testing.T) {
	j, ds := newBranchRecoveryJob(t, branchRecoveryDoc(2))
	for i, d := range ds {
		if !j.SaveDispatchCheckpoint(*d, engine.TurnCheckpoint{ProviderSession: fmt.Sprintf("provider-%d", i), WorktreeTree: "tree"}) {
			t.Fatal("checkpoint not assigned")
		}
	}
	raw, _ := json.Marshal(j)
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatal(err)
	}
	j, _, err := engine.Recover(j, false, false)
	if err != nil {
		t.Fatal(err)
	}
	j, retries, _, err := engine.PrepareParallelFanOut(j, engine.Options{})
	if err != nil || len(retries) != 2 {
		t.Fatal(err, len(retries), j.Reason)
	}
	for i, d := range retries {
		want := fmt.Sprintf("provider-%d", i)
		if d.ResumeCheckpoint == nil || d.ResumeCheckpoint.ProviderSession != want || d.Sessions[0].ResumeSessionID != want {
			t.Fatal("checkpoint/provider crossed branches", d, want)
		}
	}
}

func TestBranchFailureRoutesSharedJoinAndDefaultEscalation(t *testing.T) {
	for _, mode := range []string{"explicit-budget", "fallback-budget", "fallback-schema", "unrouted"} {
		t.Run(mode, func(t *testing.T) {
			doc := branchRecoveryDoc(3)
			join := doc.Stages["join"]
			join.Quorum = 2
			doc.Stages["join"] = join
			work := doc.Stages["work"]
			work.TurnBudget = &workflow.TurnBudgetSpec{WallSeconds: 1, OnExhaustion: "fail"}
			if mode == "explicit-budget" {
				work.TurnBudget.OnExhaustion = "join"
			}
			if strings.HasPrefix(mode, "fallback") {
				fan := doc.Stages["fan"]
				fan.BranchFailure = "fail-branch"
				doc.Stages["fan"] = fan
			}
			if mode == "fallback-schema" {
				n := 0
				work.SchemaRetries = &n
			}
			doc.Stages["work"] = work
			if issues := workflow.Validate(doc); len(issues) > 0 {
				t.Fatal(issues)
			}
			j, ds := newBranchRecoveryJob(t, doc)
			if len(ds) != 3 {
				t.Fatal(len(ds))
			}
			class, raw, reason := engine.ClassTurnBudgetExhausted, "", "budget"
			if mode == "fallback-schema" {
				class, raw, reason = "", `{"text":0}`, "schema"
			}
			var err error
			j, _, err = engine.CompleteBranch(j, ds[0].BranchID, ds[0].ID, branchResult(ds[0], raw, class))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "unrouted" {
				if j.State != domain.Escalated {
					t.Fatal("default failure must escalate task", j.State, j.Reason)
				}
				return
			}
			for _, d := range ds[1:] {
				j, _, err = engine.CompleteBranch(j, d.BranchID, d.ID, branchResult(d, `{"text":"ok"}`, ""))
				if err != nil {
					t.Fatal("settle stopped sibling", err)
				}
			}
			if j.State != domain.Done || j.LastFan == nil || j.LastFan.Branches[0].Status != engine.BranchFailed || j.LastFan.Branches[0].FailureReason != reason {
				t.Fatal("failure route laundered into pass", j.State, j.Reason, j.LastFan)
			}
			found := false
			for _, a := range j.Evidence {
				if a.Type == "Joined" {
					items, _ := a.Fields["items"].([]any)
					if len(items) == 3 {
						item, _ := items[0].(map[string]any)
						found = item["status"] == engine.BranchFailed && item["reason"] == reason && item["error"] != ""
					}
				}
			}
			if !found {
				t.Fatal("join missing typed branch failure")
			}
		})
	}
}
func TestSharedBranchGateJoinAndSameGateCounterIsolation(t *testing.T) {
	doc := branchRecoveryDoc(5)
	fan := doc.Stages["fan"]
	fan.BranchTemplate.Start = "check"
	fan.BranchTemplate.Output = ""
	doc.Stages["fan"] = fan
	delete(doc.Stages, "work")
	expr := bin("eq", fieldOf("tree", "depth"), lit(2))
	doc.Stages["check"] = workflow.Stage{Primitive: workflow.Gate, Name: "branch check", Expr: &expr, Failure: "join"}
	doc.Edges = []workflow.Edge{{From: "fan", To: "check"}, {From: "check", To: "join"}, {From: "join", To: "Done"}, {From: "join", To: "stop"}}
	// No branch output is needed for failed branches; this fixture exercises a
	// shared success/failure edge without an unreachable pass-through stage.
	j, err := engine.New("root", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	j, err = engine.BindTaskInputs(j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20 && !j.State.Terminal() && j.State != domain.Escalated; i++ {
		j, _, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
	}
	if j.State != domain.Escalated || j.EscalationKind == domain.SameGate || j.LastFan == nil || j.Attempts != 5 {
		t.Fatal(j.State, j.Reason)
	}
	for _, b := range j.LastFan.Branches {
		if b.Status != engine.BranchFailed || b.FailureReason != "gate" {
			t.Fatal(b)
		}
	}
}

func TestBranchPreemptionRefundKeepsSiblingAndOwnCheckpoint(t *testing.T) {
	j, ds := newBranchRecoveryJob(t, branchRecoveryDoc(2))
	j.SaveDispatchCheckpoint(*ds[0], engine.TurnCheckpoint{ProviderSession: "preempt-provider", WorktreeTree: "tree"})
	next, _, err := engine.ResumeInFlightWithNotification(j, ds[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err = engine.CompleteBranch(next, ds[1].BranchID, ds[1].ID, branchResult(ds[1], `{"text":"ok"}`, ""))
	if err != nil {
		t.Fatal("preemption invalidated sibling", err)
	}
	_, retry, _, err := engine.PrepareBranch(next, ds[0].BranchID)
	if err != nil || retry == nil || retry.ResumeCheckpoint == nil || retry.ResumeCheckpoint.ProviderSession != "preempt-provider" {
		t.Fatal(retry, err)
	}
}
func TestResumeFailedSharedJoinReexecutesOnlyFailedBranch(t *testing.T) {
	doc := branchRecoveryDoc(2)
	work := doc.Stages["work"]
	work.TurnBudget = &workflow.TurnBudgetSpec{WallSeconds: 1, OnExhaustion: "join"}
	doc.Stages["work"] = work
	j, ds := newBranchRecoveryJob(t, doc)
	var err error
	j, _, err = engine.CompleteBranch(j, ds[0].BranchID, ds[0].ID, branchResult(ds[0], "", engine.ClassTurnBudgetExhausted))
	if err != nil {
		t.Fatal(err)
	}
	j, _, err = engine.CompleteBranch(j, ds[1].BranchID, ds[1].ID, branchResult(ds[1], `{"text":"ok"}`, ""))
	if err != nil {
		t.Fatal(err)
	}
	for j.State != domain.Escalated {
		j, _, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
	}
	j, _, err = engine.ApplyControl(j, engine.Control{Method: "ResumeAtStage", Reason: "retry failed branch"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	j, retries, _, err := engine.PrepareParallelFanOut(j, engine.Options{})
	if err != nil || len(retries) != 1 || retries[0].BranchID != ds[0].BranchID {
		t.Fatal("passed sibling was rerun", j.State, len(retries), err)
	}
	j, _, err = engine.CompleteBranch(j, retries[0].BranchID, retries[0].ID, branchResult(retries[0], `{"text":"fixed"}`, ""))
	if err != nil || j.State != domain.Done {
		t.Fatal(j.State, j.Reason, err)
	}
}

func TestAbsentOptionalCommandBindingCountsEachEncounter(t *testing.T) {
	doc := scenarioLeafDoc()
	doc.Start = "run"
	doc.Signature = &workflow.Signature{Inputs: map[string]json.RawMessage{"profile": json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"default":{}}`)}}
	doc.Stages = map[string]workflow.Stage{"run": {Primitive: workflow.HostCommand, Command: []string{"checker", "{{input.profile.name?}}"}, Weight: 1, TimeoutSeconds: 1, Output: "Note", Failure: "repair"}, "repair": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note", Inputs: []workflow.Input{{Name: "failure", Stage: "failure:run", Type: "failure", Version: 1}}}}
	doc.Edges = []workflow.Edge{{From: "run", To: "Done"}, {From: "run", To: "repair"}, {From: "repair", To: "Done"}}
	doc.TargetRequirements = &workflow.TargetRequirements{Commands: [][]string{doc.Stages["run"].Command}}
	j, err := engine.New("root", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	j, err = engine.BindTaskInputs(j, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	for visit := 1; visit <= 2; visit++ {
		for j.State != "repair" {
			var dispatch *engine.Dispatch
			j, dispatch, _, err = engine.Prepare(j)
			if err != nil || dispatch != nil {
				t.Fatal("absent typed argument must fail before dispatch", dispatch, err)
			}
		}
		if j.Counts["run"] != visit || j.Sequence != 0 {
			t.Fatal("binding encounter not counted", j.Counts, j.Sequence)
		}
		found := false
		for i := len(j.History) - 1; i >= 0; i-- {
			event := j.History[i]
			if event.Failure != nil && event.Failure.Stage == "run" {
				found = event.Failure.Visit == visit && event.Failure.Reason == "binding"
				break
			}
		}
		if !found {
			t.Fatal("failure metadata has wrong binding visit", visit)
		}
		if visit == 1 {
			j, _, err = engine.ApplyControl(j, engine.Control{Method: "Escalate", Reason: "retry input"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			j, _, err = engine.ApplyControl(j, engine.Control{Method: "RestartWithInformation", Information: "retry encounter"}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestBudgetRouteToJoinFailsEveryEnclosingFan(t *testing.T) {
	doc := branchRecoveryDoc(2)
	doc.Start = "outer"
	inner := doc.Stages["fan"]
	delete(doc.Stages, "fan")
	doc.Stages["inner"] = inner
	work := doc.Stages["work"]
	work.TurnBudget = &workflow.TurnBudgetSpec{WallSeconds: 1, OnExhaustion: "join"}
	doc.Stages["work"] = work
	innerJoin := doc.Stages["join"]
	innerJoin.Rule = workflow.JoinAll
	innerJoin.Failure = "outerjoin"
	doc.Stages["join"] = innerJoin
	doc.Artifacts["OuterJoined"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"array","items":{"type":"object"}}`)}
	doc.Stages["outer"] = workflow.Stage{Primitive: workflow.FanOut, Parallel: true, Join: "outerjoin", Branches: []workflow.FanBranch{{ID: "nested", Start: "inner", Output: "Joined"}, {ID: "side", Start: "side", Output: "Note"}}}
	doc.Stages["side"] = workflow.Stage{Primitive: workflow.AgentTurn, Role: "author", Output: "Note"}
	doc.Stages["outerjoin"] = workflow.Stage{Primitive: workflow.FanIn, Rule: workflow.JoinAll, Failure: "stop", Output: "OuterJoined"}
	doc.Edges = []workflow.Edge{{From: "outer", To: "inner"}, {From: "outer", To: "side"}, {From: "inner", To: "work"}, {From: "work", To: "join"}, {From: "join", To: "outerjoin"}, {From: "side", To: "outerjoin"}, {From: "outerjoin", To: "Done"}, {From: "outerjoin", To: "stop"}}
	j, ds := newBranchRecoveryJob(t, doc)
	j, more, _, prepareErr := engine.PrepareParallelFanOut(j, engine.Options{})
	if prepareErr != nil {
		t.Fatal(prepareErr)
	}
	ds = append(ds, more...)
	if len(ds) != 3 {
		t.Fatal(len(ds))
	}
	ordered := []*engine.Dispatch{}
	for _, d := range ds {
		if d.FanStage != "inner" {
			ordered = append(ordered, d)
		}
	}
	for index := 1; index >= 0; index-- {
		for _, d := range ds {
			if d.FanStage == "inner" && d.BranchIndex != nil && *d.BranchIndex == index {
				ordered = append(ordered, d)
			}
		}
	}
	var err error
	for _, d := range ordered {
		raw, class := `{"text":"ok"}`, ""
		if d.FanStage == "inner" && d.BranchIndex != nil && *d.BranchIndex == 0 {
			raw, class = "", engine.ClassTurnBudgetExhausted
		}
		j, _, err = engine.CompleteBranch(j, d.BranchID, d.ID, branchResult(d, raw, class))
		if err != nil {
			t.Fatal(err)
		}
	}
	if j.LastFan == nil || j.LastFan.Branches[0].Status != engine.BranchFailed {
		t.Fatal("outer branch passed after nested budget failure", j.State, j.Reason)
	}
	frame := j.LastFan.Branches[0].Frame
	if frame == nil || frame.LastFan == nil || frame.LastFan.Branches[0].Status != engine.BranchFailed || frame.LastFan.Branches[0].FailureReason != "budget" {
		t.Fatal("inner budget branch outcome was lost")
	}
}

func TestBlockedParallelFanCollectsEveryOutstandingBranchWithoutDispatching(t *testing.T) {
	j, ds := newBranchRecoveryJob(t, branchRecoveryDoc(3))
	if len(ds) != 3 {
		t.Fatal(len(ds))
	}
	blocked, _, err := engine.ApplyControl(j, engine.Control{Method: "Block", Reason: "operator pause"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A hold admits no new branch even when one of the existing turns finishes.
	for _, d := range ds {
		blocked, _, err = engine.CompleteBranch(blocked, d.BranchID, d.ID, branchResult(d, `{"text":"collected"}`, ""))
		if err != nil {
			t.Fatal("held branch completion rejected", d.BranchID, err)
		}
		if blocked.State != domain.Blocked || !blocked.Parked {
			t.Fatal("completion escaped operator hold", blocked.State, blocked.Reason)
		}
		next, dispatches, _, e := engine.PrepareParallelFanOut(blocked, engine.Options{})
		if e != nil || len(dispatches) != 0 || next.State != domain.Blocked {
			t.Fatal("held fan launched work", len(dispatches), e)
		}
		if e = engine.Validate(blocked); e != nil {
			t.Fatal("held checkpoint invalid", e)
		}
	}
	if blocked.InFlight != nil || blocked.OperatorStage != "" {
		t.Fatal("collected branches left unresolved hold dispatch")
	}
	count := 0
	for _, a := range blocked.Evidence {
		if a.Stage == "work" {
			count++
		}
	}
	if count != 3 {
		t.Fatal("held branch artifacts lost", count)
	}
}
