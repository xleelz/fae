package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
)

func normalSubjectTools(endpoint string) map[string]external.Tool {
	tools := map[string]external.Tool{}
	for _, subject := range defaults.NormalSubjects() {
		args := map[string]string{"destination": "main", "push_namespace": "fae/"}
		tools[subject] = external.Tool{Endpoint: endpoint, Server: "fixture", Name: subject, Role: "planner", Arguments: args}
	}
	return tools
}

func TestNormalContentProjectsLocalAndBindsNormal(t *testing.T) {
	shipped := defaults.Shipped()
	local, err := normalContent(shipped, "Local", nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(local)
	b, _ := json.Marshal(defaults.Workflow())
	if string(a) != string(b) {
		t.Fatal("Local task does not pin the Local projection")
	}
	tools := normalSubjectTools("http://127.0.0.1:1")
	bound, err := normalContent(shipped, "Normal", tools)
	if err != nil {
		t.Fatal(err)
	}
	for id, s := range bound.Stages {
		if s.Primitive != workflow.ExternalEffect {
			continue
		}
		if !targetEqual(s.Pins, tools[s.Subject].Arguments) || workflow.IsTargetPinned(s.Pins) {
			t.Fatal("stage not bound to its installed tool", id, s.Pins)
		}
	}
	if !workflow.IsTargetPinned(shipped.Stages["review-create"].Pins) {
		t.Fatal("binding mutated the stored document")
	}
	if issues := workflow.Validate(bound); len(issues) != 0 {
		t.Fatal(issues)
	}
	missing := map[string]external.Tool{}
	for k, v := range tools {
		missing[k] = v
	}
	delete(missing, defaults.ReviewMerge)
	if _, err = normalContent(shipped, "Normal", missing); err == nil || !strings.Contains(err.Error(), "review-merge") || !strings.Contains(err.Error(), defaults.ReviewMerge) {
		t.Fatal("missing subject accepted or unnamed", err)
	}
	wrong := tools[defaults.ReviewCreate]
	wrong.Role = "coder"
	missing[defaults.ReviewMerge], missing[defaults.ReviewCreate] = tools[defaults.ReviewMerge], wrong
	if _, err = normalContent(shipped, "Normal", missing); err == nil || !strings.Contains(err.Error(), "role") {
		t.Fatal("role grant drift accepted", err)
	}
}

func TestDefaultUpgradeDecision(t *testing.T) {
	if builtinGeneration("default", "default") != 1 || builtinGeneration("default", builtinName("default", 7)) != 7 || builtinGeneration("default", "operator name") != 0 || builtinGeneration("default", "default (builtin generation x)") != 0 {
		t.Fatal("generation names")
	}
	row := func(by, name, hash string, retired bool) controlplane.WorkflowListing {
		return controlplane.WorkflowListing{WorkflowID: "default", Revision: 4, Name: name, Hash: hash, CreatedBy: by, Retired: retired}
	}
	invalid := errors.New("brief key main")
	for _, c := range []struct {
		name    string
		latest  controlplane.WorkflowListing
		invalid error
		upgrade bool
		state   string
	}{
		{"same content", row("daemon", "default", "shipped", false), nil, false, "current"},
		{"legacy builtin fails validation", row("daemon", "default", "old", false), invalid, true, "upgraded"},
		{"legacy valid builtin is older", row("daemon", "default", "old", false), nil, true, "upgraded"},
		{"older generation", row("daemon", builtinName("default", 1), "old", false), nil, true, "upgraded"},
		{"same generation drift kept", row("daemon", builtinName("default", 2), "other", false), nil, false, "kept"},
		{"newer builtin kept", row("daemon", builtinName("default", 3), "newer", false), nil, false, "kept"},
		{"valid operator revision kept", row("operator", "default", "mine", false), nil, false, "operator-revision"},
		{"invalid operator revision replaced", row("operator", "default", "mine", false), invalid, true, "upgraded"},
		{"retired never revived", row("daemon", "default", "old", true), invalid, false, "retired"},
	} {
		upgrade, state, reason := defaultUpgrade(c.latest, "shipped", 2, c.invalid)
		if upgrade != c.upgrade || state != c.state || (state != "current" && reason == "") {
			t.Errorf("%s: upgrade %v state %s reason %q", c.name, upgrade, state, reason)
		}
	}
}

// FAE-WF-080/-093 upgrade path on a real plane: an instance holding an old
// builtin default that current validation rejects (brief key "main") gets the
// shipped default as an audited new revision; new tasks bind the new
// revision; a later start is current, and a valid operator revision is never
// overwritten. The old revision loads but cannot be run by this binary, so
// while a task is active on it the start is refused, naming the task and
// `serve --quarantine-unrunnable-revisions`, and writes no revision; under
// the flag the start upgrades the default, lists the old revision with the
// task (no longer the one admissions bind) and quarantines the task, which
// `fae migrate` moves to the upgraded revision, where it finishes (operator
// decision of 2026-10-30; before, the task was driven on the old revision).
func TestDefaultWorkflowUpgradeOnStoredPlane(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	old := defaults.Workflow()
	for name, role := range old.Roles {
		briefs := map[string]string{}
		for _, text := range role.Briefs {
			briefs["main"] = text
		}
		role.Briefs = briefs
		old.Roles[name] = role
	}
	raw, _ := json.Marshal(old)
	if validWorkflow(raw) == nil {
		t.Fatal("fixture old default validates")
	}
	legacy, err := d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "daemon", Action: "install builtin workflow", Outcome: "ok"}, "default", "default", raw, func([]byte) error { return nil })
	if err != nil || legacy.Number != 2 {
		t.Fatal(legacy, err)
	}
	// The older binary's engine admitted it; today's constructor would refuse.
	pinned, err := engine.New("old-task", int(legacy.Number), defaults.Workflow(), "tree")
	if err != nil {
		t.Fatal(err)
	}
	pinned.Document = old
	if pinned.WorkflowHash, err = workflow.Hash(old); err != nil || engine.Validate(pinned) != nil {
		t.Fatal(err)
	}
	pinned.Disposition = domain.Disposition{Mode: domain.Local}
	payload, _ := json.Marshal(envelope{Job: pinned, Admission: AdmitParams{ID: "old-task", RunID: cfg.RunID, Mode: "Local", Priority: 10, WorkflowID: "default", Revision: legacy.Number}})
	if _, err = d.store.AdmitJob(ctx, controlplane.Audit{Actor: "test", Action: "older binary admission", Outcome: "ok"}, "old-task", controlplane.JobSnapshot{ID: "old-task", WorkflowID: "default", WorkflowRevision: legacy.Number, TargetID: cfg.Workspace, TargetRevision: 1, State: string(pinned.State), Payload: payload}); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("an active task on the old default did not refuse the start")
	}
	if !strings.Contains(err.Error(), "workflow default revision 2 ("+legacy.Hash+")") || !strings.Contains(err.Error(), "active tasks [old-task] are pinned to it") || !strings.Contains(err.Error(), "--quarantine-unrunnable-revisions") {
		t.Fatalf("the refusal does not name the task and the flag: %v", err)
	}
	cfg.QuarantineUnrunnableRevisions = true
	d, err = Start(ctx, cfg)
	cfg.QuarantineUnrunnableRevisions = false
	if err != nil {
		t.Fatal(err)
	}
	status, _ := d.defaultWorkflow.Load().(DefaultWorkflowStatus)
	shipped, _ := json.Marshal(defaults.Shipped())
	hash, _, _ := controlplane.ContentHash(shipped)
	if status.State != "upgraded" || status.Revision != 3 || status.UpgradedFrom != 2 || status.Hash != hash || status.Generation != defaults.Generation || !strings.Contains(status.Reason, "fails current validation") {
		t.Fatalf("%+v", status)
	}
	revisions := d.startupReport.Load().(StartupChecks).Revisions
	if invalid := revisions.Invalid; len(invalid) != 1 || invalid[0].Revision != 2 || fmt.Sprint(invalid[0].Tasks) != "[old-task]" || invalid[0].Admits || !strings.Contains(invalid[0].Error, "brief key must be default") || fmt.Sprint(revisions.Quarantined) != "[old-task]" {
		t.Fatalf("the unrunnable stored revision is not listed with its quarantined task: %+v", revisions)
	}
	if log, _ := os.ReadFile(d.layout.Log); !strings.Contains(string(log), "stored workflow default revision 2 ("+legacy.Hash+") loads but cannot be run by this binary and active tasks are pinned to it, so the start quarantines them (--quarantine-unrunnable-revisions) (FAE-WF-093)") {
		t.Fatal("no daemon line names the unrunnable stored revision")
	}
	response, err := call(t, d, "Status", nil, "")
	if err != nil || !strings.Contains(string(response.Result), `"default_workflow":{"state":"upgraded","revision":3`) {
		t.Fatal("status omits the upgrade", string(response.Result), err)
	}
	// The refused start wrote no revision: the upgrade is the third, from 2.
	rows, err := d.store.ListWorkflows(ctx, "default")
	if err != nil || len(rows) != 3 || rows[2].CreatedBy != "daemon" || rows[2].Name != builtinName("default", defaults.Generation) || rows[2].Parent != 2 || rows[2].Hash != hash {
		t.Fatalf("%+v %v", rows, err)
	}
	var audits int
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect audit", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='daemon' AND action='upgrade builtin workflow' AND inputs LIKE '%"from_revision":2%'`).Scan(&audits)
	})
	if err != nil || audits != 1 {
		t.Fatal("upgrade not audited", audits, err)
	}
	// The quarantined task is not driven on the old revision; migrated to
	// the upgraded one, it finishes there.
	d.mu.Lock()
	_, quarantined := d.quarantined["old-task"]
	_, loaded := d.jobs["old-task"]
	d.mu.Unlock()
	if !quarantined || loaded {
		t.Fatalf("old task quarantined %t, loaded %t", quarantined, loaded)
	}
	if _, err = call(t, d, "Migrate", MigrateParams{ID: "old-task", Revision: 3, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal("migration out of quarantine:", err)
	}
	done := awaitState(t, d, "old-task", "Done")
	var env envelope
	if err = json.Unmarshal(done.Payload, &env); err != nil || done.WorkflowRevision != 3 || env.Job.Revision != 3 || env.Job.WorkflowHash == pinned.WorkflowHash {
		t.Fatal("the migrated task did not finish on the upgraded revision", done.WorkflowRevision, err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "new-task", Tree: "tree", AuthorizingMessage: "message"}, "new-task"); err != nil {
		t.Fatal(err)
	}
	fresh, err := d.store.GetJob(ctx, "new-task")
	if err != nil || fresh.WorkflowRevision != 3 {
		t.Fatal("new task did not bind the upgraded default", fresh.WorkflowRevision, err)
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if status, _ = d.defaultWorkflow.Load().(DefaultWorkflowStatus); status.State != "current" || status.Revision != 3 {
		t.Fatalf("restart re-upgraded %+v", status)
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: "default", Document: defaults.Workflow(), AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	if status, _ = d.defaultWorkflow.Load().(DefaultWorkflowStatus); status.State != "operator-revision" || status.Revision != 4 {
		t.Fatalf("operator revision overwritten %+v", status)
	}
	if rows, err = d.store.ListWorkflows(ctx, "default"); err != nil || len(rows) != 4 {
		t.Fatal(len(rows), err)
	}
}

// FAE-WF-080, FAE-WF-005: an instance first started on wave-2 code holds the
// generation-2 builtin default (defaults/testdata), which still validates, so
// only the generation orders it. The current daemon's start upgrades it to the
// shipped generation by the audited upgrade path; a task admitted on
// generation 2 and stopped mid-run keeps that revision and finishes on it.
func TestDefaultWorkflowGenerationTwoUpgradesOnStart(t *testing.T) {
	old, err := os.ReadFile(filepath.Join("..", "defaults", "testdata", "shipped-generation-2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = validWorkflow(old); err != nil {
		t.Fatal("generation-2 content no longer validates; only its generation should upgrade it", err)
	}
	cfg, ctx := fixture(t)
	// Generation-2 artifacts predate the spec's no_red and the security
	// review's guidance; the pinned task gets outputs its schemas accept.
	script := defaults.HappyScript()
	spec := map[string]any{"scope": "scripted rehearsal", "acceptance": []string{"answer is 42"}, "contracts": []string{"answer() int"}, "risks": []string{"none in fixture"}, "required_tests": []string{"TestBehavior"}, "declared_files": []string{"answer.go", "answer_test.go"}}
	raw := func(v any) json.RawMessage { b, _ := json.Marshal(v); return b }
	review := func(dimension string) json.RawMessage {
		return raw(map[string]any{"approved": true, "dimension": dimension, "findings": []string{}})
	}
	script["old-task/spec"] = []engine.ScriptStep{{Output: raw(spec)}}
	script["old-task/reviews"] = []engine.ScriptStep{{Reviews: []json.RawMessage{review("correctness"), review("quality"), review("security"), review("architecture")}}}
	gated := &drainExecutor{started: make(chan string, 4), release: make(chan struct{}), delegate: engine.NewScripted(script)}
	cfg.Executor = gated
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	gen2, err := d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "daemon", Action: "install builtin workflow", Outcome: "ok"}, "default", builtinName("default", 2), old, validWorkflow)
	if err != nil || gen2.Number != 2 {
		t.Fatal(gen2, err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "old-task", Tree: "tree", AuthorizingMessage: "message"}, "old-task"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gated.started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Stop mid-run between stages: the intake turn is collected on revision 2.
	if _, err = call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); err != nil {
		t.Fatal(err)
	}
	close(gated.release)
	guard := newHangGuard(time.Minute)
	for !d.status.Load().(statusSnapshot).Drained {
		if !guard.observe(false) {
			t.Fatal("drain did not collect the intake turn")
		}
		time.Sleep(10 * time.Millisecond)
	}
	before, err := d.store.GetJob(ctx, "old-task")
	if err != nil {
		t.Fatal(err)
	}
	var pinned envelope
	if err = json.Unmarshal(before.Payload, &pinned); err != nil || before.WorkflowRevision != 2 || pinned.Job.InFlight != nil || len(pinned.Job.Provenance) != 1 || pinned.Job.State.Terminal() {
		t.Fatalf("not pinned mid-run on generation 2: revision %d %+v %v", before.WorkflowRevision, pinned.Job.State, err)
	}
	if _, ok := pinned.Job.Document.Stages["request-judge"]; ok {
		t.Fatal("pinned task did not bind the generation-2 content")
	}
	stop(t, d)
	cfg.Executor = engine.NewScripted(script)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	shipped, _ := json.Marshal(defaults.Shipped())
	hash, _, _ := controlplane.ContentHash(shipped)
	status, _ := d.defaultWorkflow.Load().(DefaultWorkflowStatus)
	if defaults.Generation < 3 || status.State != "upgraded" || status.Revision != 3 || status.UpgradedFrom != 2 || status.Hash != hash || status.Generation != defaults.Generation || !strings.Contains(status.Reason, "(generation 2) predates generation") {
		t.Fatalf("generation-2 default not upgraded: %+v", status)
	}
	rows, err := d.store.ListWorkflows(ctx, "default")
	if err != nil || len(rows) != 3 || rows[2].CreatedBy != "daemon" || rows[2].Name != builtinName("default", defaults.Generation) || rows[2].Parent != 2 || rows[2].Hash != hash {
		t.Fatalf("%+v %v", rows, err)
	}
	var audits int
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect audit", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='daemon' AND action='upgrade builtin workflow' AND inputs LIKE '%"from_revision":2%' AND inputs LIKE ?`, fmt.Sprintf(`%%"generation":%d%%`, defaults.Generation)).Scan(&audits)
	})
	if err != nil || audits != 1 {
		t.Fatal("upgrade not audited", audits, err)
	}
	// The mid-run task finishes on its pinned generation-2 revision (FAE-WF-005).
	done := awaitState(t, d, "old-task", "Done")
	var env envelope
	if err = json.Unmarshal(done.Payload, &env); err != nil || done.WorkflowRevision != 2 || env.Job.Revision != 2 || env.Job.WorkflowHash != pinned.Job.WorkflowHash || env.Job.Provenance[0].DispatchID != pinned.Job.Provenance[0].DispatchID {
		t.Fatal("upgrade touched the pinned task", done.WorkflowRevision, err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "new-task", Tree: "tree", AuthorizingMessage: "message"}, "new-task"); err != nil {
		t.Fatal(err)
	}
	fresh, err := d.store.GetJob(ctx, "new-task")
	if err != nil || fresh.WorkflowRevision != 3 {
		t.Fatal("new task did not bind generation 3", fresh.WorkflowRevision, err)
	}
	if err = json.Unmarshal(fresh.Payload, &env); err != nil || env.Job.Document.Stages["request-judge"].Primitive == "" {
		t.Fatal("new task lacks the generation-3 request judge", err)
	}
}

func TestMayInstallShippedDefaultTruthTable(t *testing.T) {
	for bits := 0; bits < 32; bits++ {
		retired, differs, invalid, builtin, older := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0, bits&16 != 0
		if MayInstallShippedDefault(retired, differs, invalid, builtin, older) != (!retired && differs && (invalid || builtin && older)) {
			t.Fatal(bits)
		}
	}
	if MayInstallShippedDefault(true, true, true, true, true) || MayInstallShippedDefault(false, true, false, false, true) {
		t.Fatal("retired or valid operator revision replaced")
	}
}
