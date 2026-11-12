package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// The FAE-WF-094 parity corpus: recorded runs of the shipped default workflow
// (role outputs, host-command and effect results, crashes and route
// exhaustion; timings abstracted away) whose transition sequences and gate
// results are the expected outputs. TestDefaultWorkflowParityCorpus replays
// every file under testdata/parity on the current engine and default content.
// Re-record deliberately, then review the diff:
//
//	go test ./engine -run TestDefaultWorkflowParityCorpus -update-parity
var updateParity = flag.Bool("update-parity", false, "re-record testdata/parity from the scenario builders")

type parityReview struct {
	Stages        map[string]string `json:"stages"`
	RevisionStage string            `json:"revision_stage"`
	Remediation   string            `json:"remediation"`
	MaxRevisions  int               `json:"max_revisions"`
	MaxPolls      int               `json:"max_polls"`
	MergeRequired bool              `json:"merge_required"`
}
type parityRebase struct {
	Visit int    `json:"visit"`
	Tree  string `json:"tree"`
	Base  string `json:"base"`
}
type parityOperator struct {
	Stage  string `json:"stage"`
	Method string `json:"method"`
	Reason string `json:"reason"`
}
type parityTransition struct {
	From domain.State          `json:"from"`
	To   domain.State          `json:"to"`
	Kind domain.EscalationKind `json:"kind,omitempty"`
}
type parityGate struct {
	Stage  string              `json:"stage"`
	Result workflow.GateResult `json:"result"`
}
type parityOutcome struct {
	State       domain.State          `json:"state"`
	Kind        domain.EscalationKind `json:"kind,omitempty"`
	Detail      string                `json:"detail,omitempty"`
	Transitions []parityTransition    `json:"transitions"`
	Gates       []parityGate          `json:"gates"`
	Dispatches  []string              `json:"dispatches"`
}
type parityRun struct {
	Name        string             `json:"name"`
	Description string             `json:"description"`
	Document    string             `json:"document"`
	Execution   string             `json:"execution"`
	Disposition domain.Disposition `json:"disposition"`
	Review      *parityReview      `json:"review,omitempty"`
	// NoRoute makes every route ineligible with an elapsed exhaustion bound.
	NoRoute bool `json:"no_route,omitempty"`
	// Crashes counts unplanned restarts (safe scripted replay) per stage.
	Crashes map[string]int `json:"crashes,omitempty"`
	// Rebase gives a live reconciliation's new tree and base for one visit.
	Rebase map[string][]parityRebase `json:"rebase,omitempty"`
	// Diagnostics is the executor detail a typed outcome of the stage carries.
	Diagnostics map[string]string `json:"diagnostics,omitempty"`
	// Operator applies an audited task control when the task reaches a stage.
	Operator *parityOperator `json:"operator,omitempty"`
	// PaneStarved is the daemon's inventory proof that idle sessions saturate
	// the pane limit with nothing left to shed.
	PaneStarved bool `json:"pane_starved,omitempty"`
	// Script is visit-indexed per stage; the last step repeats for later visits.
	Script   map[string][]engine.ScriptStep `json:"script"`
	Expected parityOutcome                  `json:"expected"`
}

func parityDocument(t *testing.T, name string) workflow.Document {
	switch name {
	case "local":
		return defaults.Workflow()
	case "shipped":
		return defaults.Shipped()
	}
	t.Fatalf("unknown corpus document %q", name)
	return workflow.Document{}
}

