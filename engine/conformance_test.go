package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-030 acceptance: a conformance suite per primitive on the scripted
// adapter. HumanDecision, FanOut/FanIn, SubWorkflow, Review, HostCommand,
// Loop and Escalate have their own suites; these cover AgentTurn, Gate,
// Branch, FreezeArtifact, CompareFrozen, VcsCommit, Merge, Integrate and
// ExternalEffect.

func conformanceJob(t *testing.T, d workflow.Document, live bool) engine.Job {
	t.Helper()
	newJob := engine.New
	if live {
		newJob = engine.NewLive
	}
	j, err := newJob("task", 1, d, "tree")
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// driveLive runs a live-labelled job on scripted results, adjusting each.
func driveLive(t *testing.T, j engine.Job, script map[string][]engine.ScriptStep, adjust func(*engine.Dispatch, *engine.Result)) engine.Job {
	t.Helper()
	return liveDrive(t, j, engine.NewScripted(script), func(d *engine.Dispatch, r *engine.Result) {
		if r.Class != "" && string(r.Output) == "null" {
			r.Output = nil
		}
		if adjust != nil {
			adjust(d, r)
		}
	})
}
func visited(j engine.Job, stage string) int {
	n := 0
	for _, e := range j.History {
		if e.Kind == "transition" && string(e.To) == stage {
			n++
		}
	}
	return n
}
func evidenceOf(j engine.Job, stage string) []workflow.Evidence {
	out := []workflow.Evidence{}
	for _, a := range j.Evidence {
		if a.Stage == stage {
			out = append(out, a)
		}
	}
	return out
}

func TestConformanceAgentTurn(t *testing.T) {
	d := floorDoc()
	change := d.Stages["change"]
	change.Phase = "implement"
	d.Stages["change"] = change
	j := drive(t, conformanceJob(t, d, false), engine.NewScripted(floorScript(1)), false)
	if j.State != domain.Done {
		t.Fatal(j.State, j.Reason)
	}
	// One artifact of the declared type, with provenance naming the turn.
	a := evidenceOf(j, "change")
	if len(a) != 1 || a[0].Type != "report" || a[0].Role != "coder" || !a[0].Provenance {
		t.Fatal("agent turn artifact", a)
	}
	found := false
	for _, p := range j.Provenance {
		if p.ArtifactID == a[0].ID && p.Stage == "change" && p.Primitive == workflow.AgentTurn && p.Role == "coder" && p.Session == a[0].Session && p.DispatchID != "" {
			found = true
		}
	}
	if !found {
		t.Fatal("agent turn provenance missing")
	}
	// The dispatch carries the stage configuration (phase label) and one
	// session of the declared role.
	j = conformanceJob(t, d, false)
	for d0 := (*engine.Dispatch)(nil); d0 == nil; {
		var err error
		if j, d0, _, err = engine.Prepare(j); err != nil {
			t.Fatal(err)
		}
		if d0 != nil && (d0.Config.Phase != "implement" || len(d0.Sessions) != 1 || d0.Sessions[0].Role != "coder") {
			t.Fatal("dispatch", d0.Config.Phase, d0.Sessions)
		}
	}
	// Output that is not the declared schema is a bounded malformed outcome.
	script := floorScript(1)
	script["change"] = []engine.ScriptStep{{Output: json.RawMessage(`[1]`)}, {Output: json.RawMessage(`[1]`)}, {Output: json.RawMessage(`[1]`)}, {Output: json.RawMessage(`[1]`)}}
	j = drive(t, conformanceJob(t, d, false), engine.NewScripted(script), false)
	// Repeated identical malformed output is bounded by SameGate (FAE-ROB-017):
	// the 4th identical failure escalates, naming the malformed reason.
	if j.State != domain.Escalated || j.EscalationKind != domain.SameGate || !strings.Contains(j.Reason, "failed 4 times") || !strings.Contains(j.Reason, "MalformedOutput") || len(evidenceOf(j, "change")) != 0 {
		t.Fatal("malformed agent output", j.State, j.Reason)
	}
}

func TestConformanceGate(t *testing.T) {
	d := floorDoc()
	g := d.Stages["gate"]
	g.Name = "tests green"
	d.Stages["gate"] = g
	j := drive(t, conformanceJob(t, d, false), engine.NewScripted(floorScript(1)), false)
	if j.State != domain.Done || !transitioned(j, "gate", "review") {
		t.Fatal("passing gate", j.State, j.Reason)
	}
	named := false
	for _, r := range j.GateRecords {
		named = named || r.Kind == workflow.RecordGate && r.Name == "tests green" && r.Result.Passed && len(r.Result.EvidenceIDs) == 1
	}
	if !named || engine.ReplayGates(j) != nil {
		t.Fatal("gate result not recorded with its name and evidence")
	}
	for name, run := range map[string]string{"false": `{"exit_code":1,"executed_count":1,"fresh_build":true}`, "unknown": `{"executed_count":1,"fresh_build":true}`} {
		script := floorScript(1)
		script["test"] = step(run)
		j = drive(t, conformanceJob(t, d, false), engine.NewScripted(script), false)
		if !transitioned(j, "gate", "stop") || transitioned(j, "gate", "review") {
			t.Fatal(name, "gate did not fail closed", j.State)
		}
		last := j.GateRecords[len(j.GateRecords)-1].Result
		if name == "unknown" && (last.Value != workflow.Unknown || !strings.Contains(string(raw(last.Leaves)), "missing")) {
			t.Fatal("unknown leaf not named", last)
		}
	}
}

func TestConformanceBranch(t *testing.T) {
	d := floorDoc()
	lane := bin("eq", fieldOf("change", "lane"), lit("fast"))
	d.Stages["route"] = workflow.Stage{Primitive: workflow.Branch, Default: "stop", Cases: []workflow.Case{{When: lane, Next: "review"}}}
	replaceEdge(&d, workflow.Edge{From: "gate", To: "review"}, workflow.Edge{From: "gate", To: "route"}, workflow.Edge{From: "route", To: "review"}, workflow.Edge{From: "route", To: "stop"})
	if issues := workflow.Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	for output, want := range map[string]string{`{"lane":"fast"}`: "review", `{"lane":"slow"}`: "stop", `{}`: string(domain.Escalated)} {
		script := floorScript(1)
		script["change"] = step(output)
		j := drive(t, conformanceJob(t, d, false), engine.NewScripted(script), false)
		if !transitioned(j, "route", want) {
			t.Fatal(output, "branch took", j.State, j.Reason)
		}
		if want == string(domain.Escalated) && !strings.Contains(j.Reason, "UnknownDecision") {
			t.Fatal("an unknown arm must escalate, not default", j.Reason)
		}
	}
}

// identityDoc freezes an author's declared test, then compares an editor's.
func identityDoc() workflow.Document {
	d := floorDoc()
	d.Artifacts["FrozenIdentity"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"identity":{"type":"string"}},"required":["identity"]}`)}
	d.Artifacts["Compare"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"identity":{"type":"string"},"unchanged":{"type":"boolean"}},"required":["identity","unchanged"]}`)}
	unchanged := bin("eq", fieldOf("compare", "unchanged"), lit(true))
	d.Stages["author"] = workflow.Stage{Primitive: workflow.AgentTurn, Role: "coder", Output: "report", ChangeProducer: true}
	d.Stages["freeze"] = workflow.Stage{Primitive: workflow.FreezeArtifact, Output: "FrozenIdentity", Subject: "author", Inputs: []workflow.Input{{Name: "subject", Type: "report", Stage: "author", Version: 1}}}
	d.Stages["compare"] = workflow.Stage{Primitive: workflow.CompareFrozen, Output: "Compare", Subject: "change", Inputs: []workflow.Input{{Name: "frozen", Type: "FrozenIdentity", Stage: "freeze", Version: 1}, {Name: "subject", Type: "report", Stage: "change", Version: 1}}}
	d.Stages["frozen-gate"] = workflow.Stage{Primitive: workflow.Gate, Expr: &unchanged, Failure: "stop"}
	d.Start = "author"
	replaceEdge(&d, workflow.Edge{From: "change", To: "test"}, workflow.Edge{From: "author", To: "freeze"}, workflow.Edge{From: "freeze", To: "change"}, workflow.Edge{From: "change", To: "compare"}, workflow.Edge{From: "compare", To: "frozen-gate"}, workflow.Edge{From: "frozen-gate", To: "test"}, workflow.Edge{From: "frozen-gate", To: "stop"})
	return d
}

