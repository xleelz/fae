package daemon

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/proof"
	"github.com/xleelz/fae/workflow"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// This is an actual configured report adapter, not a claim that a theorem
// prover is installed. It exercises execution, normalization, gate decisions,
// durable command-linked receipts and restart behavior through the daemon.
func TestNativeFormalReportsGateAndPersistReceipts(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("Python unavailable for the local configured-report fixture")
	}
	cfg, ctx, _ := liveDaemonFixture(t)
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "model.spec"), []byte("fixture model: the adapter reports its verification obligations\n"), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, cfg.Workspace, "add", "model.spec")
	liveGit(t, cfg.Workspace, "commit", "-m", "formal report fixture source")
	cfg.Live.Base = liveGit(t, cfg.Workspace, "rev-parse", "HEAD")
	liveGit(t, cfg.Workspace, "branch", "-f", "fae/integration", cfg.Live.Base)
	cases := []struct {
		id                                                             string
		outcome                                                        proof.Outcome
		obligations, verified, failed, unknown, skipped, exit, timeout int
		passed, mapped, nonproof                                       bool
	}{
		{id: "configured", timeout: 10, passed: true, nonproof: true},
		{id: "proved", outcome: proof.Proved, obligations: 2, verified: 2, timeout: 10, passed: true},
		{id: "mapped", outcome: proof.Proved, obligations: 3, verified: 3, timeout: 10, passed: true, mapped: true},
		{id: "unknown", outcome: proof.Unknown, obligations: 1, unknown: 1, timeout: 10},
		{id: "zero", outcome: proof.Proved, timeout: 10},
		{id: "skipped", outcome: proof.Skipped, obligations: 1, skipped: 1, timeout: 10},
		{id: "disproved", outcome: proof.Disproved, obligations: 1, failed: 1, exit: 1, timeout: 10},
		{id: "timeout", outcome: proof.Proved, obligations: 1, verified: 1, timeout: 1},
	}
	commands := map[string][]string{}
	cfg.Live.Files = nil
	cfg.Live.TestFile = ""
	cfg.Live.TestID = ""
	cfg.Live.ExpectedFailure = ""
	cfg.Live.Tasks = nil
	cfg.Live.AllowedCommands = nil
	for _, c := range cases {
		report, _ := json.Marshal(map[string]any{"outcome": c.outcome, "obligations": c.obligations, "verified": c.verified, "failed": c.failed, "unknown": c.unknown, "skipped": c.skipped})
		if c.mapped {
			report, _ = json.Marshal(map[string]any{"summary": map[string]any{"status": "SUCCESS", "counts": map[string]any{"all": c.obligations, "discharged": c.verified, "counterexamples": c.failed, "inconclusive": c.unknown, "omitted": c.skipped}}, "solver": map[string]any{"version": "configured-fixture-v1"}})
		}
		program := "import sys,time; "
		if c.id == "timeout" {
			program += "time.sleep(3); "
		}
		program += fmt.Sprintf("print(%q); sys.exit(%d)", string(report), c.exit)
		if c.nonproof {
			program = "print('ordinary configured command executed')"
		}
		argv := []string{"python3", "-c", program}
		commands[c.id] = argv
		cfg.Live.AllowedCommands = append(cfg.Live.AllowedCommands, argv)
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
	markers := map[string][]byte{}
	for _, c := range cases {
		task := "formal-" + c.id
		argv := commands[c.id]
		pass := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "verify", Path: []string{"passed"}}, {Op: "literal", Value: true}}}
		doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: task, Start: "verify", Limits: workflow.Limits{Attempts: 4, TestRuns: 1, ScopeWidens: 1}, Roles: map[string]workflow.Role{}, Artifacts: map[string]workflow.ArtifactType{"ProofReceipt": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["passed","outcome","obligations","verified","failed","unknown","skipped","source_digest","output_digest","executable_digest"]}`)}}, Stages: map[string]workflow.Stage{"verify": {Primitive: workflow.HostCommand, Output: "ProofReceipt", Command: argv, Weight: 1, TimeoutSeconds: c.timeout, Formal: &proof.Config{Tool: "fixture-adapter", Parser: "normalized-json", Kind: proof.Deductive, Sources: []string{"model.spec"}}}, "gate": {Primitive: workflow.Gate, Name: "formal receipt passed", Expr: &pass, Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Reason: "proof was not established"}}, Edges: []workflow.Edge{{From: "verify", To: "gate"}, {From: "gate", To: "Done"}, {From: "gate", To: "stop"}}})
		if c.nonproof {
			stage := doc.Stages["verify"]
			stage.Formal = nil
			stage.CommandAdapter = "configured"
			stage.Output = "ExecutionReceipt"
			doc.Stages["verify"] = stage
			doc.Artifacts = map[string]workflow.ArtifactType{"ExecutionReceipt": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["executed","exit_code","timed_out","truncated","tree","output_digest"]}`)}}
			gate := doc.Stages["gate"]
			gate.Expr = &workflow.Expr{Op: "and", Args: []workflow.Expr{{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "verify", Path: []string{"executed"}}, {Op: "literal", Value: true}}}, {Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "verify", Path: []string{"exit_code"}}, {Op: "literal", Value: 0}}}}}
			gate.Name = "configured command executed successfully"
			doc.Stages["gate"] = gate
		}
		if c.mapped {
			doc.Stages["verify"].Formal.Tool = "future-configured-tool"
			doc.Stages["verify"].Formal.Parser = "json"
			doc.Stages["verify"].Formal.JSONFields = &proof.JSONFields{Outcome: "summary.status", Obligations: "summary.counts.all", Verified: "summary.counts.discharged", Failed: "summary.counts.counterexamples", Unknown: "summary.counts.inconclusive", Skipped: "summary.counts.omitted", ToolVersion: "solver.version", StatusMap: map[string]proof.Outcome{"SUCCESS": proof.Proved, "COUNTEREXAMPLE": proof.Disproved, "INCONCLUSIVE": proof.Unknown}}
		}
		governanceCall[controlplane.Revision](t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"})
		if _, err = call(t, d, "Admit", AdmitParams{ID: task, RunID: "formal-fixtures", WorkflowID: doc.ID, AuthorizingMessage: "message", DeclaredFiles: []string{"model.spec"}}, task); err != nil {
			t.Fatal(c.id, err)
		}
		row, e := awaitJob(t, d, task, 30*time.Second, func(_ controlplane.JobSnapshot, e envelope) bool {
			return e.Job.State.Terminal() || e.Job.State == "Escalated" || e.Job.Parked && !deferredPark(e)
		})
		expectedState := "Escalated"
		if c.passed {
			expectedState = "Done"
		}
		if string(e.Job.State) != expectedState {
			t.Fatalf("%s state %s expected%s reason%s detail%s history%+v", c.id, e.Job.State, expectedState, e.Job.Reason, e.Job.EscalationDetail, e.Job.History)
		}
		if e.Job.ExecutionKind != "live" || len(e.Job.GateRecords) != 1 || e.Job.GateRecords[0].Result.Passed != c.passed {
			t.Fatal(c.id, "proof did not reach genuine gate", e.Job.GateRecords)
		}
		versions[task] = row.Version
		found := false
		for _, a := range e.Job.Evidence {
			if a.Stage != "verify" {
				continue
			}
			found = true
			if c.nonproof {
				if a.Fields["executed"] != true || fmt.Sprint(a.Fields["exit_code"]) != "0" || a.Fields["tree"] == "" || a.Fields["output_digest"] == "" {
					t.Fatal("missing execution-only receipt", a.Fields)
				}
				for _, key := range []string{"passed", "proof", "outcome", "obligations", "verified"} {
					if _, exists := a.Fields[key]; exists {
						t.Fatal("ordinary execution mislabeled as proof", a.Fields)
					}
				}
				continue
			}
			if a.Fields["passed"] != c.passed || a.Fields["source_digest"] == "" || a.Fields["output_digest"] == "" || a.Fields["executable_digest"] == "" {
				t.Fatal(c.id, "missing receipt evidence", a.Fields)
			}
			if c.id == "timeout" && a.Fields["timed_out"] != true {
				t.Fatal("timeout not recorded", a.Fields)
			}
		}
		if !found {
			t.Fatal(c.id, "missing formal artifact")
		}
		runs, err := d.store.EngineRuns(ctx, task, 10)
		if err != nil || len(runs) != 1 {
			t.Fatal(c.id, runs, err)
		}
		key := "formal-proof/" + runs[0].RunID
		if c.nonproof {
			if raw, err := d.store.GetMarker(ctx, key); !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("ordinary command recorded formal marker", string(raw), err)
			}
			continue
		}
		raw, err := d.store.GetMarker(ctx, key)
		if err != nil {
			t.Fatal(key, err)
		}
		markers[key] = append([]byte(nil), raw...)
		var receipt struct {
			RunID      string       `json:"run_id"`
			Task       string       `json:"task_id"`
			Dispatch   string       `json:"dispatch_id"`
			Tree       string       `json:"tree"`
			Output     string       `json:"output_digest"`
			Executable string       `json:"executable_digest"`
			Proof      proof.Result `json:"proof"`
		}
		if err = json.Unmarshal(raw, &receipt); err != nil {
			t.Fatal(err)
		}
		if c.mapped && (receipt.Proof.Tool != "future-configured-tool" || receipt.Proof.Parser != "json" || receipt.Proof.Verified != 3 || receipt.Proof.ToolVersion != "configured-fixture-v1") {
			t.Fatal("configured mapping not retained", receipt.Proof)
		}
		if receipt.RunID != runs[0].RunID || receipt.Task != task || receipt.Dispatch != runs[0].DispatchID || receipt.Output != runs[0].OutputDigest || receipt.Tree != runs[0].Tree || receipt.Executable == "" || receipt.Proof.Passed != c.passed {
			t.Fatal(c.id, "receipt not bound to durable actual command", string(raw), runs[0])
		}
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for task, version := range versions {
		row, err := d.store.GetJob(ctx, task)
		if err != nil || row.Version != version {
			t.Fatal("formal stage replayed", task, row.Version, version, err)
		}
	}
	configuredRuns, err := d.store.EngineRuns(ctx, "formal-configured", 10)
	if err != nil || len(configuredRuns) != 1 {
		t.Fatal(configuredRuns, err)
	}
	if raw, err := d.store.GetMarker(ctx, "formal-proof/"+configuredRuns[0].RunID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("restart created proof for ordinary command", string(raw), err)
	}
	for key, before := range markers {
		after, err := d.store.GetMarker(ctx, key)
		if err != nil || string(before) != string(after) {
			t.Fatal("formal receipt changed on restart", key, err)
		}
	}
}
