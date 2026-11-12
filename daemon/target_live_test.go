package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

// FAE-WF-035 acceptance: acceptance text full of shell metacharacters reaches
// the brokered process as literal arguments, and a vector outside the target
// allowlist is refused before anything runs.
func TestBoundCommandRunsLiteralAndOutsideAllowlistRefused(t *testing.T) {
	ctx := context.Background()
	root := reviewPhysical(t)
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "init")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, repo, "add", ".")
	liveGit(t, repo, "commit", "-m", "base")
	manager := &worktree.Manager{Repository: repo, Root: filepath.Join(root, "trees")}
	w, err := manager.Ensure(ctx, "run", "task", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	template := []string{"printf", "[%s]", "{{acceptance}}"}
	stage := workflow.Stage{Primitive: workflow.HostCommand, Command: template, Inputs: []workflow.Input{{Name: "spec", Type: "Spec", Stage: "spec", Version: 1}}, Bindings: map[string]workflow.ArgBinding{"acceptance": {Type: "text", Input: "spec", Field: "acceptance"}}}
	hostile := "x; touch pwned && $(touch pwned2) | tee pwned3 `touch pwned4` > pwned5"
	inputs := []workflow.Evidence{{Type: "Spec", Version: 1, Stage: "spec", Fields: map[string]any{"acceptance": hostile}}}
	argv, err := workflow.BindCommand(stage, inputs)
	if err != nil {
		t.Fatal(err)
	}
	d := engine.Dispatch{Config: stage, Inputs: inputs, Argv: argv}
	x := &liveExecutor{broker: &effects.Broker{Manager: manager}, config: LiveConfig{AllowedCommands: [][]string{template}}}
	target := targetV4(repo)
	target.AllowedCommands = append(target.AllowedCommands, []string{"printf", "[%s]", "{{text}}"})
	got, broker, err := x.authorizeCommand(ctx, d, &target)
	if err != nil || !slices.Equal(got, []string{"printf", "[%s]", hostile}) {
		t.Fatal(got, err)
	}
	run, err := broker.Run(ctx, w, effects.Command{Argv: got, Timeout: 10e9})
	if err != nil || run.ExitCode != 0 || run.Output != "["+hostile+"]" {
		t.Fatalf("%+v %v", run, err)
	}
	for _, name := range []string{"pwned", "pwned2", "pwned3", "pwned4", "pwned5"} {
		if _, e := os.Stat(filepath.Join(w.Path, name)); e == nil {
			t.Fatal("shell interpreted bound argument", name)
		}
	}
	if _, err = x.broker.Run(ctx, w, effects.Command{Argv: got, Timeout: 10e9}); err == nil {
		t.Fatal("instance broker ran a vector it never allowlisted")
	}
	strict := targetV4(repo)
	if _, _, err = x.authorizeCommand(ctx, d, &strict); err == nil || !strings.Contains(err.Error(), "target allowlist") {
		t.Fatal("vector outside target allowlist accepted", err)
	}
	d.Argv = []string{"printf", "[%s]", "forged"}
	if _, _, err = x.authorizeCommand(ctx, d, &target); err == nil {
		t.Fatal("forged vector accepted")
	}
	x.config.AllowedCommands = nil
	d.Argv = argv
	if _, _, err = x.authorizeCommand(ctx, d, &target); err == nil {
		t.Fatal("template outside instance allowlist accepted")
	}
}