const conformanceTest = "package p\nfunc TestX(t *T) { if answer() != 42 { t.Fatal(\"got\") } }\n"

func TestConformanceFreezeAndCompare(t *testing.T) {
	d := identityDoc()
	if issues := workflow.Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	report := func(source string) []engine.ScriptStep {
		raw, _ := json.Marshal(map[string]any{"source": source, "test_id": "TestX"})
		return []engine.ScriptStep{{Output: raw}}
	}
	reformatted := "package p\n\nfunc TestX(t *T) {\n\tif answer() != 42 {\n\t\tt.Fatal(\"got\")\n\t}\n}\n\nfunc TestAdded(t *T) {}\n"
	for name, c := range map[string]struct {
		source string
		same   bool
	}{"reformatted with an added test": {reformatted, true}, "assertion changed": {strings.Replace(conformanceTest, "42", "43", 1), false}} {
		script := floorScript(1)
		script["author"], script["change"], script["freeze"], script["compare"] = report(conformanceTest), report(c.source), step(`{}`), step(`{}`)
		j := drive(t, conformanceJob(t, d, false), engine.NewScripted(script), false)
		frozen, compared := evidenceOf(j, "freeze"), evidenceOf(j, "compare")
		if len(frozen) != 1 || len(compared) != 1 || frozen[0].Fields["identity"] == "" {
			t.Fatal(name, "identity artifacts", frozen, compared)
		}
		if compared[0].Fields["unchanged"] != c.same || (j.State == domain.Done) != c.same {
			t.Fatal(name, "compare verdict", compared[0].Fields, j.State)
		}
	}
}

