package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestEngineCheckCommandAndGateEnforcement(t *testing.T) {
	d := floorDoc()
	// Replace test stage with a HostCommand Check stage (format == "json", count_field == "checked_models").
	d.Stages["test"] = workflow.Stage{
		Primitive:      workflow.HostCommand,
		Output:         "report",
		Command:        []string{"model-check", "--json"},
		Weight:         1,
		TimeoutSeconds: 30,
		Check: &workflow.CheckSpec{
			Format:     "json",
			Output:     "stdout",
			CountField: "checked_models",
		},
	}
	checkGateExpr := bin("and",
		bin("eq", fieldOf("test", "exit_code"), lit(0)),
		bin("ge", fieldOf("test", "checked_models"), lit(1)),
	)
	d.Stages["gate"] = workflow.Stage{
		Primitive: workflow.Gate,
		TestStage: "test",
		Expr:      &checkGateExpr,
		Failure:   "stop",
	}

	// 1. Passing check command with checked_models = 5 reaches Done and satisfies protected VcsCommit.
	script := floorScript(2)
	script["test"] = []engine.ScriptStep{
		{Output: json.RawMessage(`{"exit_code":0,"checked_models":5}`)},
	}
	j := drive(t, newPrimitiveJob(t, d), engine.NewScripted(script), true)
	if j.State != domain.Done {
		t.Fatalf("expected check command with checked_models=5 to reach Done, got state=%s reason=%s", j.State, j.Reason)
	}

	// 2. Vacuous check command with checked_models = 0 fails the gate and routes to stop (Escalated).
	scriptZero := floorScript(2)
	scriptZero["test"] = []engine.ScriptStep{
		{Output: json.RawMessage(`{"exit_code":0,"checked_models":0}`)},
	}
	jZero := drive(t, newPrimitiveJob(t, d), engine.NewScripted(scriptZero), true)
	if jZero.State != domain.Escalated {
		t.Fatalf("expected check command with checked_models=0 to fail gate and escalate, got state=%s", jZero.State)
	}
}