// strictTarget pins required QA tests, a compile check and a diff classifier.
func strictTarget(cfg Config) TargetConfig {
	vet := []string{"go", "vet", "./..."}
	return TargetConfig{SchemaVersion: 4, Workspace: cfg.Workspace, IntegrationRef: cfg.Live.IntegrationRef, ProtectedBranches: []string{cfg.Live.IntegrationRef}, AllowedCommands: [][]string{defaults.TestCommand(), {"gofmt", "-l", "."}, vet}, RequiredTests: []RequiredTest{{Name: "suite", Argv: defaults.TestCommand(), TimeoutSeconds: 300}}, CompileCheck: &TargetCommand{Argv: vet, TimeoutSeconds: 300}, DiffClassifier: &DiffClassifier{Code: []string{"**/*.go"}, NotCode: []string{"**/*_test.go"}}}
}
func liveEnvelope(t *testing.T, d *Daemon, id string) envelope {
	t.Helper()
	snapshot, err := d.store.GetJob(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err = decodeEnvelope(snapshot.Payload, &e); err != nil {
		t.Fatal(err)
	}
	return e
}

// FAE-WF-085: QA runs the target's always-required tests and compile check
// and proves the tree; the commit floor holds the pinned revision through a
// restart even after a newer revision drops the requirement.
func TestLiveTargetRequiredChecksPinnedThroughRestart(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	cfg.Live.AllowedCommands = append(cfg.Live.AllowedCommands, []string{"go", "vet", "./..."})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "strict", Document: strictTarget(cfg), AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "required-one", TargetConfig: "strict", AuthorizingMessage: "message"}, "required-one"); err != nil {
		t.Fatal(err)
	}
	relaxed := strictTarget(cfg)
	relaxed.RequiredTests, relaxed.CompileCheck = nil, nil
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "strict", Document: relaxed, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "required-one", "Done")
	e := liveEnvelope(t, d, "required-one")
	if e.Admission.TargetRevision != 1 {
		t.Fatal("target binding moved", e.Admission.TargetRevision)
	}
	if _, err = d.store.GetMarker(ctx, requiredProofKey("required-one", e.Job.Tree, e.Admission)); err != nil {
		t.Fatal("pinned required checks never proved the committed tree", err)
	}
	folded := 0
	for _, a := range e.Job.Evidence {
		// The workflow's suite run and the required suite each executed TestBehavior.
		if a.Stage == "post-merge-qa" || a.Stage == "qa" {
			if a.Fields["executed_count"] != json.Number("2") {
				t.Fatal("required test not folded into QA evidence", a.Fields)
			}
			folded++
		}
	}
	if folded < 2 {
		t.Fatal("QA evidence missing", folded)
	}
	if tip := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef); tip == base {
		t.Fatal("integration never advanced")
	}
}

// FAE-WF-085: a workflow whose roles never run the required tests cannot
// commit; the refusal escalates before any commit object is written.
func TestLiveCommitRefusedWithoutRequiredTests(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	cfg.Live.AllowedCommands = append(cfg.Live.AllowedCommands, []string{"go", "vet", "./..."})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "strict", Document: strictTarget(cfg), AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	unlisted := strictTarget(cfg)
	build := []string{"go", "build", "./..."}
	unlisted.AllowedCommands = append(unlisted.AllowedCommands, build)
	unlisted.CompileCheck = &TargetCommand{Argv: build, TimeoutSeconds: 300}
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "unlisted", Document: unlisted, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "unlisted", TargetConfig: "unlisted", AuthorizingMessage: "message"}, "unlisted"); err == nil || !strings.Contains(err.Error(), "instance allowlist lacks") {
		t.Fatal("target check outside the instance allowlist admitted", err)
	}
	doc := defaults.Workflow()
	doc.ID = "no-qa"
	for _, id := range []string{"qa", "post-merge-qa"} {
		s := doc.Stages[id]
		s.TestShape = "go-unit"
		doc.Stages[id] = s
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "omitted", WorkflowID: "no-qa", TargetConfig: "strict", AuthorizingMessage: "message"}, "omitted"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "omitted", "Escalated")
	e := liveEnvelope(t, d, "omitted")
	if e.Job.EscalationKind != domain.HumanDecisionRequired || !strings.Contains(e.Job.Reason, "RequiredCheckFailed") || e.Job.LatestCommitHead() != "" {
		t.Fatal("unproven tree committed or wrong refusal", e.Job.EscalationKind, e.Job.Reason)
	}
	if tip := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef); tip != base {
		t.Fatal("refused task integrated")
	}
}

