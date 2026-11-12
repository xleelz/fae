package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
)

// TestLiveClaudeBridgeFixture is the bridge TestDaemonClaudeProviderPath
// launches: the production provider.Run with the Claude Code protocol, over a
// scripted CLI (never a real provider session).
func TestLiveClaudeBridgeFixture(t *testing.T) {
	if os.Getenv("LIVE_CLAUDE_BRIDGE") != "1" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	// Under FAE-HOST-011 (2026-11-05 amendment), an agentic session granted a
	// shell has role shims placed first on PATH; a tool-less session has no
	// role shims on PATH.
	if probe := os.Getenv("LIVE_CLAUDE_SHIM_PROBE"); probe != "" && strings.Contains(os.Getenv("PATH"), "command-shims") {
		out, err := exec.Command("go", "version").CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
		_ = os.WriteFile(probe, []byte(fmt.Sprintf("%d %s", code, strings.TrimSpace(string(out)))), 0600)
	}
	if toollessProbe := os.Getenv("LIVE_CLAUDE_TOOLLESS_PROBE"); toollessProbe != "" && !strings.Contains(os.Getenv("PATH"), "command-shims") {
		_ = os.WriteFile(toollessProbe, []byte(os.Getenv("PATH")), 0600)
	}
	c := provider.Config{Provider: "claude", SessionRoot: os.Getenv("LIVE_CLAUDE_SESSIONS"), Binary: os.Getenv("LIVE_CLAUDE_CLI"), Files: []string{"answer.go"}, Timeout: 30 * time.Second}
	if err := provider.Run(ctx, c, os.Args[len(os.Args)-1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

// claudeCLIScript emulates Claude Code's stream-json: one announced session
// with a StructuredOutput-only surface and one structured result. It logs its
// arguments to log, so the test can see a resumed session.
func claudeCLIScript(log string) string {
	return `#!/bin/sh
sid=22345678-1234-4234-8234-123456789abc
resumed=fresh
for arg in "$@"; do
 case "$arg" in --session-id=*) sid="${arg#--session-id=}";; --resume=*) sid="${arg#--resume=}"; resumed=resumed;; esac
done
printf '%s %s\n' "$resumed" "$sid" >> '` + log + `'
cat > /dev/null
tools='["StructuredOutput"]'
[ -e '` + log + `.unsafe' ] && tools='["StructuredOutput","Bash"]'
printf '{"type":"system","subtype":"init","session_id":"%s","tools":%s,"mcp_servers":[]}\n' "$sid" "$tools"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"%s","structured_output":{"output":{"accepted":true},"edits":[]}}\n' "$sid"
`
}

// FAE-SEC-002, FAE-WF-033, FAE-VER-002: live turns run through the production
// Claude provider path (bridge -> provider.Run -> CanAcceptClaudeTurn) and a
// workflow-global role resumes its persistent provider context on the next
// task (openProviderSession -> CanResumeProvider), driven by the daemon.
func TestDaemonClaudeProviderPath(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	scratch := filepath.Dir(cfg.Root)
	sessions, cli, log := filepath.Join(scratch, "claude-sessions"), filepath.Join(scratch, "claude"), filepath.Join(scratch, "claude.log")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte(claudeCLIScript(log)), 0700); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Live.Command = []string{exe, "-test.run=^TestLiveClaudeBridgeFixture$", "--", "{config}"}
	probe := filepath.Join(scratch, "shim-probe")
	toollessProbe := filepath.Join(scratch, "toolless-probe")
	cfg.Live.Environment = map[string]string{"LIVE_CLAUDE_BRIDGE": "1", "LIVE_CLAUDE_CLI": cli, "LIVE_CLAUDE_SESSIONS": sessions, "LIVE_CLAUDE_SHIM_PROBE": probe, "LIVE_CLAUDE_TOOLLESS_PROBE": toollessProbe, "CLAUDE_CODE_OAUTH_TOKEN": "fixture-token"}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	accepted := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "intake", Path: []string{"accepted"}}, {Op: "literal", Value: true}}}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "claude-path", Start: "intake", Limits: workflow.Limits{Attempts: 4, TestRuns: 2, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"intake": {Instructions: "Accept the item.", Briefs: map[string]string{"default": "Item {{task_id}}."}, Routes: []workflow.Binding{{Provider: "claude", Model: "fixture-model"}}, Scope: "workflow-global", Cap: 1, Outputs: []string{"IntakeReport"}}},
		Artifacts: map[string]workflow.ArtifactType{"IntakeReport": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}},
		Stages:    map[string]workflow.Stage{"intake": {Primitive: workflow.AgentTurn, Role: "intake", Output: "IntakeReport"}, "intake-gate": {Primitive: workflow.Gate, Expr: &accepted, Failure: "stop"}, "stop": {Primitive: workflow.Escalate}},
		Edges:     []workflow.Edge{{From: "intake", To: "intake-gate"}, {From: "intake-gate", To: "Done"}, {From: "intake-gate", To: "stop"}}})
	if issues := workflow.Validate(doc); len(issues) != 0 {
		t.Fatal(issues)
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"claude-one", "claude-two"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, RunID: id, WorkflowID: doc.ID, AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(id, err)
		}
		awaitState(t, d, id, "Done")
		e := liveEnvelope(t, d, id)
		if e.Job.ExecutionKind != "live" || len(e.Job.Provenance) == 0 || e.Job.Provenance[0].Stage != "intake" {
			t.Fatalf("%s: no live intake evidence %+v", id, e.Job.Provenance)
		}
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	turns := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(turns) != 2 || !strings.HasPrefix(turns[0], "fresh ") || !strings.HasPrefix(turns[1], "resumed ") || strings.Fields(turns[0])[1] != strings.Fields(turns[1])[1] {
		t.Fatalf("workflow-global provider context not resumed across tasks: %q", turns)
	}
	if toollessRaw, err := os.ReadFile(toollessProbe); err != nil || strings.Contains(string(toollessRaw), "command-shims") {
		t.Fatalf("tool-less session unexpectedly mounted role shims on PATH: %q %v", toollessRaw, err)
	}
	// FAE-HOST-011 & FAE-RT-015: an agentic role session granted shell places
	// role shims first on PATH where go is the denial wrapper (exit 77).
	agenticDoc := doc
	agenticDoc.ID = "claude-agentic-path"
	agenticDoc.Roles = map[string]workflow.Role{
		"intake": {
			Instructions: "Accept the item.",
			Briefs:       map[string]string{"default": "Item {{task_id}}."},
			Routes:       []workflow.Binding{{Provider: "claude", Model: "fixture-model"}},
			Scope:        "task-bound",
			Cap:          1,
			Session:      "agentic",
			Profile:      workflow.Profile{Allowed: []string{"handoff", "shell"}},
			Outputs:      []string{"IntakeReport"},
		},
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: agenticDoc.ID, Document: agenticDoc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "claude-shim", RunID: "claude-shim", WorkflowID: agenticDoc.ID, AuthorizingMessage: "message"}, "claude-shim"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "claude-shim", "Done")
	if raw, err = os.ReadFile(probe); err != nil || string(raw) != "77 fae: command refused (exit 77): role tools and nested heavy commands require a separate broker dispatch; use a scoped host-command stage" {
		t.Fatalf("role session tool not refused by the shim: %q %v", raw, err)
	}
	// A turn whose CLI announces a command tool is never accepted: the bridge
	// fails, the task records no intake evidence and the turn parks for the
	// operator (FAE-ROB-016) instead of reaching Done.
	if err = os.WriteFile(log+".unsafe", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "claude-unsafe", RunID: "claude-unsafe", WorkflowID: doc.ID, AuthorizingMessage: "message"}, "claude-unsafe"); err != nil {
		t.Fatal(err)
	}
	_, e := awaitJob(t, d, "claude-unsafe", 3*time.Minute, func(_ controlplane.JobSnapshot, e envelope) bool {
		return e.Job.Parked && !deferredPark(e) || e.Job.State.Terminal()
	})
	if !e.Job.Parked || e.Job.Reason != "UncertainDispatch" || e.Job.State != "intake" || len(e.Job.Provenance) != 0 {
		t.Fatalf("unsafe Claude turn accepted or not parked: state=%s reason=%s provenance=%+v", e.Job.State, e.Job.Reason, e.Job.Provenance)
	}
	if raw, _ = os.ReadFile(log); strings.Count(string(raw), "\n") < 3 {
		t.Fatal("unsafe turn never reached the CLI", string(raw))
	}
}