func TestConformanceVcsCommit(t *testing.T) {
	j := driveLive(t, conformanceJob(t, floorDoc(), true), floorScript(1), nil)
	commit := evidenceOf(j, "commit")
	if j.State != domain.Done || len(commit) != 1 || commit[0].Fields["sha"] != "scripted" || j.ExecutionHead != "scripted" {
		t.Fatal("commit artifact or head", j.State, j.Reason, commit, j.ExecutionHead)
	}
	script := floorScript(1)
	script["review"] = []engine.ScriptStep{{Reviews: []json.RawMessage{json.RawMessage(`{"approved":false}`)}}}
	j = drive(t, conformanceJob(t, floorDoc(), false), engine.NewScripted(script), false)
	assertKind(t, j, domain.ResumeEvidence, "ProtectedEvidence")
	if len(evidenceOf(j, "commit")) != 0 {
		t.Fatal("unreviewed tree committed")
	}
}

// mergeDoc reconciles before the commit; a changed base re-verifies.
func mergeDoc() workflow.Document {
	d := floorDoc()
	d.Stages["merge"] = workflow.Stage{Primitive: workflow.Merge, Output: "report", Reverify: "again"}
	d.Stages["again"] = workflow.Stage{Primitive: workflow.Loop, MaxIterations: 2}
	replaceEdge(&d, workflow.Edge{From: "review", To: "commit"}, workflow.Edge{From: "review", To: "merge"}, workflow.Edge{From: "merge", To: "commit"}, workflow.Edge{From: "merge", To: "again"}, workflow.Edge{From: "again", To: "test"})
	return d
}

func TestConformanceMerge(t *testing.T) {
	script := floorScript(2)
	script["merge"] = append(step(`{"result":"clean"}`), step(`{"result":"clean"}`)...)
	j := drive(t, conformanceJob(t, mergeDoc(), false), engine.NewScripted(script), false)
	if j.State != domain.Done || visited(j, "again") != 0 || len(evidenceOf(j, "merge")) != 1 {
		t.Fatal("clean reconcile", j.State, j.Reason)
	}
	// A changed integration base takes the bounded reverify route and the
	// protected commit then needs fresh tests and reviews.
	moved := 0
	j = driveLive(t, conformanceJob(t, mergeDoc(), true), script, func(d *engine.Dispatch, r *engine.Result) {
		if d.Stage == "merge" && moved == 0 {
			moved++
			r.NewBase = "new-tip"
		}
	})
	if j.State != domain.Done || visited(j, "again") != 1 || visited(j, "test") != 2 || j.Base != "new-tip" {
		t.Fatal("changed base did not re-verify", j.State, j.Reason, visited(j, "again"))
	}
	if _, _, err := engine.Complete(conformanceJob(t, mergeDoc(), false), "none", engine.Result{NewBase: "x"}); err == nil {
		t.Fatal("completion without a dispatch accepted")
	}
}

func integrateDoc() workflow.Document {
	d := floorDoc()
	d.Stages["integrate"] = workflow.Stage{Primitive: workflow.Integrate, Output: "report", Protected: true, Reverify: "retry"}
	d.Stages["retry"] = workflow.Stage{Primitive: workflow.Loop, MaxIterations: 1}
	replaceEdge(&d, workflow.Edge{From: "commit", To: "Done"}, workflow.Edge{From: "commit", To: "integrate"}, workflow.Edge{From: "integrate", To: "Done"}, workflow.Edge{From: "integrate", To: "retry"}, workflow.Edge{From: "retry", To: "test"})
	return d
}