// noRedLiveWorkflow is a code-bearing class whose only implementation path
// requires the spec's no-red declaration; its stage names reuse the fixture
// bridge's scripted outputs.
func noRedLiveWorkflow() workflow.Document {
	base := defaults.Workflow()
	d := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "no-red", Start: "intake", Roles: map[string]workflow.Role{}, Artifacts: map[string]workflow.ArtifactType{}, Stages: map[string]workflow.Stage{}, Limits: base.Limits, NoRedStages: []string{"spec"}, TaskClasses: map[string]workflow.TaskClass{"code": {CodeBearing: true}}, DefaultClass: "code"})
	for _, name := range []string{"spec", "coder", "reviewer-correctness", "reviewer-quality", "reviewer-security", "reviewer-architecture"} {
		r := base.Roles[name]
		if name == "coder" {
			r.Briefs = map[string]string{"implement": r.Briefs["implement"]}
			r.Outputs = []string{"ImplementationReport"}
		}
		d.Roles[name] = r
	}
	for _, name := range []string{"Intake", "Spec", "ImplementationReport", "EngineRun", "Review", "Commit", "Integration"} {
		d.Artifacts[name] = base.Artifacts[name]
	}
	var schema map[string]any
	_ = json.Unmarshal(d.Artifacts["Spec"].Schema, &schema)
	schema["properties"].(map[string]any)["no_red"] = map[string]any{"type": "boolean"}
	raw, _ := json.Marshal(schema)
	d.Artifacts["Spec"] = workflow.ArtifactType{Version: 1, Schema: raw}
	for _, id := range []string{"intake", "intake-gate", "spec", "implement", "qa", "qa-gate", "reviews", "commit", "integration-gate", "integration-stop", "stop"} {
		d.Stages[id] = base.Stages[id]
	}
	spec := d.Stages["spec"]
	spec.Inputs = spec.Inputs[:1]
	d.Stages["spec"] = spec
	implement := d.Stages["implement"]
	implement.Inputs = []workflow.Input{{Name: "spec", Type: "Spec", Stage: "spec", Version: 1}}
	d.Stages["implement"] = implement
	qa := d.Stages["qa"]
	qa.Inputs = []workflow.Input{{Name: "implementation", Type: "ImplementationReport", Stage: "implement", Version: 1}}
	d.Stages["qa"] = qa
	gate := d.Stages["qa-gate"]
	gate.Failure = "stop"
	d.Stages["qa-gate"] = gate
	reviews := d.Stages["reviews"]
	reviews.Inputs = []workflow.Input{{Name: "spec", Type: "Spec", Stage: "spec", Version: 1}, {Name: "implementation", Type: "ImplementationReport", Stage: "implement", Version: 1}}
	d.Stages["reviews"] = reviews
	commit := d.Stages["commit"]
	commit.Inputs = []workflow.Input{{Name: "implementation", Type: "ImplementationReport", Stage: "implement", Version: 1}}
	d.Stages["commit"] = commit
	d.Stages["integrate"] = workflow.Stage{Primitive: workflow.Integrate, Protected: true, Output: "Integration", Inputs: []workflow.Input{{Name: "commit", Type: "Commit", Stage: "commit", Version: 1}}}
	noRed := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "spec", Path: []string{"no_red"}}, {Op: "literal", Value: true}}}
	d.Stages["route"] = workflow.Stage{Primitive: workflow.Branch, Default: "stop", Cases: []workflow.Case{{When: noRed, Next: "implement"}}}
	for _, e := range [][2]string{{"intake", "intake-gate"}, {"intake-gate", "spec"}, {"intake-gate", "stop"}, {"spec", "route"}, {"route", "implement"}, {"route", "stop"}, {"implement", "qa"}, {"qa", "qa-gate"}, {"qa-gate", "reviews"}, {"qa-gate", "stop"}, {"reviews", "commit"}, {"commit", "integrate"}, {"integrate", "integration-gate"}, {"integration-gate", "Done"}, {"integration-gate", "integration-stop"}} {
		d.Edges = append(d.Edges, workflow.Edge{From: e[0], To: e[1]})
	}
	return currentWorkflowFixture(d)
}

