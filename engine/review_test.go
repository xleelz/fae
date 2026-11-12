package engine_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

func TestReviewNoFalseDoneFromNegativeIntegration(t *testing.T) {
	for _, fixture := range []struct{ name, stage, output string }{
		{"not landed", "integrate", `{"landed":false,"sha":"scripted-commit"}`},
		{"wrong commit landed", "integrate", `{"landed":true,"sha":"different-commit"}`},
		{"empty commit", "commit", `{"sha":"","files":["answer.go"]}`},
		{"intake refused", "intake", `{"accepted":false}`},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			script := defaults.HappyScript()
			script[fixture.stage][0].Output = json.RawMessage(fixture.output)
			for stage, steps := range script {
				for len(steps) < 10 {
					steps = append(steps, steps[0])
				}
				script[stage] = steps
			}
			job := drive(t, newJob(t), engine.NewScripted(script), true)
			if job.State == domain.Done {
				t.Fatalf("negative %s evidence reached Done", fixture.stage)
			}
		})
	}
}
func TestReviewRecoveryRetainsDispatchIdentityAndCounters(t *testing.T) {
	j := newJob(t)
	j, _, _, _ = engine.Prepare(j)
	j, dispatch, _, e := engine.Prepare(j)
	if e != nil {
		t.Fatal(e)
	}
	originalAttempts, originalSequence, originalVisits := j.Attempts, j.Sequence, j.Counts[dispatch.Stage]
	for i := 0; i < 4; i++ {
		j, replay, e := engine.Recover(j, true, true)
		if e != nil {
			t.Fatal(e)
		}
		if replay == nil || replay.ID != dispatch.ID || replay.Sessions[0].ID != dispatch.Sessions[0].ID {
			t.Fatal("graceful recovery changed identity")
		}
		if j.Attempts != originalAttempts || j.Sequence != originalSequence || j.Counts[dispatch.Stage] != originalVisits || j.Failures["dispatch-recovery"] != 0 {
			t.Fatal("graceful recovery charged budget")
		}
	}
	result, e := engine.NewScripted(defaults.HappyScript()).Execute(context.Background(), *dispatch)
	if e != nil {
		t.Fatal(e)
	}
	complete, _, e := engine.Complete(j, dispatch.ID, result)
	if e != nil {
		t.Fatal(e)
	}
	recovered, replay, e := engine.Recover(complete, false, true)
	if e != nil || replay != nil || recovered.State != complete.State || recovered.Failures["dispatch-recovery"] != 0 {
		t.Fatal("completed handoff redispatched", e)
	}
}
func TestReviewEvidenceTypeMustMatchProducer(t *testing.T) {
	j := drive(t, newJob(t), engine.NewScripted(defaults.HappyScript()), false)
	// Two EngineRun-shaped schema definitions must not make a change of producer
	// output type valid: type binding is part of workflow provenance.
	j.Document.Artifacts["ForeignRun"] = j.Document.Artifacts["EngineRun"]
	// Rehashing simulates a legitimate document containing both types, not changing
	// any output stage binding. The artifact is then mislabeled with the other type.
	hash, e := workflow.Hash(j.Document)
	if e != nil {
		t.Fatal(e)
	}
	j.WorkflowHash = hash
	for i := range j.Evidence {
		if j.Evidence[i].Type == "EngineRun" {
			j.Evidence[i].Type = "ForeignRun"
			break
		}
	}
	if e := engine.Validate(j); e == nil {
		t.Fatal("accepted artifact type incompatible with producer stage")
	}
}

func TestReviewPersistedDispatchMustMatchWorkflow(t *testing.T) {
	for _, which := range []string{"config", "identity", "sessions", "attempt", "negative stage count"} {
		t.Run(which, func(t *testing.T) {
			j := newJob(t)
			j, _, _, _ = engine.Prepare(j)
			j, d, _, e := engine.Prepare(j)
			if e != nil {
				t.Fatal(e)
			}
			switch which {
			case "config":
				j.InFlight.Config.Output = "Spec"
			case "identity":
				j.InFlight.ID = "foreign-token"
			case "sessions":
				j.InFlight.Sessions = nil
			case "attempt":
				j.InFlight.Attempt = 0
			case "negative stage count":
				j.Counts[d.Stage] = -1
			}
			if e := engine.Validate(j); e == nil {
				t.Fatalf("corrupt durable %s accepted", which)
			}
		})
	}
}

func TestReviewDimensionsCannotBeDuplicated(t *testing.T) {
	script := defaults.HappyScript()
	for i := range script["reviews"][0].Reviews {
		script["reviews"][0].Reviews[i] = json.RawMessage(`{"approved":true,"dimension":"correctness","findings":[]}`)
	}
	for stage, steps := range script {
		for len(steps) < 10 {
			steps = append(steps, steps[0])
		}
		script[stage] = steps
	}
	job := drive(t, newJob(t), engine.NewScripted(script), true)
	if job.State == domain.Done {
		t.Fatal("four reports of same review dimension replaced independent dimensions")
	}
}
func TestReviewArtifactLargeIntegerPreserved(t *testing.T) {
	script := defaults.HappyScript()
	script["run-tests"][0].Output = json.RawMessage(`{"exit_code":0,"executed_count":1,"matched_count":9007199254740993,"expected_failure":false,"test_in_source":true,"red_passed":true,"fresh_build":true,"output_digest":"fixture"}`)
	job := drive(t, newJob(t), engine.NewScripted(script), true)
	if job.State != domain.Done {
		t.Fatal("valid exact-number artifact failed", job.State, job.Reason)
	}
	found := false
	for _, evidence := range job.Evidence {
		if evidence.Stage == "run-tests" {
			if evidence.Fields["matched_count"] != json.Number("9007199254740993") {
				t.Fatal("integer rounded", evidence.Fields)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("missing numeric artifact")
	}
}

func TestReviewRemediationUsesLatestReviewerBatch(t *testing.T) {
	doc := defaults.Workflow()
	doc.Limits.Attempts = 100
	j, e := engine.New("task", 1, doc, "fixture-tree")
	if e != nil {
		t.Fatal(e)
	}
	script := defaults.HappyScript()
	good := script["spec-review"][0]
	bad := engine.ScriptStep{Reviews: []json.RawMessage{json.RawMessage(`{"approved":true,"findings":[]}`), json.RawMessage(`{"approved":false,"findings":[{"finding":"fix spec","severity":"blocking"}]}`)}}
	script["spec-review"] = []engine.ScriptStep{bad, good}
	for stage, steps := range script {
		if stage == "spec-review" {
			continue
		}
		for len(steps) < 10 {
			steps = append(steps, steps[0])
		}
		script[stage] = steps
	}
	result := drive(t, j, engine.NewScripted(script), true)
	if result.State != domain.Done || result.Counts["spec-review"] != 2 {
		t.Fatal("stale reviewer batch poisoned remediation", result.State, result.Reason, result.Counts)
	}
}