func TestConformanceIntegrate(t *testing.T) {
	script := floorScript(3)
	script["commit"] = []engine.ScriptStep{{Output: json.RawMessage(`{"sha":"c1"}`)}, {Output: json.RawMessage(`{"sha":"c1"}`)}}
	script["integrate"] = []engine.ScriptStep{{Class: "target-moved"}, {Output: json.RawMessage(`{"landed":true,"sha":"c1"}`)}}
	j := driveLive(t, conformanceJob(t, integrateDoc(), true), script, nil)
	if j.State != domain.Done || visited(j, "retry") != 1 || len(evidenceOf(j, "integrate")) != 1 {
		t.Fatal("moved target retried and landed", j.State, j.Reason)
	}
	script["integrate"] = []engine.ScriptStep{{Class: "target-moved"}, {Class: "target-moved"}}
	j = driveLive(t, conformanceJob(t, integrateDoc(), true), script, nil)
	assertKind(t, j, domain.IntegrationConflict, "retry exhausted")
	// A scripted (untrusted) executor cannot claim a moved target.
	j = conformanceJob(t, integrateDoc(), false)
	script["integrate"] = []engine.ScriptStep{{Class: "target-moved"}}
	executor := engine.NewScripted(script)
	for i := 0; i < 50 && j.State != "integrate"; i++ {
		var d *engine.Dispatch
		var err error
		if j, d, _, err = engine.Prepare(j); err != nil {
			t.Fatal(err)
		}
		if d != nil {
			r, _ := executor.Execute(t.Context(), *d)
			if j, _, err = engine.Complete(j, d.ID, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	j, d, _, err := engine.Prepare(j)
	if err != nil || d == nil {
		t.Fatal("integrate did not dispatch", err)
	}
	r, _ := executor.Execute(t.Context(), *d)
	r.Output = nil
	if _, _, err = engine.Complete(j, d.ID, r); err == nil {
		t.Fatal("scripted moved target accepted")
	}
}

func effectDoc() workflow.Document {
	d := floorDoc()
	d.Artifacts["receipt"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","properties":{"external_id":{"type":"string"}},"required":["external_id"]}`)}
	r := d.Roles["coder"]
	r.Outputs = append(r.Outputs, "receipt")
	d.Roles["coder"] = r
	d.Stages["publish"] = workflow.Stage{Primitive: workflow.ExternalEffect, Role: "coder", NormalOnly: true, Subject: "review_publish", Pins: map[string]string{"review": "fixed"}, IdempotenceKey: "publish", Output: "receipt"}
	replaceEdge(&d, workflow.Edge{From: "commit", To: "Done"}, workflow.Edge{From: "commit", To: "publish"}, workflow.Edge{From: "publish", To: "Done"})
	return d
}

func TestConformanceExternalEffect(t *testing.T) {
	d := effectDoc()
	if issues := workflow.Validate(d); len(issues) != 0 {
		t.Fatal(issues)
	}
	bare := d
	bare.Stages = map[string]workflow.Stage{}
	for k, v := range d.Stages {
		bare.Stages[k] = v
	}
	s := bare.Stages["publish"]
	s.Pins, s.IdempotenceKey = nil, ""
	bare.Stages["publish"] = s
	if len(workflow.Validate(bare)) == 0 {
		t.Fatal("unpinned effect accepted")
	}
	script := floorScript(1)
	script["publish"] = step(`{"external_id":"R-1"}`)
	// Outside Normal live the engine refuses the effect.
	j := drive(t, conformanceJob(t, d, false), engine.NewScripted(script), false)
	assertKind(t, j, domain.ExternalEffectRefused, "UnsupportedExternalEffect")
	normal := func() engine.Job {
		j := conformanceJob(t, d, true)
		j.Disposition = domain.Disposition{Mode: domain.Normal, Live: true, Destination: "main", PushNamespace: "fae/"}
		return j
	}
	j = driveLive(t, normal(), script, nil)
	receipt := evidenceOf(j, "publish")
	if j.State != domain.Done || len(receipt) != 1 || receipt[0].Fields["external_id"] != "R-1" {
		t.Fatal("Normal live effect", j.State, j.Reason)
	}
	script["publish"] = []engine.ScriptStep{{Class: engine.ClassExternalRefused}}
	j = driveLive(t, normal(), script, func(d *engine.Dispatch, r *engine.Result) {
		if d.Stage == "publish" {
			r.Detail = "external status 403 (reported id R-9)"
		}
	})
	assertKind(t, j, domain.ExternalEffectRefused, "reported id R-9")
	if len(evidenceOf(j, "publish")) != 0 {
		t.Fatal("refused effect recorded a receipt")
	}
}
