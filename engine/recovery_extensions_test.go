package engine_test

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
	"time"
)

func nestedDoc() workflow.Document {
	d := dataWorkflow()
	d.Signature = nil
	d.Start = "outer"
	d.Stages = map[string]workflow.Stage{
		"outer":      {Primitive: workflow.FanOut, Parallel: true, Join: "outerjoin", Branches: []workflow.FanBranch{{ID: "deep", Start: "middle"}, {ID: "sibling", Start: "sibling"}}},
		"middle":     {Primitive: workflow.FanOut, Parallel: true, Join: "middlejoin", Branches: []workflow.FanBranch{{ID: "middle", Start: "inner"}, {ID: "other", Start: "middleleaf"}}},
		"inner":      {Primitive: workflow.FanOut, Parallel: true, Join: "innerjoin", Branches: []workflow.FanBranch{{ID: "inner", Start: "leaf"}, {ID: "other", Start: "innerleaf"}}},
		"middleleaf": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"},
		"innerleaf":  {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"},
		"leaf":       {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"},
		"sibling":    {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"},
		"innerjoin":  {Primitive: workflow.FanIn, Rule: workflow.JoinAll, Failure: "stop"},
		"middlejoin": {Primitive: workflow.FanIn, Rule: workflow.JoinAll, Failure: "stop"},
		"outerjoin":  {Primitive: workflow.FanIn, Rule: workflow.JoinAll, Failure: "stop"},
		"stop":       {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "fan failed"},
	}
	d.Edges = []workflow.Edge{{From: "outer", To: "middle"}, {From: "outer", To: "sibling"}, {From: "middle", To: "inner"}, {From: "inner", To: "leaf"}, {From: "leaf", To: "innerjoin"}, {From: "innerjoin", To: "middlejoin"}, {From: "innerjoin", To: "stop"}, {From: "middlejoin", To: "outerjoin"}, {From: "middlejoin", To: "stop"}, {From: "sibling", To: "outerjoin"}, {From: "outerjoin", To: "Done"}, {From: "outerjoin", To: "stop"}}
	d.Edges = append(d.Edges, workflow.Edge{From: "middle", To: "middleleaf"}, workflow.Edge{From: "middleleaf", To: "middlejoin"}, workflow.Edge{From: "inner", To: "innerleaf"}, workflow.Edge{From: "innerleaf", To: "innerjoin"})
	return d
}

func failureDoc() workflow.Document {
	d := dataWorkflow()
	d.Signature = nil
	d.Start = "primary"
	x := bin("eq", fieldOf("primary", "text"), lit("repaired"))
	d.Stages = map[string]workflow.Stage{
		"primary": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note", Failure: "repair"},
		"repair":  {Primitive: workflow.AgentTurn, Role: "author", Output: "Note", Inputs: []workflow.Input{{Name: "failure", Stage: "failure:primary", Type: "failure", Version: 1}}},
		"check":   {Primitive: workflow.Gate, Name: "replacement", Expr: &x, Failure: "stop"},
		"stop":    {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "failed"},
	}
	d.Edges = []workflow.Edge{{From: "primary", To: "check"}, {From: "primary", To: "repair"}, {From: "repair", To: "check"}, {From: "check", To: "Done"}, {From: "check", To: "stop"}}
	return d
}

func TestSchemaFailureRoutePinsReasonAndReplacementReplays(t *testing.T) {
	j := newPrimitiveJob(t, failureDoc())
	x := &recording{inner: engine.NewScripted(map[string][]engine.ScriptStep{"primary": append(step(`{"text":3}`), step(`{"text":4}`)...), "repair": step(`{"text":"repaired"}`)})}
	j = drive(t, j, x, true)
	if j.State != domain.Done || j.Counts["primary"] != 2 {
		t.Fatal(j.State, j.Reason, j.Counts)
	}
	var failure, prior bool
	for _, d := range x.dispatches {
		if d.Stage == "primary" && d.Visit == 2 {
			prior = len(d.PriorRejection) > 0
		}
		if d.Stage == "repair" {
			for _, a := range d.Inputs {
				failure = failure || a.Type == "failure" && a.Fields["reason"] == "schema"
			}
		}
	}
	if !failure || !prior {
		t.Fatal("missing failure reason or retry rejection", failure, prior)
	}
	if err := engine.ReplayGates(j); err != nil {
		t.Fatal(err)
	}
	d := failureDoc()
	s := d.Stages["primary"]
	s.Inputs = d.Stages["repair"].Inputs
	d.Stages["primary"] = s
	if issues := workflow.Validate(d); len(issues) == 0 {
		t.Fatal("failure evidence accepted outside handler")
	}
}

func TestNewRevisionSchemaRetryBoundWithoutHandler(t *testing.T) {
	for _, retries := range []int{0, 1} {
		doc := dataWorkflow()
		doc.Signature = nil
		doc.Start = "make"
		doc.Stages = map[string]workflow.Stage{"make": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"}}
		doc.Edges = []workflow.Edge{{From: "make", To: "Done"}}
		if retries == 0 {
			s := doc.Stages["make"]
			s.SchemaRetries = &retries
			doc.Stages["make"] = s
		}
		raw, _ := json.Marshal(doc)
		written, err := workflow.ParseWrite(raw)
		if err != nil {
			t.Fatal(err)
		}
		x := engine.NewScripted(map[string][]engine.ScriptStep{"make": append(step(`{"text":3}`), step(`{"text":4}`)...)})
		job := drive(t, newPrimitiveJob(t, written), x, true)
		if job.State != domain.Escalated || job.Counts["make"] != retries+1 || job.EscalationKind != domain.SameGate {
			t.Fatal(retries, job.State, job.Reason, job.Counts)
		}
	}
}

func TestResumeAtStagePreservesEvidenceAndRefusesFloor(t *testing.T) {
	d := dataWorkflow()
	d.Signature = nil
	d.Start = "make"
	d.Stages = map[string]workflow.Stage{"make": d.Stages["make"]}
	d.Edges = []workflow.Edge{{From: "make", To: "Done"}}
	j := newPrimitiveJob(t, d)
	j, dispatch, _, err := engine.Prepare(j)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch == nil {
		j, dispatch, _, err = engine.Prepare(j)
		if err != nil || dispatch == nil {
			t.Fatal(err, j.State, j.Reason)
		}
	}
	x := engine.NewScripted(map[string][]engine.ScriptStep{"make": step(`{"text":"done"}`)})
	r, err := x.Execute(context.Background(), *dispatch)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err = engine.Complete(j, dispatch.ID, r)
	if err != nil {
		t.Fatal(err)
	}
	// Operator escalation at a completed stage preserves its evidence snapshot.
	j.State = domain.Escalated
	j.EscalatedStage = "make"
	j.EscalationKind = domain.HumanDecisionRequired
	j.Failures["schema:make"] = 2
	j.Failures["schema:make-more"] = 7
	before, _ := json.Marshal(j.Evidence)
	next, _, err := engine.ApplyControl(j, engine.Control{Method: "ResumeAtStage", Reason: "retry stage"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if next.Failures["schema:make"] != 0 || next.Failures["schema:make-more"] != 7 {
		t.Fatal("resume reset unrelated stage budget", next.Failures)
	}
	after, _ := json.Marshal(next.Evidence)
	if string(before) != string(after) || next.State != "make" {
		t.Fatal("resume changed evidence or target")
	}
	j.EscalationKind = domain.NoRedViolation
	if _, _, err = engine.ApplyControl(j, engine.Control{Method: "ResumeAtStage", Reason: "retry"}, time.Now()); err == nil || !strings.Contains(err.Error(), "full restart") {
		t.Fatal(err)
	}
}

func TestResumeFailedJoinRetainsPassedBranchAndEarlierEvidence(t *testing.T) {
	d := fanDoc(workflow.JoinAll, 0)
	script := fanScript(`{"approved":true}`, `{"approved":false}`)
	script["secure"] = []engine.ScriptStep{reviewStep(`{"approved":false}`), reviewStep(`{"approved":true}`)}
	x := &recording{inner: engine.NewScripted(script)}
	j := drive(t, newPrimitiveJob(t, d), x, true)
	if j.State != domain.Escalated || j.LastFan == nil {
		t.Fatal(j.State, j.Reason)
	}
	before := len(j.Evidence)
	j, _, err := engine.ApplyControl(j, engine.Control{Method: "ResumeAtStage", Reason: "retry failed security review"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	j = drive(t, j, x, true)
	if j.State != domain.Done || j.Counts["review"] != 1 || j.Counts["secure"] != 2 || len(j.Evidence) <= before {
		t.Fatal(j.State, j.Reason, j.Counts)
	}
}

func TestJoinListPreservesBranchOrderAndRejectsAmbiguousOutput(t *testing.T) {
	d := dataWorkflow()
	d.Signature = nil
	d.Start = "fan"
	d.Artifacts["Notes"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"array","items":{"type":"object","required":["index","status","output"]}}`)}
	d.Stages = map[string]workflow.Stage{
		"fan": {Primitive: workflow.FanOut, Parallel: true, Join: "join", Branches: []workflow.FanBranch{{ID: "a", Start: "a"}, {ID: "b", Start: "b"}}},
		"a":   {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"}, "b": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note"},
		"join": {Primitive: workflow.FanIn, Rule: workflow.JoinSettle, Output: "Notes", Failure: "stop"},
		"post": {Primitive: workflow.AgentTurn, Role: "author", Output: "Note", Inputs: []workflow.Input{{Name: "all", Stage: "join", Type: "Notes", Version: 1}}},
		"stop": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired, Reason: "failed"},
	}
	d.Edges = []workflow.Edge{{From: "fan", To: "a"}, {From: "fan", To: "b"}, {From: "a", To: "join"}, {From: "b", To: "join"}, {From: "join", To: "post"}, {From: "join", To: "stop"}, {From: "post", To: "Done"}}
	j := newPrimitiveJob(t, d)
	j, _, _, err := engine.Prepare(j)
	if err != nil {
		t.Fatal(err)
	}
	j, ds, _, err := engine.PrepareParallelFanOut(j, engine.Options{})
	if err != nil || len(ds) != 2 {
		t.Fatal(err, len(ds))
	}
	x := engine.NewScripted(map[string][]engine.ScriptStep{"a": step(`{"text":"a"}`), "b": step(`{"text":"b"}`)})
	for n := len(ds) - 1; n >= 0; n-- {
		r, e := x.Execute(context.Background(), *ds[n])
		if e != nil {
			t.Fatal(e)
		}
		j, _, e = engine.CompleteBranch(j, ds[n].BranchID, ds[n].ID, r)
		if e != nil {
			t.Fatal(e)
		}
		j = roundTrip(t, j)
	}
	if err := engine.Validate(j); err != nil {
		t.Fatal(err)
	}
	tampered := roundTrip(t, j)
	for i := range tampered.Evidence {
		if tampered.Evidence[i].Stage == "join" {
			tampered.Evidence[i].Fields["error"] = "unhashed gate input"
		}
	}
	if err := engine.Validate(tampered); err == nil {
		t.Fatal("gate-visible unhashed list metadata accepted")
	}
	projected, ids, projectionErr := engine.ResolveProjection(j, workflow.Projection{Stage: "join"})
	if projectionErr != nil || len(ids) != 1 {
		t.Fatal("list projection", projected, ids, projectionErr)
	}
	if items, ok := projected.([]any); !ok || len(items) != 2 {
		t.Fatal("list became metadata wrapper", projected)
	}
	j, post, _, err := engine.Prepare(j)
	if err != nil || post == nil {
		t.Fatal(err, j.State, j.Reason)
	}
	var found bool
	for _, a := range post.Inputs {
		if a.Stage == "join" {
			raw, e := workflow.EvidenceJSON(a)
			if e != nil {
				t.Fatal(e)
			}
			var items []map[string]any
			if json.Unmarshal(raw, &items) != nil || len(items) != 2 || items[0]["output"].(map[string]any)["text"] != "a" || items[1]["output"].(map[string]any)["text"] != "b" {
				t.Fatal(string(raw))
			}
			found = true
		}
	}
	if !found {
		t.Fatal("post join turn lost ordered list")
	}
	d.Artifacts["Extra"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	r := d.Roles["author"]
	r.Outputs = append(r.Outputs, "Extra")
	d.Roles["author"] = r
	d.Stages["extra"] = workflow.Stage{Primitive: workflow.AgentTurn, Role: "author", Output: "Extra"}
	replaceEdge(&d, workflow.Edge{From: "a", To: "join"}, workflow.Edge{From: "a", To: "extra"}, workflow.Edge{From: "extra", To: "join"})
	if issues := workflow.Validate(d); len(issues) == 0 {
		t.Fatal("ambiguous branch output accepted")
	}
}

func TestThreeDeepFanDispatchesSurviveRestartAndIsolateSiblings(t *testing.T) {
	j := newPrimitiveJob(t, nestedDoc())
	x := engine.NewScripted(map[string][]engine.ScriptStep{"leaf": step(`{"text":"deep"}`), "sibling": step(`{"text":"sibling"}`), "innerleaf": step(`{"text":"inner"}`), "middleleaf": step(`{"text":"middle"}`)})
	seen := map[string]bool{}
	for n := 0; n < 30 && j.State != domain.Done; n++ {
		var ds []*engine.Dispatch
		var err error
		if j.Fan == nil {
			var d *engine.Dispatch
			j, d, _, err = engine.Prepare(j)
			if d != nil {
				ds = append(ds, d)
			}
		} else {
			j, ds, _, err = engine.PrepareParallelFanOut(j, engine.Options{})
		}
		if err != nil {
			t.Fatal(err)
		}
		j = roundTrip(t, j)
		for _, d := range ds {
			seen[d.BranchID] = true
			for _, a := range d.Inputs {
				if a.Stage == "sibling" || a.Stage == "leaf" {
					t.Fatal("sibling evidence leaked", d.BranchID, a.Stage)
				}
			}
			r, e := x.Execute(context.Background(), *d)
			if e != nil {
				t.Fatal(e)
			}
			j, _, e = engine.Complete(j, d.ID, r)
			if e != nil {
				t.Fatal(e)
			}
			j = roundTrip(t, j)
		}
		if j.State == domain.Escalated {
			t.Fatal(j.Reason, j.EscalationDetail)
		}
	}
	if j.State != domain.Done || !seen["deep/middle/inner"] || !seen["sibling"] {
		t.Fatalf("state %s dispatches %v", j.State, seen)
	}
	if err := engine.ReplayGates(j); err != nil {
		t.Fatal(err)
	}
}

func TestResumeNestedFailedJoinRetainsPassedGrandchildren(t *testing.T) {
	doc := nestedDoc()
	doc.Limits.Attempts = 40
	retries := 1
	inner := doc.Stages["innerleaf"]
	inner.SchemaRetries = &retries
	doc.Stages["innerleaf"] = inner
	script := map[string][]engine.ScriptStep{"leaf": step(`{"text":"keep"}`), "middleleaf": step(`{"text":"middle"}`), "sibling": step(`{"text":"sibling"}`), "innerleaf": append(append(step(`{"text":1}`), step(`{"text":2}`)...), step(`{"text":"fixed"}`)...)}
	script["middleleaf"] = append(script["middleleaf"], step(`{"text":"middle"}`)...)
	script["sibling"] = append(script["sibling"], step(`{"text":"sibling"}`)...)
	x := &recording{inner: engine.NewScripted(script)}
	job := drive(t, newPrimitiveJob(t, doc), x, true)
	if job.State != domain.Escalated || job.LastFan == nil && job.Fan == nil {
		t.Fatal(job.State, job.Reason)
	}
	before := len(job.Evidence)
	resumed, _, err := engine.ApplyControl(job, engine.Control{Method: "ResumeAtStage", Reason: "retry nested failure"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	resumed = roundTrip(t, resumed)
	resumed = drive(t, resumed, x, true)
	if resumed.State != domain.Done {
		t.Fatal(resumed.State, resumed.Reason, resumed.EscalationDetail)
	}
	counts := map[string]int{}
	for _, dispatch := range x.dispatches {
		counts[dispatch.Stage]++
	}
	if counts["leaf"] != 1 || counts["innerleaf"] != 3 || counts["middleleaf"] != 1 || counts["sibling"] != 1 || len(resumed.Evidence) <= before {
		t.Fatal("completed grandchildren reran", counts)
	}
}

func TestObjectInputNamedValueRemainsAnObjectInProjection(t *testing.T) {
	doc := dataWorkflow()
	doc.Signature.Inputs["profile"] = json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`)
	job := newPrimitiveJob(t, doc)
	var err error
	job, err = engine.BindTaskInputs(job, map[string]any{"profile": map[string]any{"value": "literal"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	value, _, err := engine.ResolveProjection(job, workflow.Projection{Input: "profile", Field: "value"})
	if err != nil || value != "literal" {
		t.Fatal("object field lost", value, err)
	}
	whole, _, err := engine.ResolveProjection(job, workflow.Projection{Input: "profile"})
	if err != nil {
		t.Fatal(err)
	}
	if object, ok := whole.(map[string]any); !ok || object["value"] != "literal" {
		t.Fatal("object unwrapped", whole)
	}
}

func TestNestedProjectedDispatchTamperingFailsClosed(t *testing.T) {
	job := newPrimitiveJob(t, nestedDoc())
	var dispatch *engine.Dispatch
	var err error
	job, dispatch, _, err = engine.Prepare(job)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch == nil {
		var dispatches []*engine.Dispatch
		job, dispatches, _, err = engine.PrepareParallelFanOut(job, engine.Options{})
		if err != nil || len(dispatches) == 0 {
			t.Fatal(err)
		}
		dispatch = dispatches[0]
	}
	if job.InFlight == nil {
		t.Fatal("missing projected dispatch")
	}
	if err = engine.Validate(roundTrip(t, job)); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*engine.Dispatch){func(d *engine.Dispatch) { d.Tree = "forged-tree" }, func(d *engine.Dispatch) { d.Config.Brief = "forged config" }} {
		raw, _ := json.Marshal(job)
		var tampered engine.Job
		_ = json.Unmarshal(raw, &tampered)
		change(tampered.InFlight)
		if err = engine.Validate(tampered); err == nil {
			t.Fatal("tampered root alias accepted")
		}
	}
}
