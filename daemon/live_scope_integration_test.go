package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentruntime "github.com/xleelz/fae/runtime"
)

// TestLiveScopeBridgeFixture records each turn's task_scope brief block, then
// answers exactly as the live daemon bridge fixture does.
func TestLiveScopeBridgeFixture(t *testing.T) {
	if os.Getenv("LIVE_SCOPE_BRIDGE") != "1" {
		return
	}
	var c agentruntime.BridgeConfig
	raw, err := os.ReadFile(os.Args[len(os.Args)-1])
	if err != nil || json.Unmarshal(raw, &c) != nil {
		os.Exit(31)
	}
	var blocks []json.RawMessage
	if json.Unmarshal(c.Brief, &blocks) != nil {
		os.Exit(32)
	}
	record := map[string]any{"role": c.Role}
	for _, block := range blocks {
		var b struct{ Kind, Stage string }
		_ = json.Unmarshal(block, &b)
		if b.Kind == "stage" {
			record["stage"] = b.Stage
		}
		if b.Kind == "task_scope" {
			record["scope"] = block
		}
	}
	out, _ := json.Marshal(record)
	sum := sha256.Sum256([]byte(c.SessionID))
	if os.WriteFile(filepath.Join(os.Getenv("LIVE_SCOPE_RECORD"), hex.EncodeToString(sum[:8])+".json"), out, 0600) != nil {
		os.Exit(33)
	}
	TestLiveDaemonBridgeFixture(t)
}

// One instance with no pinned files takes a task's whole scope from the task:
// the Spec's declared files, the RedReport's RED identity, a commit message
// from its title. Every agent turn sees the scope known at that point.
func TestDaemonLiveDerivedScopeLifecycle(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	records := reviewPhysical(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Live.Command = []string{exe, "-test.run=^TestLiveScopeBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment["LIVE_SCOPE_BRIDGE"] = "1"
	cfg.Live.Environment["LIVE_SCOPE_RECORD"] = records
	cfg.Live.Files, cfg.Live.TestFile, cfg.Live.TestID, cfg.Live.ExpectedFailure, cfg.Live.CommitMessage = nil, "", "", "", ""
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal("derived scope configuration refused", err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "derived", Title: "Make answer return 42", Content: "answer() must return 42", AuthorizingMessage: "message"}, "derived"); err != nil {
		t.Fatal(err)
	}
	e := snapshotEnvelope(t, awaitState(t, d, "derived", "Done"))
	sha := ""
	for _, a := range e.Job.Evidence {
		if a.Type == "Commit" {
			sha, _ = a.Fields["sha"].(string)
		}
	}
	tip := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if sha == "" || tip == base || !strings.Contains(liveGit(t, cfg.Workspace, "show", tip+":answer.go"), "return 42") {
		t.Fatal("derived task did not land", sha, tip)
	}
	if msg := liveGit(t, cfg.Workspace, "log", "-1", "--format=%B", sha); !strings.HasPrefix(msg, "Make answer return 42\n\nFae task: derived\nFae dispatch: derived/") {
		t.Fatalf("task commit message %q", msg)
	}
	entries, err := os.ReadDir(records)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, entry := range entries {
		raw, err := os.ReadFile(filepath.Join(records, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var r struct {
			Stage string
			Scope json.RawMessage
		}
		if err = json.Unmarshal(raw, &r); err != nil || len(r.Scope) == 0 {
			t.Fatal("turn without task_scope", string(raw), err)
		}
		if prior, ok := seen[r.Stage]; ok && prior != string(r.Scope) {
			t.Fatalf("%s turns saw different scopes: %s and %s", r.Stage, prior, r.Scope)
		}
		seen[r.Stage] = string(r.Scope)
	}
	const declared = `{"files":["answer.go","answer_test.go"],"kind":"task_scope","test_file":""}`
	const red = `{"files":["answer.go","answer_test.go"],"kind":"task_scope","test_file":"answer_test.go"}`
	for stage, want := range map[string]string{
		"intake":     `{"files":[],"kind":"task_scope","test_file":""}`,
		"spec":       `{"files":[],"kind":"task_scope","test_file":""}`,
		"plan":       declared,
		"test-first": declared,
		"implement":  red,
		"reviews":    red,
		"oracle":     red,
	} {
		if seen[stage] != want {
			t.Errorf("%s task_scope %s, want %s", stage, seen[stage], want)
		}
	}
}