func TestEngineFanOutBranchesFromEmptyAndDynamicExpansion(t *testing.T) {
	d := floorDoc()
	d.Start = "discover"
	d.Stages["discover"] = workflow.Stage{
		Primitive: workflow.AgentTurn,
		Role:      "coder",
		Output:    "report",
	}
	d.Stages["fan"] = workflow.Stage{
		Primitive: workflow.FanOut,
		Join:      "join",
		Parallel:  true,
		BranchesFrom: &workflow.BranchesFromSpec{
			Stage: "discover",
			Field: "items",
			Min:   0,
			Max:   16,
			Empty: "change",
		},
		BranchTemplate: &workflow.FanBranch{
			ID:         "template",
			Start:      "branch-impl",
			Pass:       approvedOnce("branch-review"),
			WriteScope: []string{"pkg/{{item.pkg}}/**"},
		},
	}
	d.Stages["branch-impl"] = workflow.Stage{
		Primitive:      workflow.AgentTurn,
		Role:           "coder",
		Output:         "report",
		ChangeProducer: true,
		Inputs:         []workflow.Input{{Name: "item", Type: "report", Stage: "fan", Version: 1, Optional: true}},
	}
	d.Stages["branch-review"] = workflow.Stage{
		Primitive: workflow.Review,
		Role:      "reviewer",
		Output:    "report",
	}
	d.Stages["join"] = workflow.Stage{
		Primitive: workflow.FanIn,
		Rule:      workflow.JoinAll,
		Failure:   "stop",
	}
	d.Edges = append(d.Edges,
		workflow.Edge{From: "discover", To: "fan"},
		workflow.Edge{From: "fan", To: "branch-impl"},
		workflow.Edge{From: "fan", To: "change"},
		workflow.Edge{From: "branch-impl", To: "branch-review"},
		workflow.Edge{From: "branch-review", To: "join"},
		workflow.Edge{From: "join", To: "test"},
		workflow.Edge{From: "join", To: "stop"},
	)

	// 1. Empty items array with Min == 0 routes directly to Empty ("change") and finishes Done.
	emptyScript := floorScript(2)
	emptyScript["discover"] = []engine.ScriptStep{
		{Output: json.RawMessage(`{"items":[]}`)},
	}
	jEmpty := drive(t, newPrimitiveJob(t, d), engine.NewScripted(emptyScript), true)
	if jEmpty.State != domain.Done {
		t.Fatalf("expected empty branches_from with Min==0 to route to change and reach Done, got state=%s reason=%s", jEmpty.State, jEmpty.Reason)
	}
	if !strings.Contains(kinds(jEmpty), "fan-out:visit 1: 0 branches -> change") {
		t.Fatalf("expected fan-out empty route event, got: %s", kinds(jEmpty))
	}

	// 2. Dynamic expansion of 3 disjoint items expands branch-0..branch-2 with parameterized WriteScope.
	dynScript := floorScript(5)
	dynScript["discover"] = []engine.ScriptStep{
		{Output: json.RawMessage(`{"items":[{"pkg":"alpha"},{"pkg":"beta"},{"pkg":"gamma"}]}`)},
	}
	dynScript["branch-impl"] = []engine.ScriptStep{
		{Output: json.RawMessage(`{"done":"alpha"}`)},
		{Output: json.RawMessage(`{"done":"beta"}`)},
		{Output: json.RawMessage(`{"done":"gamma"}`)},
	}
	dynScript["branch-review"] = []engine.ScriptStep{
		reviewStep(`{"approved":true}`),
		reviewStep(`{"approved":true}`),
		reviewStep(`{"approved":true}`),
	}
	jDyn := drive(t, newPrimitiveJob(t, d), engine.NewScripted(dynScript), true)
	if jDyn.State != domain.Done {
		t.Fatalf("expected 3-branch dynamic fan-out to reach Done, got state=%s reason=%s (%s)", jDyn.State, jDyn.Reason, jDyn.EscalationDetail)
	}
	if !strings.Contains(kinds(jDyn), "fan-out:visit 1: 3 branches joined at join") {
		t.Fatalf("expected 3 branches joined at join, got: %s", kinds(jDyn))
	}

	// 3. Overlap is an operator-visible width refusal before any branch dispatch.
	overlapJob := newPrimitiveJob(t, d)
	overlapExec := engine.NewScripted(map[string][]engine.ScriptStep{
		"discover": {{Output: json.RawMessage(`{"items":[{"pkg":"alpha"},{"pkg":"alpha"}]}`)}},
	})
	var stepErr error
	for step := 0; step < 10; step++ {
		var disp *engine.Dispatch
		overlapJob, disp, _, stepErr = engine.Prepare(overlapJob)
		if stepErr != nil {
			break
		}
		if disp != nil {
			res, err := overlapExec.Execute(context.Background(), *disp)
			if err != nil {
				t.Fatal(err)
			}
			overlapJob, _, stepErr = engine.Complete(overlapJob, disp.ID, res)
			if stepErr != nil {
				break
			}
		}
	}
	if stepErr != nil {
		t.Fatal("invalid width escaped as a Prepare infrastructure error", stepErr)
	}
	if overlapJob.State != domain.Escalated || !strings.Contains(overlapJob.Reason, "width") {
		t.Fatalf("overlap did not escalate with width: state=%s reason=%s", overlapJob.State, overlapJob.Reason)
	}
	for _, p := range overlapJob.Provenance {
		if p.Stage == "branch-impl" || p.Stage == "branch-review" {
			t.Fatal("invalid width produced branch evidence", p)
		}
	}
	if overlapJob.Attempts != 1 {
		t.Fatal("width refusal charged a branch attempt", overlapJob.Attempts)
	}
}

