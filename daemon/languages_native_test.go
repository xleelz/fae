package daemon

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Real toolchains execute through daemon admission, live leases, HostCommand
// dispatch, evidence checkpoint and a final gate. No provider process is used.
func TestNativeLanguageHostCommandsPersistAndDoNotReplay(t *testing.T) {
	for _, tool := range []string{"cargo", "rustc", "node", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("native %s unavailable: %v", tool, err)
		}
	}
	if err := exec.Command("python3", "-m", "pytest", "--version").Run(); err != nil {
		t.Skip("pytest unavailable in selected Python")
	}
	cfg, ctx, _ := liveDaemonFixture(t)
	sources := map[string]string{
		"Cargo.toml":       "[package]\nname = \"fae_native_languages\"\nversion = \"0.1.0\"\nedition = \"2021\"\n[lib]\npath = \"rust_test.rs\"\n",
		"Cargo.lock":       "version = 3\n\n[[package]]\nname = \"fae_native_languages\"\nversion = \"0.1.0\"\n",
		"rust_test.rs":     "#[test]\nfn selected() { assert_eq!(42, 42); }\n#[test]\nfn unselected() { panic!(\"unselected Rust test ran\"); }\n",
		"test_python.py":   "def test_selected():\n    assert 42 == 42\n\ndef test_unselected():\n    assert False, 'unselected Python test ran'\n",
		"behavior.test.ts": "import { test } from 'node:test';\nimport assert from 'node:assert/strict';\ntest('SelectedBehavior', () => { const answer: number = 42; assert.equal(answer, 42); });\ntest('UnselectedBehavior', () => { throw new Error('unselected TypeScript test ran'); });\n",
	}
	for name, body := range sources {
		if err := os.WriteFile(filepath.Join(cfg.Workspace, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	liveGit(t, cfg.Workspace, "add", ".")
	liveGit(t, cfg.Workspace, "commit", "-m", "native language fixtures")
	cfg.Live.Base = liveGit(t, cfg.Workspace, "rev-parse", "HEAD")
	liveGit(t, cfg.Workspace, "branch", "-f", "fae/integration", cfg.Live.Base)
	cases := []struct {
		id, shape, file, test string
		argv, files           []string
	}{
		{"rust", "rust-unit", "rust_test.rs", "selected", []string{"cargo", "test", "--lib"}, []string{"Cargo.toml", "Cargo.lock", "rust_test.rs"}},
		{"python", "python-pytest", "test_python.py", "test_python.py::test_selected", []string{"python3", "-m", "pytest", "."}, []string{"test_python.py"}},
		{"typescript", "node-test", "behavior.test.ts", "SelectedBehavior", []string{"node", "--test", "behavior.test.ts"}, []string{"behavior.test.ts"}},
	}
	cfg.Live.Files = nil
	cfg.Live.TestFile = ""
	cfg.Live.TestID = ""
	cfg.Live.ExpectedFailure = ""
	cfg.Live.AllowedCommands = nil
	cfg.Live.Tasks = map[string]LiveTaskConfig{}
	for _, c := range cases {
		cfg.Live.AllowedCommands = append(cfg.Live.AllowedCommands, c.argv)
		cfg.Live.Tasks[c.id] = LiveTaskConfig{Files: c.files, TestFile: c.file, TestID: c.test, ExpectedFailure: "a declared failure that this passing fixture does not exhibit"}
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	versions := map[string]int64{}
	for _, c := range cases {
		equal := func(name string, value any) workflow.Expr {
			return workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "run", Path: []string{name}}, {Op: "literal", Value: value}}}
		}
		pass := workflow.Expr{Op: "and", Args: []workflow.Expr{equal("exit_code", 0), {Op: "ge", Args: []workflow.Expr{{Op: "field", Stage: "run", Path: []string{"executed_count"}}, {Op: "literal", Value: 1}}}, equal("matched_count", 1), equal("red_passed", true), equal("fresh_build", true), equal("test_in_source", true)}}
		checks := pass.Args
		pass = checks[0]
		for _, check := range checks[1:] {
			pass = workflow.Expr{Op: "and", Args: []workflow.Expr{pass, check}}
		}
		doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "native-" + c.id, Start: "run", Limits: workflow.Limits{Attempts: 4, TestRuns: 2, ScopeWidens: 1}, Roles: map[string]workflow.Role{}, Artifacts: map[string]workflow.ArtifactType{"EngineRun": {Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}}, Stages: map[string]workflow.Stage{"run": {Primitive: workflow.HostCommand, Output: "EngineRun", Command: c.argv, Weight: 1, TimeoutSeconds: 30, TestRun: true, TestShape: c.shape}, "gate": {Primitive: workflow.Gate, Expr: &pass, TestStage: "run", Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Reason: "language execution evidence failed"}}, Edges: []workflow.Edge{{From: "run", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "stop"}}})
		governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
		if _, err = call(t, d, "Admit", AdmitParams{ID: c.id, RunID: "languages", WorkflowID: doc.ID, AuthorizingMessage: "message"}, c.id); err != nil {
			t.Fatal(c.id, err)
		}
		row, e := awaitJob(t, d, c.id, 45*time.Second, func(_ controlplane.JobSnapshot, e envelope) bool {
			return e.Job.State.Terminal() || e.Job.State == "Escalated" || e.Job.Parked && !deferredPark(e)
		})
		if e.Job.State != "Done" {
			t.Fatalf("%s: state=%s reason=%s detail=%s history=%+v", c.id, e.Job.State, e.Job.Reason, e.Job.EscalationDetail, e.Job.History)
		}
		versions[c.id] = row.Version
		if e.Job.ExecutionKind != "live" || e.Job.TestRuns != 1 || len(e.Job.GateRecords) != 1 || !e.Job.GateRecords[0].Result.Passed {
			t.Fatalf("%s missing durable live gate: %+v", c.id, e.Job)
		}
		found := false
		for _, artifact := range e.Job.Evidence {
			if artifact.Stage == "run" {
				found = true
				if fmt.Sprint(artifact.Fields["executed_count"]) != "1" || fmt.Sprint(artifact.Fields["matched_count"]) != "1" || artifact.Fields["test_in_source"] != true || artifact.Fields["output_digest"] == "" {
					t.Fatal(c.id, artifact.Fields)
				}
			}
		}
		if !found {
			t.Fatal(c.id, "missing persisted EngineRun")
		}
	}
	lease, err := d.live.guard.host.Snapshot(ctx)
	if err != nil || len(lease.Holders) != 0 || len(lease.Waiting) != 0 {
		t.Fatal("host lease leak", lease, err)
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		row, err := d.store.GetJob(ctx, c.id)
		if err != nil {
			t.Fatal(err)
		}
		if row.Version != versions[c.id] || row.State != "Done" {
			t.Fatal("completed language run replayed", c.id, row.Version, versions[c.id], row.State)
		}
	}
}