// replayParity drives the durable engine exactly as the drive does, with the
// recorded results standing in for executors.
func replayParity(t *testing.T, run parityRun) parityOutcome {
	t.Helper()
	return replayParityWith(t, run, parityDocument(t, run.Document))
}
func replayParityWith(t *testing.T, run parityRun, doc workflow.Document) parityOutcome {
	t.Helper()
	newJob := engine.New
	if run.Execution == "live" {
		newJob = engine.NewLive
	}
	j, err := newJob("parity", 1, doc, "tree")
	if err != nil {
		t.Fatal(err)
	}
	j.Disposition = run.Disposition
	if r := run.Review; r != nil {
		j.ReviewStages, j.ReviewRevisionStage, j.ReviewRemediation = r.Stages, r.RevisionStage, r.Remediation
		j.ReviewMaxRevisions, j.ReviewMaxPolls, j.ReviewPollSeconds, j.ReviewMergeRequired = r.MaxRevisions, r.MaxPolls, 1, r.MergeRequired
	}
	script := map[string][]engine.ScriptStep{}
	for stage, steps := range run.Script {
		padded := append([]engine.ScriptStep(nil), steps...)
		for len(padded) < 64 {
			padded = append(padded, steps[len(steps)-1])
		}
		script[stage] = padded
	}
	executor := engine.NewScripted(script)
	options := engine.Options{}
	if run.NoRoute {
		options.SelectRoute = func([]workflow.Binding) (workflow.Binding, bool) { return workflow.Binding{}, false }
		options.RouteExhaustion = time.Nanosecond
	}
	crashes := map[string]int{}
	for k, v := range run.Crashes {
		crashes[k] = v
	}
	out := parityOutcome{Dispatches: []string{}}
	for i := 0; i < 4000 && !j.State.Terminal() && j.State != domain.Escalated; i++ {
		if j.Parked && j.Reason == "ReviewPending" {
			j.AdmissionRetryAfter = time.Time{} // timings are abstracted away
		}
		if run.PaneStarved && engine.PaneStarved(j) {
			if j, _, err = engine.EscalatePaneStarvation(j); err != nil {
				t.Fatal(run.Name, err)
			}
			continue
		}
		if j.Parked && j.Reason != "ReviewPending" && j.Reason != "all-routes-unavailable" {
			t.Fatalf("%s: parked without a scripted resolution: %s", run.Name, j.Reason)
		}
		if o := run.Operator; o != nil && string(j.State) == o.Stage && j.InFlight == nil {
			if j, _, err = engine.ApplyControl(j, engine.Control{Method: o.Method, Reason: o.Reason}, time.Time{}); err != nil {
				t.Fatal(run.Name, err)
			}
			continue
		}
		next, d, _, err := engine.PrepareWith(j, options)
		if err != nil {
			t.Fatal(run.Name, err)
		}
		j = next
		if d == nil {
			continue
		}
		for d != nil && crashes[d.Stage] > 0 {
			crashes[d.Stage]--
			if j, d, err = engine.Recover(j, false, true); err != nil {
				t.Fatal(run.Name, err)
			}
		}
		if d == nil || j.InFlight == nil {
			continue
		}
		out.Dispatches = append(out.Dispatches, d.Stage)
		r, err := executor.Execute(context.Background(), *d)
		if err != nil {
			t.Fatal(run.Name, err)
		}
		if run.Execution == "live" {
			r.ExecutionKind = "live"
		}
		if r.Class != "" && string(r.Output) == "null" {
			r.Output = nil // a typed outcome carries no role output
			r.Detail = run.Diagnostics[d.Stage]
		}
		for _, rb := range run.Rebase[d.Stage] {
			if rb.Visit == d.Visit {
				r.NewTree, r.NewBase = rb.Tree, rb.Base
			}
		}
		if j, _, err = engine.Complete(j, d.ID, r); err != nil {
			t.Fatal(run.Name, d.Stage, err)
		}
	}
	if !j.State.Terminal() && j.State != domain.Escalated {
		t.Fatalf("%s: run did not terminate: %s %s", run.Name, j.State, j.Reason)
	}
	if err = engine.Validate(j); err != nil {
		t.Fatal(run.Name, err)
	}
	out.State, out.Kind, out.Detail = j.State, j.EscalationKind, timestamp.ReplaceAllString(j.EscalationDetail, "TIME")
	out.Transitions, out.Gates = []parityTransition{}, []parityGate{}
	for _, e := range j.History {
		if e.Kind == "transition" {
			out.Transitions = append(out.Transitions, parityTransition{e.From, e.To, e.EscalationKind})
		}
		if e.Kind == "gate" && e.Gate != nil {
			out.Gates = append(out.Gates, parityGate{e.Stage, *e.Gate})
		}
	}
	return out
}

var timestamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`)

func canonical(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err = dec.Decode(&generic); err != nil {
		t.Fatal(err)
	}
	b, _ = json.MarshalIndent(generic, "", " ")
	return b
}

func TestDefaultWorkflowParityCorpus(t *testing.T) {
	dir := filepath.Join("testdata", "parity")
	if *updateParity {
		recordParity(t, dir)
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 20 {
		t.Fatalf("parity corpus has %d runs; FAE-WF-094 requires at least 20", len(files))
	}
	arms, kinds := map[string]bool{}, map[domain.EscalationKind]bool{}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var run parityRun
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&run); err != nil {
			t.Fatal(file, err)
		}
		t.Run(run.Name, func(t *testing.T) {
			got := replayParity(t, run)
			if a, b := canonical(t, got), canonical(t, run.Expected); !bytes.Equal(a, b) {
				t.Fatalf("%s diverged from its recorded run; if the change is intended, re-record with -update-parity and review the diff\n got %s\nwant %s", file, a, b)
			}
		})
		doc := parityDocument(t, run.Document)
		for i, tr := range run.Expected.Transitions {
			if s, ok := doc.Stages[string(tr.From)]; ok && s.Primitive == workflow.Branch && i+1 <= len(run.Expected.Transitions) {
				arms[string(tr.From)+">"+string(tr.To)] = true
			}
			if tr.Kind != "" {
				kinds[tr.Kind] = true
			}
		}
	}
	// Coverage floor: every Branch arm of the default and every escalation
	// kind its content and the engine can raise from recorded results.
	for id, s := range defaults.Shipped().Stages {
		if s.Primitive != workflow.Branch {
			continue
		}
		for _, next := range append([]string{s.Default}, func() (out []string) {
			for _, c := range s.Cases {
				out = append(out, c.Next)
			}
			return
		}()...) {
			if !arms[id+">"+next] {
				t.Errorf("no recorded run takes Branch arm %s>%s", id, next)
			}
		}
	}
	for _, kind := range parityKinds {
		if !kinds[kind] {
			t.Errorf("no recorded run escalates %s", kind)
		}
	}
	for _, kind := range domain.EscalationKinds {
		if _, excluded := unreachableKinds[kind]; excluded == slices.Contains(parityKinds, kind) {
			t.Errorf("Appendix B kind %s must be either recorded or documented unreachable", kind)
		}
	}
}

// The firing control: a one-arm change to the default's verdict routing, or a
// dropped recorded transition, makes the replay diverge from the corpus.
func TestParityCorpusDetectsDivergence(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "parity", "09-oracle-production-blocker.json"))
	if err != nil {
		t.Fatal(err)
	}
	var run parityRun
	if err = json.Unmarshal(raw, &run); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(canonical(t, replayParity(t, run)), canonical(t, run.Expected)) {
		t.Fatal("baseline replay diverged")
	}
	doc := defaults.Workflow()
	verdict := doc.Stages["verdict"]
	verdict.Cases = append([]workflow.Case(nil), verdict.Cases...)
	verdict.Cases[1].Next, verdict.Cases[2].Next = verdict.Cases[2].Next, verdict.Cases[1].Next
	doc.Stages["verdict"] = verdict
	if bytes.Equal(canonical(t, replayParityWith(t, run, doc)), canonical(t, run.Expected)) {
		t.Fatal("swapped verdict arms replayed identically")
	}
	run.Expected.Transitions = run.Expected.Transitions[1:]
	if bytes.Equal(canonical(t, replayParity(t, run)), canonical(t, run.Expected)) {
		t.Fatal("a dropped transition went unnoticed")
	}
}

// parityKinds are the Appendix B kinds the default workflow can raise; the
// corpus records a run for each. The operator verb (OperatorEscalate) and the
// daemon's pane-inventory proof (PaneCapStarvation) are recorded inputs.
var parityKinds = []domain.EscalationKind{domain.AttemptCap, domain.LoopCap, domain.SameGate, domain.TestRunCap, domain.ScopeWidenRejected, domain.NoRedViolation, domain.StaleBranchResidue, domain.DispatchRecoveryExhausted, domain.StallExhausted, domain.ProviderUnavailable, domain.PaneCapStarvation, domain.ResumeEvidence, domain.MergeOwnHunkLoss, domain.IntegrationConflict, domain.ExternalEffectRefused, domain.ReviewGateFailed, domain.DryRunComplete, domain.OperatorEscalate, domain.HumanDecisionRequired}

// unreachableKinds are the Appendix B kinds no run of the default can raise,
// with the reason. Together with parityKinds they are exactly Appendix B.
var unreachableKinds = map[domain.EscalationKind]string{
	domain.CoordinatorVeto: "Fae has no transition verifier (coordinator) role or stage, so nothing can veto a transition; the kind is defined but never raised",
	domain.FootprintExceedsHost: "The offline engine corpus has no host measurements; the daemon raises this kind before dispatch using its slot admission ledger, covered by native host-slot tests",
}

// ---- Scenario builders, used only to (re)record the corpus. ----

type scenario struct {
	run  parityRun
	want domain.State
	kind domain.EscalationKind
}

func raw(v any) json.RawMessage          { b, _ := json.Marshal(v); return b }
func parityStep(v any) engine.ScriptStep { return engine.ScriptStep{Output: raw(v)} }
func classStep(class string) engine.ScriptStep {
	return engine.ScriptStep{Class: class}
}
func testRun(exit, executed int, red bool) map[string]any {
	return map[string]any{"exit_code": exit, "executed_count": executed, "matched_count": 1, "expected_failure": exit != 0, "test_in_source": true, "red_passed": red, "fresh_build": true, "output_digest": fmt.Sprintf("exit-%d-%d", exit, executed)}
}
func happy() map[string][]engine.ScriptStep {
	return defaults.HappyScript()
}
func oracle(approved bool, findings ...map[string]any) engine.ScriptStep {
	if findings == nil {
		findings = []map[string]any{}
	}
	return parityStep(map[string]any{"approved": approved, "findings": findings, "rationale": "recorded"})
}
func finding(target string, valid bool) map[string]any {
	return map[string]any{"finding": "recorded " + target, "valid": valid, "severity": "blocking", "target": target, "rationale": "recorded"}
}
func receipt(fields map[string]any) engine.ScriptStep {
	out := map[string]any{"tool": "recorded", "argument_digest": "recorded", "external_id": "R1", "reconciliation_state": "reconciled", "idempotency_key": "recorded"}
	for k, v := range fields {
		out[k] = v
	}
	return parityStep(out)
}

var local = domain.Disposition{Mode: domain.Local}
var normalLive = domain.Disposition{Mode: domain.Normal, Live: true, TicketRoom: "room", Destination: "main", PushNamespace: "fae/"}

func shippedReview(maxRevisions, maxPolls int) *parityReview {
	stages := map[string]string{}
	for id, s := range defaults.Shipped().Stages {
		if s.Primitive == workflow.ExternalEffect && strings.HasPrefix(s.Subject, "review_") {
			stages[id] = s.Subject
		}
	}
	return &parityReview{Stages: stages, RevisionStage: "review-revision", Remediation: "review-remediation-loop", MaxRevisions: maxRevisions, MaxPolls: maxPolls, MergeRequired: true}
}
func normalScript() map[string][]engine.ScriptStep {
	s := happy()
	for _, id := range []string{"ticket-open", "ticket-in-review", "ticket-closed", "review-publish"} {
		s[id] = []engine.ScriptStep{receipt(nil)}
	}
	s["review-create"] = []engine.ScriptStep{receipt(map[string]any{"review_id": "R1", "review_revision": 1})}
	s["review-revision"] = []engine.ScriptStep{receipt(map[string]any{"review_id": "R1"})}
	s["review-read"] = []engine.ScriptStep{receipt(map[string]any{"review_id": "R1", "review_decision": "ready"})}
	s["review-merge"] = []engine.ScriptStep{receipt(map[string]any{"review_id": "R1", "review_merged": true})}
	return s
}

func scenarios() []scenario {
	var out []scenario
	add := func(name, description, document, execution string, d domain.Disposition, script map[string][]engine.ScriptStep, want domain.State, kind domain.EscalationKind, edit func(*parityRun)) {
		r := parityRun{Name: name, Description: description, Document: document, Execution: execution, Disposition: d, Script: script}
		if edit != nil {
			edit(&r)
		}
		out = append(out, scenario{r, want, kind})
	}
	with := func(stage string, steps ...engine.ScriptStep) map[string][]engine.ScriptStep {
		s := happy()
		s[stage] = steps
		return s
	}
	rejected := engine.ScriptStep{Reviews: []json.RawMessage{raw(map[string]any{"approved": false, "findings": []map[string]any{{"finding": "untestable", "severity": "blocking"}}}), happy()["spec-review"][0].Reviews[1]}}
	blocking := func(dimension string) json.RawMessage {
		return raw(map[string]any{"approved": false, "dimension": dimension, "findings": []map[string]any{{"finding": "edge case", "severity": "blocking", "target": "Production"}}})
	}
	reviewsBlocked := engine.ScriptStep{Reviews: append([]json.RawMessage{blocking("correctness")}, happy()["reviews"][0].Reviews[1:]...)}
	add("local-happy-path", "Every stage passes first time; verdict approves and merge is clean.", "local", "scripted", local, happy(), domain.Done, "", nil)
	add("intake-rejected", "Intake does not accept the item; the intake gate's failure arm escalates.", "local", "scripted", local, with("intake", parityStep(map[string]any{"accepted": false})), domain.Escalated, domain.HumanDecisionRequired, nil)
	add("spec-rejected-once", "One spec reviewer blocks; the spec loop reworks once.", "local", "scripted", local, with("spec-review", rejected, happy()["spec-review"][0]), domain.Done, "", nil)
	add("spec-loop-exhausted", "Spec review blocks every time; the spec loop's cap of 3 escalates LoopCap.", "local", "scripted", local, with("spec-review", rejected), domain.Escalated, domain.LoopCap, nil)
	add("red-wrong-reason-once", "The first RED run fails for the wrong reason; the RED gate reworks the test once.", "local", "scripted", local, with("run-red", parityStep(map[string]any{"exit_code": 1, "executed_count": 1, "matched_count": 1, "expected_failure": false, "test_in_source": true, "red_passed": false, "fresh_build": true, "output_digest": "wrong"}), happy()["run-red"][0]), domain.Done, "", nil)
	add("green-fails-once", "The first implementation fails its test; the green gate reworks it once.", "local", "scripted", local, with("run-tests", parityStep(testRun(1, 1, false)), happy()["run-tests"][0]), domain.Done, "", nil)
	add("format-debt-once", "The first implementation adds formatting debt; the format gate reworks it once.", "local", "scripted", local, with("format", parityStep(map[string]any{"new_debt": 1}), parityStep(map[string]any{"new_debt": 0})), domain.Done, "", nil)
	add("review-blocks-once", "The correctness reviewer blocks once; the reviews gate reworks the implementation.", "local", "scripted", local, with("reviews", reviewsBlocked, happy()["reviews"][0]), domain.Done, "", nil)
	add("oracle-production-blocker", "The oracle records one valid blocking production finding; the verdict's production arm reimplements.", "local", "scripted", local, with("oracle", oracle(false, finding("Production", true)), happy()["oracle"][0]), domain.Done, "", nil)
	add("oracle-red-test-blocker", "The oracle targets the RED test; the verdict's RED arm re-authors it.", "local", "scripted", local, with("oracle", oracle(false, finding("RedTest", true)), happy()["oracle"][0]), domain.Done, "", nil)
	add("oracle-unknown-target", "A valid blocker the oracle cannot place escalates through the verdict's unknown arm.", "local", "scripted", local, with("oracle", oracle(false, finding("unknown", true))), domain.Escalated, domain.HumanDecisionRequired, nil)
	add("oracle-withholds-approval", "No valid blocker but no approval: the verdict's default arm escalates.", "local", "scripted", local, with("oracle", oracle(false, finding("Production", false))), domain.Escalated, domain.HumanDecisionRequired, nil)
	add("qa-fails-once", "The whole-suite QA run fails once; the QA gate reworks the implementation.", "local", "scripted", local, with("qa", parityStep(testRun(1, 3, true)), happy()["qa"][0]), domain.Done, "", nil)
	add("merge-conflicted-once", "Reconciliation conflicts once; the merge route's conflict arm reimplements.", "local", "scripted", local, with("merge", parityStep(map[string]any{"result": "conflicted", "patch_identical": false, "paths": []string{"answer.go"}}), happy()["merge"][0]), domain.Done, "", nil)
	add("merge-own-hunk-loss", "Reconciliation loses the task's own hunk; the merge route's default arm escalates MergeOwnHunkLoss.", "local", "scripted", local, with("merge", parityStep(map[string]any{"result": "own-hunk-loss", "patch_identical": false})), domain.Escalated, domain.MergeOwnHunkLoss, nil)
	add("post-merge-qa-fails-once", "QA on the reconciled tree fails once; the post-merge gate reworks the implementation.", "local", "scripted", local, with("post-merge-qa", parityStep(testRun(1, 3, true)), happy()["post-merge-qa"][0]), domain.Done, "", nil)
	add("integration-not-landed", "Integration reports no landing; the integration gate's failure arm escalates IntegrationConflict.", "local", "scripted", local, with("integrate", parityStep(map[string]any{"landed": false, "sha": "scripted-commit"})), domain.Escalated, domain.IntegrationConflict, nil)
	add("format-same-reason", "Formatting debt repeats with the same reason; the fourth same-gate failure escalates SameGate.", "local", "scripted", local, with("format", parityStep(map[string]any{"new_debt": 2})), domain.Escalated, domain.SameGate, nil)
	runs := happy()
	runs["run-red"], runs["run-tests"] = nil, nil
	for i := 1; i <= 4; i++ {
		wrong := testRun(i, 1, false)
		wrong["expected_failure"] = false
		runs["run-red"] = append(runs["run-red"], parityStep(wrong))
	}
	runs["run-red"] = append(runs["run-red"], happy()["run-red"][0])
	for i := 1; i <= 4; i++ {
		runs["run-tests"] = append(runs["run-tests"], parityStep(testRun(i, i, false)))
	}
	add("test-runs-exhausted", "Four RED runs fail for different wrong reasons, then the green run fails differently each time until the per-task cap of 8 test runs escalates TestRunCap.", "local", "scripted", local, runs, domain.Escalated, domain.TestRunCap, nil)
	add("attempts-exhausted", "The oracle keeps finding new production blockers; rework exhausts the task attempt limit of 20.", "local", "scripted", local, with("oracle", oracle(false, finding("Production", true))), domain.Escalated, domain.AttemptCap, nil)
	add("routes-exhausted", "No route is ever eligible; the route-exhaustion bound escalates ProviderUnavailable.", "local", "scripted", local, happy(), domain.Escalated, domain.ProviderUnavailable, func(r *parityRun) { r.NoRoute = true })
	add("crash-recovery-exhausted", "The daemon dies three times during the spec turn; safe replay is bounded and escalates DispatchRecoveryExhausted.", "local", "scripted", local, happy(), domain.Escalated, domain.DispatchRecoveryExhausted, func(r *parityRun) { r.Crashes = map[string]int{"spec": 3} })
	add("external-effect-outside-normal", "Unprojected shipped content run outside Normal live: the engine refuses its first Normal-only effect.", "shipped", "scripted", local, happy(), domain.Escalated, domain.ExternalEffectRefused, nil)
	add("normal-live-delivery", "Normal live: ticket to review, draft review created, read ready, published, merged, ticket closed, Done.", "shipped", "live", normalLive, normalScript(), domain.Done, "", func(r *parityRun) { r.Review = shippedReview(5, 3) })
	remediated := normalScript()
	remediated["review-read"] = []engine.ScriptStep{receipt(map[string]any{"review_id": "R1", "review_decision": "remediation", "review_commit": "review-head"}), receipt(map[string]any{"review_id": "R1", "review_decision": "ready"})}
	add("normal-review-remediation", "A blocking automated comment routes to the implementer; the fix ships as revision 2 of the same review.", "shipped", "live", normalLive, remediated, domain.Done, "", func(r *parityRun) { r.Review = shippedReview(5, 3) })
	capped := normalScript()
	capped["review-read"] = []engine.ScriptStep{receipt(map[string]any{"review_id": "R1", "review_decision": "remediation", "review_commit": "review-head"})}
	add("normal-review-revision-cap", "Every read requires remediation; the same-review revision cap escalates ReviewGateFailed.", "shipped", "live", normalLive, capped, domain.Escalated, domain.ReviewGateFailed, func(r *parityRun) { r.Review = shippedReview(2, 3) })
	pending := normalScript()
	pending["review-read"] = []engine.ScriptStep{receipt(map[string]any{"review_id": "R1", "review_decision": "waiting"}), receipt(map[string]any{"review_id": "R1", "review_decision": "waiting"}), receipt(map[string]any{"review_id": "R1", "review_decision": "ready"})}
	add("normal-review-polls", "The review status is pending twice; bounded polling waits, then publishes and merges.", "shipped", "live", normalLive, pending, domain.Done, "", func(r *parityRun) { r.Review = shippedReview(5, 5) })
	blocked := normalScript()
	blocked["review-merge"] = []engine.ScriptStep{classStep("review-wait")}
	add("normal-merge-barrier", "The merge gate never clears; the observation budget escalates ReviewGateFailed and Done is never reached.", "shipped", "live", normalLive, blocked, domain.Escalated, domain.ReviewGateFailed, func(r *parityRun) { r.Review = shippedReview(5, 2) })
	dry := normalScript()
	for id, s := range defaults.Shipped().Stages {
		if s.Primitive == workflow.ExternalEffect {
			dry[id] = []engine.ScriptStep{receipt(map[string]any{"external_id": "", "reconciliation_state": "dry-run"})}
		}
	}
	add("normal-dry-run", "Normal dry run records every intended effect and ends DryRunComplete, never Done.", "shipped", "live", domain.Disposition{Mode: domain.Normal}, dry, domain.Escalated, domain.DryRunComplete, func(r *parityRun) { r.Review = shippedReview(5, 3); r.Review.MergeRequired = false })
	add("integration-target-moved-once", "The integration target moves once (known no-write); the retry loop reconciles onto the new tip; the patch-identical reconcile carries the approvals, re-runs QA only and the task lands (FAE-VCS-005a).", "local", "live", local, with("integrate", classStep("target-moved"), happy()["integrate"][0]), domain.Done, "", func(r *parityRun) {
		r.Rebase = map[string][]parityRebase{"merge": {{Visit: 2, Tree: "union", Base: "new-tip"}}}
	})
	add("integration-moved-stale-approvals", "The integration target moves but reconciliation reports no new base; the protected commit cannot revalidate its approvals and escalates ResumeEvidence.", "local", "live", local, with("integrate", classStep("target-moved"), happy()["integrate"][0]), domain.Escalated, domain.ResumeEvidence, nil)
	add("implement-stalls", "The implementer stalls past its retry bound; the run escalates StallExhausted.", "local", "live", local, with("implement", classStep("stall-retry")), domain.Escalated, domain.StallExhausted, nil)
	add("stale-branch-residue", "The worktree holds unexplained prior work at the RED turn; the run escalates StaleBranchResidue.", "local", "live", local, with("test-first", classStep("stale-residue")), domain.Escalated, domain.StaleBranchResidue, nil)
	add("operator-escalates", "The operator escalates the task while it waits at the oracle; the run escalates OperatorEscalate.", "local", "scripted", local, happy(), domain.Escalated, domain.OperatorEscalate, func(r *parityRun) {
		r.Operator = &parityOperator{Stage: "oracle", Method: "Escalate", Reason: "operator review of the reviewed change"}
	})
	add("pane-cap-starvation", "Host admission refuses the spec turn at the pane limit and the inventory proves only idle sessions hold it; the run escalates PaneCapStarvation.", "local", "live", local, with("spec", classStep("infrastructure-deferred")), domain.Escalated, domain.PaneCapStarvation, func(r *parityRun) {
		r.Diagnostics = map[string]string{"spec": "host admission: PaneLimit 60 held by idle sessions"}
		r.PaneStarved = true
	})
	widen := map[string]any{"changed_files": []string{"answer.go"}, "hunks": []string{"return 42"}, "proof": "needs the helper", "source": happyOutput("implement")["source"], "test_id": "TestBehavior", "request": map[string]any{"kind": "scope-widen", "files": []string{"helper.go"}, "reason": "the fix touches the helper"}}
	rejectedWiden := with("implement", parityStep(widen))
	rejectedWiden["request-judge"] = []engine.ScriptStep{parityStep(map[string]any{"approved": false, "rationale": "the helper is outside the spec's scope"})}
	add("scope-widen-rejected", "The implementer requests a scope widen and the independent judge rejects it; the run escalates ScopeWidenRejected.", "local", "scripted", local, rejectedWiden, domain.Escalated, domain.ScopeWidenRejected, nil)
	approvedWiden := with("implement", parityStep(widen), happy()["implement"][0])
	approvedWiden["request-judge"] = []engine.ScriptStep{parityStep(map[string]any{"approved": true, "rationale": "necessary"})}
	add("scope-widen-approved", "The judge approves a scope widen; the implementation resumes with the widened scope and the task lands.", "local", "scripted", local, approvedWiden, domain.Done, "", nil)
	rework := with("implement", parityStep(map[string]any{"changed_files": []string{"answer.go"}, "hunks": []string{"none"}, "proof": "the RED is wrong", "source": happyOutput("implement")["source"], "test_id": "TestBehavior", "request": map[string]any{"kind": "red-rework", "reason": "the RED asserts the wrong value"}}), happy()["implement"][0])
	rework["request-judge"] = []engine.ScriptStep{parityStep(map[string]any{"approved": true, "rationale": "the RED contradicts the spec"})}
	add("red-rework-approved", "The judge approves a RED rework; the RED is re-authored and frozen anew before implementation resumes.", "local", "scripted", local, rework, domain.Done, "", nil)
	noRed := happy()
	spec := happyOutput("spec")
	spec["no_red"] = true
	noRed["spec"] = []engine.ScriptStep{parityStep(spec)}
	noRed["run-suite"] = happy()["qa"]
	add("no-red-delivery", "The reviewed spec declares the task no-red; it skips the RED sequence, is proven by the whole suite and lands.", "local", "scripted", local, noRed, domain.Done, "", nil)
	violation := happy()
	violation["spec"], violation["run-suite"] = noRed["spec"], noRed["run-suite"]
	violation["commit"] = []engine.ScriptStep{classStep("no-red-violation")}
	add("no-red-violation", "A no-red task's diff contains production code; the target's diff classifier refuses the commit and the run escalates NoRedViolation.", "local", "live", local, violation, domain.Escalated, domain.NoRedViolation, func(r *parityRun) {
		r.Diagnostics = map[string]string{"commit": "production code without RED: answer.go"}
	})
	add("integration-conflict", "The integration target moves on both landing attempts; the patch-identical reconcile re-runs QA only (FAE-VCS-005a), the integration remediation route is then exhausted and the run escalates IntegrationConflict.", "local", "live", local, with("integrate", classStep("target-moved")), domain.Escalated, domain.IntegrationConflict, func(r *parityRun) {
		r.Rebase = map[string][]parityRebase{"merge": {{Visit: 2, Tree: "union", Base: "new-tip"}}}
	})
	refused := normalScript()
	refused["review-publish"] = []engine.ScriptStep{classStep("external-refused")}
	add("normal-effect-refused", "Normal live on the projected shipped content: the review adapter refuses the pinned publish call; the run escalates ExternalEffectRefused with the reported id and never merges.", "shipped", "live", normalLive, refused, domain.Escalated, domain.ExternalEffectRefused, func(r *parityRun) {
		r.Review = shippedReview(5, 3)
		r.Diagnostics = map[string]string{"review-publish": "external status 403 (reported id R1-refused)"}
	})
	return out
}

// happyOutput decodes one recorded happy-path role output for editing.
func happyOutput(stage string) map[string]any {
	var out map[string]any
	_ = json.Unmarshal(happy()[stage][0].Output, &out)
	return out
}

func recordParity(t *testing.T, dir string) {
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for i, s := range scenarios() {
		s.run.Expected = replayParity(t, s.run)
		if s.run.Expected.State != s.want || s.run.Expected.Kind != s.kind {
			t.Fatalf("scenario %s ended %s %s (%s), designed for %s %s", s.run.Name, s.run.Expected.State, s.run.Expected.Kind, s.run.Expected.Detail, s.want, s.kind)
		}
		name := fmt.Sprintf("%02d-%s.json", i+1, s.run.Name)
		names = append(names, name)
		if err := os.WriteFile(filepath.Join(dir, name), append(canonical(t, s.run), '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(names)
	t.Logf("recorded %d parity runs", len(names))
}