func TestEngineParallelFanOutPrepareAndCompleteBranch(t *testing.T) {
	d := fanDoc(workflow.JoinAll, 0)
	fanSt := d.Stages["fan"]
	fanSt.Parallel = true
	d.Stages["fan"] = fanSt

	x := engine.NewScripted(fanScript(`{"approved":true}`, `{"approved":true}`))
	j := newPrimitiveJob(t, d)
	// Advance until we reach the fan-out stage and its branches are created.
	for i := 0; i < 20 && j.Fan == nil; i++ {
		var disp *engine.Dispatch
		var err error
		j, disp, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		if disp != nil {
			res, err := x.Execute(context.Background(), *disp)
			if err != nil {
				t.Fatal(err)
			}
			j, _, err = engine.Complete(j, disp.ID, res)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if j.Fan == nil || !j.Fan.Parallel {
		t.Fatalf("expected active parallel fan-out, got: %+v", j.Fan)
	}

	// Prepare both branches ("correctness" and "security") concurrently before either completes!
	j, d0, _, err := engine.PrepareBranch(j, "correctness")
	if err != nil || d0 == nil {
		t.Fatalf("PrepareBranch correctness failed: d0=%v err=%v", d0, err)
	}
	j, d1, _, err := engine.PrepareBranch(j, "security")
	if err != nil || d1 == nil {
		t.Fatalf("PrepareBranch security failed: d1=%v err=%v", d1, err)
	}
	// Both branches are simultaneously running in j.Fan, and Validate(j) accepts it because Parallel == true.
	if err := engine.Validate(j); err != nil {
		t.Fatalf("expected Validate to accept 2 running branches when Parallel=true, got: %v", err)
	}

	r0, err := x.Execute(context.Background(), *d0)
	if err != nil {
		t.Fatal(err)
	}
	r1, err := x.Execute(context.Background(), *d1)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err = engine.CompleteBranch(j, "correctness", d0.ID, r0)
	if err != nil {
		t.Fatalf("CompleteBranch correctness failed: %v", err)
	}
	j, _, err = engine.CompleteBranch(j, "security", d1.ID, r1)
	if err != nil {
		t.Fatalf("CompleteBranch security failed: %v", err)
	}
	if j.Fan != nil {
		t.Fatalf("expected FanIn to settle after both parallel branches completed, got Fan=%+v", j.Fan)
	}
	j = drive(t, j, x, true)
	if j.State != domain.Done {
		t.Fatalf("expected parallel fan-out job to finish Done, got state=%s reason=%s", j.State, j.Reason)
	}
}

func TestEngineProtectedDocumentOnlyCommitFloor(t *testing.T) {
	// Advance a job to the "commit" stage and strip the test-run evidence so the
	// protected commit floor relies solely on DocumentGlobs + DocumentChecks.
	prepareAtCommitWithoutTestRun := func(t *testing.T) engine.Job {
		t.Helper()
		d := floorDoc()
		j := newPrimitiveJob(t, d)
		s := engine.NewScripted(floorScript(1))
		for j.State != "commit" {
			nextJ, disp, _, err := engine.Prepare(j)
			if err != nil {
				t.Fatalf("Prepare(%s): %v", j.State, err)
			}
			j = nextJ
			if disp != nil {
				if disp.Stage == "commit" {
					break
				}
				r, err := s.Execute(context.Background(), *disp)
				if err != nil {
					t.Fatalf("Execute(%s): %v", disp.Stage, err)
				}
				j, _, err = engine.Complete(j, disp.ID, r)
				if err != nil {
					t.Fatalf("Complete(%s): %v", disp.Stage, err)
				}
			}
		}
		var ev []workflow.Evidence
		for _, a := range j.Evidence {
			if a.Stage != "test" {
				ev = append(ev, a)
			}
		}
		j.Evidence = ev
		return j
	}

	// 1. Document-only task (DeclaredFiles matching DocumentGlobs) with positive DocumentChecks receipt dispatches commit cleanly.
	jDoc := prepareAtCommitWithoutTestRun(t)
	jDoc.DeclaredFiles = []string{"models/schema.yaml"}
	jDoc.DocumentGlobs = []string{"models/*.yaml", "docs/**"}
	jDoc.DocumentChecks = []engine.DocumentCheckReceipt{
		{ID: "validate-yaml", CountField: "valid_files", ExitCode: 0, Count: 2},
	}
	jDocNext, dispDoc, _, err := engine.Prepare(jDoc)
	if err != nil {
		t.Fatalf("Prepare commit for document-only task: %v", err)
	}
	if dispDoc == nil || dispDoc.Stage != "commit" || jDocNext.State == domain.Escalated {
		t.Fatalf("expected document-only task with passing DocumentChecks (count=2) to dispatch commit, got state=%s disp=%v detail=%s", jDocNext.State, dispDoc, jDocNext.EscalationDetail)
	}

	// 2. Document-only task with Count == 0 in DocumentChecks is refused by protected commit floor.
	jZero := prepareAtCommitWithoutTestRun(t)
	jZero.DeclaredFiles = []string{"models/schema.yaml"}
	jZero.DocumentGlobs = []string{"models/*.yaml"}
	jZero.DocumentChecks = []engine.DocumentCheckReceipt{
		{ID: "validate-yaml", CountField: "valid_files", ExitCode: 0, Count: 0},
	}
	jZeroOut, dispZero, _, err := engine.Prepare(jZero)
	if err != nil {
		t.Fatalf("Prepare commit for count=0 task: %v", err)
	}
	if dispZero != nil || jZeroOut.State != domain.Escalated {
		t.Fatalf("expected document-only task with count=0 to be refused at commit, got state=%s disp=%v", jZeroOut.State, dispZero)
	}

	// 3. Task touching a non-document file ("pkg/code.go") alongside "models/schema.yaml" is refused without a code test run.
	jMixed := prepareAtCommitWithoutTestRun(t)
	jMixed.DeclaredFiles = []string{"models/schema.yaml", "pkg/code.go"}
	jMixed.DocumentGlobs = []string{"models/*.yaml"}
	jMixed.DocumentChecks = []engine.DocumentCheckReceipt{
		{ID: "validate-yaml", CountField: "valid_files", ExitCode: 0, Count: 2},
	}
	jMixedOut, dispMixed, _, err := engine.Prepare(jMixed)
	if err != nil {
		t.Fatalf("Prepare commit for mixed task: %v", err)
	}
	if dispMixed != nil || jMixedOut.State != domain.Escalated {
		t.Fatalf("expected task touching non-document file to require code test run and escalate, got state=%s disp=%v", jMixedOut.State, dispMixed)
	}
}

func TestEngineParallelFanOutDifferentStageNamesAndBaseTree(t *testing.T) {
	// Two parallel branches at different stage names ("review" -> "review2" vs "secure"):
	// completing "review" while "secure" is still in flight must update j.State to match
	// otherInFlight.Stage ("secure") so engine.Validate(j) succeeds, and preserve BaseTree.
	d := fanDoc(workflow.JoinAll, 0)
	fanSt := d.Stages["fan"]
	fanSt.Parallel = true
	fanSt.Branches[0].Pass = approvedOnce("review2")
	d.Stages["fan"] = fanSt
	d.Stages["review2"] = d.Stages["review"]
	replaceEdge(&d, workflow.Edge{From: "review", To: "join"}, workflow.Edge{From: "review", To: "review2"}, workflow.Edge{From: "review2", To: "join"})
	if issues := workflow.Validate(d); len(issues) > 0 {
		t.Fatalf("workflow.Validate failed: %v", issues)
	}

	j := newPrimitiveJob(t, d)
	script := fanScript(`{"approved":true}`, `{"approved":true}`)
	script["review2"] = []engine.ScriptStep{reviewStep(`{"approved":true}`)}
	x := engine.NewScripted(script)
	for i := 0; i < 20 && j.Fan == nil && j.State != "fan"; i++ {
		var disp *engine.Dispatch
		var err error
		j, disp, _, err = engine.Prepare(j)
		if err != nil {
			t.Fatal(err)
		}
		if disp != nil {
			res, err := x.Execute(context.Background(), *disp)
			if err != nil {
				t.Fatal(err)
			}
			j, _, err = engine.Complete(j, disp.ID, res)
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	var disps []*engine.Dispatch
	var err error
	j, disps, _, err = engine.PrepareParallelFanOut(j, engine.Options{})
	if err != nil || len(disps) != 2 {
		t.Fatalf("expected PrepareParallelFanOut to return 2 dispatches, got %d (err=%v)", len(disps), err)
	}
	for _, br := range j.Fan.Branches {
		if br.BaseTree == "" {
			t.Fatalf("expected branch %s BaseTree to be populated, got empty", br.ID)
		}
	}

	// Complete "correctness" step 1 ("review") while "security" ("secure") is still in flight!
	r0Step1, err := x.Execute(context.Background(), *disps[0])
	if err != nil {
		t.Fatal(err)
	}
	j, _, err = engine.CompleteBranch(j, "correctness", disps[0].ID, r0Step1)
	if err != nil {
		t.Fatalf("CompleteBranch correctness step1 failed: %v", err)
	}
	if err := engine.Validate(j); err != nil {
		t.Fatalf("engine.Validate failed after CompleteBranch with sibling at different stage: %v (state=%s inFlight=%+v)", err, j.State, j.InFlight)
	}

	// Prepare "correctness"'s second stage ("review2") while "secure" is still in flight!
	j, moreDisps, _, err := engine.PrepareParallelFanOut(j, engine.Options{})
	if err != nil || len(moreDisps) != 1 || moreDisps[0].Stage != "review2" {
		t.Fatalf("expected PrepareParallelFanOut to prepare review2, got %v (err=%v)", moreDisps, err)
	}
	if err := engine.Validate(j); err != nil {
		t.Fatalf("engine.Validate failed after preparing review2: %v", err)
	}
}