// FAE-WF-073 acceptance: a no-red task whose diff contains production code is
// refused at commit by the target's classifier; a diff the classifier calls
// non-code lands; no-red without a declared classifier is refused.
func TestLiveNoRedDiffClassifierAtCommit(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "base_test.go"), []byte("package sample\n\nimport \"testing\"\n\nfunc TestBase(t *testing.T) {}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, cfg.Workspace, "add", "base_test.go")
	liveGit(t, cfg.Workspace, "commit", "-m", "base test")
	cfg.Live.Base = liveGit(t, cfg.Workspace, "rev-parse", "HEAD")
	liveGit(t, cfg.Workspace, "branch", "-f", "fae/integration", cfg.Live.Base)
	cfg.Live.Environment["LIVE_DAEMON_NO_RED"] = "1"
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := noRedLiveWorkflow()
	if issues := workflow.Validate(doc); len(issues) != 0 {
		t.Fatal(issues)
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	target := func(id string, classifier *DiffClassifier) {
		c := TargetConfig{SchemaVersion: 4, Workspace: cfg.Workspace, IntegrationRef: cfg.Live.IntegrationRef, ProtectedBranches: []string{cfg.Live.IntegrationRef}, AllowedCommands: [][]string{defaults.TestCommand()}, DiffClassifier: classifier}
		if _, err := call(t, d, "TargetConfigPut", TargetPutParams{ID: id, Document: c, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	target("go-code", &DiffClassifier{Code: []string{"**/*.go"}, NotCode: []string{"**/*_test.go"}})
	target("pkg-code", &DiffClassifier{Code: []string{"pkg/**"}})
	target("unclassified", nil)
	for id, tc := range map[string]string{"production": "go-code", "non-code": "pkg-code", "unclassified": "unclassified"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, RunID: id, WorkflowID: "no-red", TargetConfig: tc, AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(id, err)
		}
	}
	awaitState(t, d, "non-code", "Done")
	for id, detail := range map[string]string{"production": "production code without RED: answer.go", "unclassified": "without a target diff classifier"} {
		awaitState(t, d, id, "Escalated")
		e := liveEnvelope(t, d, id)
		if e.Job.EscalationKind != domain.NoRedViolation || !strings.Contains(e.Job.Reason, detail) || e.Job.LatestCommitHead() != "" || !engine.NoRedDeclared(e.Job) {
			t.Fatal(id, "no-red code committed or wrong refusal", e.Job.EscalationKind, e.Job.Reason)
		}
	}
	if got := liveGit(t, cfg.Workspace, "show", cfg.Live.IntegrationRef+":answer.go"); !strings.Contains(got, "return 42") {
		t.Fatal("classified non-code no-red task did not land", got)
	}
}

// FAE-WF-077: a RED rework after implementation captures a new RED identity
// that must fail against the task's base revision (here it already passes on
// the implemented tree); both identities stay in the audit trail and the
// scratch base worktree is removed.
func TestLiveRedReworkVerifiedAgainstBase(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	cfg.Live.Environment["LIVE_DAEMON_RED_REWORK"] = "1"
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "rework", AuthorizingMessage: "message"}, "rework"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "rework", "Done")
	e := liveEnvelope(t, d, "rework")
	identities := map[any]bool{}
	for _, a := range e.Job.Evidence {
		if a.Stage == "freeze-red" {
			identities[a.Fields["identity"]] = true
		}
	}
	verified := 0
	for _, h := range e.Job.History {
		if h.Kind == "execution-diagnostic" && h.Stage == "run-red" && strings.Contains(h.Cause, "RED verified against the task base "+base) {
			verified++
		}
	}
	if len(identities) != 2 || verified != 1 {
		t.Fatal("replacement RED not captured and verified against base", identities, verified)
	}
	scratch := d.live.manager.Binding(e.Admission.RunID, "rework.red-base")
	if _, err = os.Stat(scratch.Path); err == nil || liveGit(t, cfg.Workspace, "branch", "--list", scratch.Branch) != "" {
		t.Fatal("scratch base worktree leaked")
	}
	if got := liveGit(t, cfg.Workspace, "show", cfg.Live.IntegrationRef+":answer_test.go"); !strings.Contains(got, "(reworked)") {
		t.Fatal("replacement RED did not land", got)
	}
}
