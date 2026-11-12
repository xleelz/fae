package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/engine"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

func initGitWorktree(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = cleanGitEnv(
			"GIT_AUTHOR_NAME=Fae",
			"GIT_AUTHOR_EMAIL=fae@example.invalid",
			"GIT_COMMITTER_NAME=Fae",
			"GIT_COMMITTER_EMAIL=fae@example.invalid",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-q", "--allow-empty", "-m", "initial")
}

// FAE-RT-017(a,c,d): CaptureTaskScope enforces 1 MiB per-file and 4 MiB
// per-turn inline bounds, separates write_scope from read_only_inputs, lists
// >1 MiB files in unavailable_scope with reason "exceeds_inline_bound" for
// tool-less sessions, and lists metadata (path, bytes, lines, sha256) without
// full content for agentic sessions.
func TestCaptureTaskScopeInlineBoundsAndSeparatedScopes(t *testing.T) {
	if DefaultInlineFileBytes != 1<<20 || DefaultInlineTurnBytes != 4<<20 {
		t.Fatalf("unexpected defaults: file=%d turn=%d", DefaultInlineFileBytes, DefaultInlineTurnBytes)
	}
	root := t.TempDir()
	writeFile(t, root, "small.go", "package small\n")
	writeFile(t, root, "readonly.go", "package readonly\nconst Version = 1\n")
	largeContent := strings.Repeat("line0123456789\n", (1<<20)/15+10) // > 1 MiB
	writeFile(t, root, "large.go", largeContent)

	// Tool-less session: > 1 MiB file is omitted from WriteScope and reported in UnavailableScope.
	toolless, err := CaptureTaskScope(root, TaskScope{
		Session:       "",
		Files:         []string{"small.go", "large.go"},
		ReadOnlyFiles: []string{"readonly.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if toolless.WriteScope["small.go"] != "package small\n" {
		t.Fatalf("expected small.go inlined, got %q", toolless.WriteScope["small.go"])
	}
	if _, ok := toolless.WriteScope["large.go"]; ok {
		t.Fatal("large.go must not be inlined in tool-less WriteScope")
	}
	if toolless.ReadOnlyInputs["readonly.go"] != "package readonly\nconst Version = 1\n" {
		t.Fatalf("expected readonly.go in ReadOnlyInputs, got %q", toolless.ReadOnlyInputs["readonly.go"])
	}
	if len(toolless.UnavailableScope) != 1 || toolless.UnavailableScope[0].Path != "large.go" || toolless.UnavailableScope[0].Reason != "exceeds_inline_bound" {
		t.Fatalf("unexpected UnavailableScope: %+v", toolless.UnavailableScope)
	}

	// Agentic session: > 1 MiB file is omitted from inline WriteScope and listed in Metadata.
	agentic, err := CaptureTaskScope(root, TaskScope{
		Session:       "agentic",
		Files:         []string{"small.go", "large.go"},
		ReadOnlyFiles: []string{"readonly.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := agentic.WriteScope["large.go"]; ok {
		t.Fatal("large.go must not be inlined in agentic WriteScope")
	}
	if len(agentic.UnavailableScope) != 0 {
		t.Fatalf("agentic turn should not mark large file unavailable: %+v", agentic.UnavailableScope)
	}
	if len(agentic.Metadata) != 1 || agentic.Metadata[0].Path != "large.go" || agentic.Metadata[0].Bytes != int64(len(largeContent)) || agentic.Metadata[0].Lines <= 0 || len(agentic.Metadata[0].SHA256) != 64 {
		t.Fatalf("unexpected agentic Metadata: %+v", agentic.Metadata)
	}
}

// FAE-RT-015(a) & FAE-RT-017(b,c): AgenticToolbox enforces declared tool
// capabilities, supports line-range read and replacement edit on a > 1 MiB
// file in write_scope, refuses edits on read_only_inputs or out-of-scope
// files, and completes via handoff.
func TestAgenticToolboxRangeReadReplacementEditAndReadOnlyRefusal(t *testing.T) {
	root := t.TempDir()
	var b strings.Builder
	totalLines := 70000
	for i := 1; i <= totalLines; i++ {
		fmt.Fprintf(&b, "// line %06d padding0123456789\n", i)
	}
	largeContent := b.String()
	if len(largeContent) <= DefaultInlineFileBytes {
		t.Fatalf("expected > 1 MiB test file, got %d bytes", len(largeContent))
	}
	writeFile(t, root, "large.go", largeContent)
	writeFile(t, root, "readonly.go", "package main\nconst Frozen = true\n")
	writeFile(t, root, "outside.go", "package main\n")

	schema := json.RawMessage(`{"type":"object","properties":{"accepted":{"type":"boolean"}},"required":["accepted"],"additionalProperties":false}`)
	tb := &AgenticToolbox{
		Worktree:       root,
		Profile:        workflow.Profile{Allowed: []string{"read", "edit", "handoff"}},
		ChangeProducer: true,
		Stage:          "implement",
		WriteScope:     []string{"large.go"},
		ReadOnlyFiles:  []string{"readonly.go"},
		OutputSchema:   schema,
	}

	// Unlisted tool capability (e.g. shell or search_glob) is refused.
	if _, err := tb.Call(context.Background(), AgenticToolCall{Tool: "shell", Command: "true"}); err == nil || !strings.Contains(err.Error(), "not allowed by role profile") {
		t.Fatalf("expected unlisted tool refusal, got %v", err)
	}

	// Range read on > 1 MiB file returns exact requested lines.
	readRes, err := tb.Call(context.Background(), AgenticToolCall{
		Tool:      "read",
		Path:      "large.go",
		StartLine: 50000,
		EndLine:   50002,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(readRes.Content, "line 050000") || !strings.Contains(readRes.Content, "line 050002") || strings.Contains(readRes.Content, "line 050003") {
		t.Fatalf("unexpected range read content: %q", readRes.Content)
	}

	// Targeted replacement edit on > 1 MiB file in write_scope succeeds.
	_, err = tb.Call(context.Background(), AgenticToolCall{
		Tool:        "edit",
		Path:        "large.go",
		Target:      "// line 050001 padding0123456789\n",
		Replacement: "// line 050001 UPDATED_IN_AGENTIC_TURN\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	verifyRead, err := tb.Call(context.Background(), AgenticToolCall{
		Tool:      "read",
		Path:      "large.go",
		StartLine: 50000,
		EndLine:   50002,
	})
	if err != nil || !strings.Contains(verifyRead.Content, "UPDATED_IN_AGENTIC_TURN") {
		t.Fatalf("replacement edit not reflected in > 1 MiB file: %v %q", err, verifyRead.Content)
	}

	// Edit targeting read_only_inputs is refused immediately.
	if _, err := tb.Call(context.Background(), AgenticToolCall{
		Tool:        "edit",
		Path:        "readonly.go",
		Target:      "Frozen = true",
		Replacement: "Frozen = false",
	}); err == nil || !strings.Contains(err.Error(), "read_only_inputs") {
		t.Fatalf("expected read_only_inputs edit refusal, got %v", err)
	}
	if got := readFile(t, root, "readonly.go"); got != "package main\nconst Frozen = true\n" {
		t.Fatalf("readonly.go was modified: %q", got)
	}

	// Edit targeting out-of-scope file is refused immediately.
	if _, err := tb.Call(context.Background(), AgenticToolCall{
		Tool:        "edit",
		Path:        "outside.go",
		Target:      "package main",
		Replacement: "package hacked",
	}); err == nil || !strings.Contains(err.Error(), "outside write_scope") {
		t.Fatalf("expected out-of-scope edit refusal, got %v", err)
	}

	// Handoff validates against OutputSchema and records completion payload.
	if _, err := tb.Call(context.Background(), AgenticToolCall{
		Tool:   "handoff",
		Output: json.RawMessage(`{"accepted":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if string(tb.HandoffOutput()) != `{"accepted":true}` {
		t.Fatalf("unexpected handoff output: %s", tb.HandoffOutput())
	}
}

// FAE-RT-015(b): At turn collection, worktree diff against startTree reverts
// any modified, created, or deleted file outside write_scope (or in
// read_only_inputs) and refuses the turn as output-refused naming the
// offending paths.
func TestAgenticTurnRevertsOutOfScopeWritesAndRefusesOutput(t *testing.T) {
	c, config, l := turnFixture(t, "implement", []any{
		map[string]any{
			"kind":             "task_scope",
			"files":            []string{"answer.go"},
			"read_only_files":  []string{"readonly.go"},
			"test_file":        "",
		},
	}, `{"output":{"accepted":true},"edits":[]}`)
	writeFile(t, l.Worktree, "readonly.go", "package readonly\n")
	writeFile(t, l.Worktree, "other.go", "package other\n")
	initGitWorktree(t, l.Worktree)

	l.Session = "agentic"
	l.ChangeProducer = true
	l.Profile.Allowed = []string{"read", "edit", "handoff"}
	l.ReadOnlyFiles = []string{"readonly.go"}
	c.Files = []string{"answer.go"}
	writeLaunch(t, config, l)

	// Fake provider modifies in-scope answer.go, modifies out-of-scope other.go,
	// modifies readonly.go, and creates untracked rogue.go in l.Worktree.
	script := `#!/bin/sh
out=''
while [ "$#" -gt 0 ]; do
 case "$1" in --output-last-message) shift; out="$1";; esac
 shift
done
cat > prompt.json
printf 'package answer\n' > "` + filepath.Join(l.Worktree, "answer.go") + `"
printf 'package hacked\n' > "` + filepath.Join(l.Worktree, "other.go") + `"
printf 'package hacked\n' > "` + filepath.Join(l.Worktree, "readonly.go") + `"
printf 'package rogue\n' > "` + filepath.Join(l.Worktree, "rogue.go") + `"
printf '{"output":{"accepted":true},"edits":[]}\n' > "$out"
`
	if err := os.WriteFile(c.Binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	err := Run(context.Background(), c, config)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Class != agentruntime.RefusedOutput {
		t.Fatalf("expected output-refused, got %v", err)
	}
	for _, want := range []string{"other.go", "readonly.go", "rogue.go"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected refusal to name offending path %q, got %v", want, err)
		}
	}
	// Out-of-scope and read-only files must be reverted to startTree, and untracked rogue.go removed.
	if got := readFile(t, l.Worktree, "other.go"); got != "package other\n" {
		t.Fatalf("other.go not reverted: %q", got)
	}
	if got := readFile(t, l.Worktree, "readonly.go"); got != "package readonly\n" {
		t.Fatalf("readonly.go not reverted: %q", got)
	}
	if _, statErr := os.Stat(filepath.Join(l.Worktree, "rogue.go")); !os.IsNotExist(statErr) {
		t.Fatalf("untracked out-of-scope rogue.go was not cleaned: %v", statErr)
	}
}

// FAE-RT-015(c) & FAE-RT-019(a,b): Agentic Claude turn checkpoints
// {provider_session, worktree_tree} on tool/assistant boundaries, enforces
// max_assistant_turns as turn-budget-exhausted, preserves ThreadID in
// providerSession when checkpointed, and resumes in the same provider_session
// keeping in-scope edits and reverting out-of-scope edits.
func TestAgenticTurnBudgetExhaustionAndCheckpointResume(t *testing.T) {
	c, config, l := turnFixture(t, "implement", []any{
		taskScope([]string{"answer.go"}, ""),
	}, acceptedNoEdits)
	writeFile(t, l.Worktree, "other.go", "package other\n")
	initGitWorktree(t, l.Worktree)

	c.Provider = "claude"
	c.Binary = filepath.Join(t.TempDir(), "provider")
	c.SessionRoot = filepath.Join(t.TempDir(), "sessions")
	c.Files = []string{"answer.go"}
	l.Binding.Provider = "claude"
	l.Binding.Model = "claude-sonnet-4-6"
	l.WorkflowID = "wf"
	l.WorkflowRevision = "rev-1"
	l.Role = "implementer"
	l.Session = "agentic"
	l.SessionPolicy = "task-bound"
	l.ChangeProducer = true
	l.Profile.Allowed = []string{"read", "edit", "handoff"}
	l.TurnBudget = &workflow.TurnBudgetSpec{
		MaxAssistantTurns: 1,
		OnExhaustion:      "resume-once",
	}
	writeLaunch(t, config, l)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-credential")

	// First run: provider announces session, makes in-scope edit on turn 1,
	// then exceeds max_assistant_turns on turn 2.
	script1 := `#!/bin/sh
sid=""
for arg in "$@"; do
  case "$arg" in
    --session-id=*) sid="${arg#--session-id=}" ;;
    --resume=*) sid="${arg#--resume=}" ;;
  esac
done
printf '{"type":"system","subtype":"init","session_id":"%s","tools":["StructuredOutput","Read","Edit"],"mcp_servers":[]}\n' "$sid"
printf 'package answer // checkpointed step 1\n' > "` + filepath.Join(l.Worktree, "answer.go") + `"
printf '{"type":"assistant","session_id":"%s"}\n' "$sid"
printf '{"type":"assistant","session_id":"%s"}\n' "$sid"
printf '{"type":"result","subtype":"error_max_turns","is_error":true,"session_id":"%s"}\n' "$sid"
`
	if err := os.WriteFile(c.Binary, []byte(script1), 0700); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), c, config)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Class != agentruntime.TurnBudgetExhausted {
		t.Fatalf("expected turn-budget-exhausted refusal, got %v", err)
	}
	cp, err := agentruntime.ReadTurnCheckpoint(l.LogDirectory)
	if err != nil || cp == nil || cp.ProviderSession == "" || cp.WorktreeTree == "" {
		t.Fatalf("expected durable checkpoint.json after budget exhaustion, got %+v (%v)", cp, err)
	}

	// Simulate an out-of-scope file pollution and an uncheckpointed edit before resume.
	writeFile(t, l.Worktree, "other.go", "corrupted out of scope\n")
	writeFile(t, l.Worktree, "answer.go", "uncheckpointed partial edit\n")

	// Second run: resume from cp.ProviderSession and cp.WorktreeTree.
	l.ResumeSession = cp.ProviderSession
	l.ResumeTree = cp.WorktreeTree
	l.TurnBudget = &workflow.TurnBudgetSpec{MaxAssistantTurns: 3, OnExhaustion: "resume-once"}
	l.LogDirectory = t.TempDir()
	l.Handoff = filepath.Join(l.LogDirectory, "handoff.json")
	writeLaunch(t, config, l)

	script2 := `#!/bin/sh
resumed=""
for arg in "$@"; do
  case "$arg" in
    --resume=*) resumed="${arg#--resume=}" ;;
  esac
done
if [ -z "$resumed" ]; then
  echo "missing --resume flag" >&2
  exit 2
fi
printf '{"type":"system","subtype":"init","session_id":"%s","tools":["StructuredOutput","Read","Edit"],"mcp_servers":[]}\n' "$resumed"
printf '{"type":"assistant","session_id":"%s"}\n' "$resumed"
printf '{"type":"result","subtype":"success","is_error":false,"session_id":"%s","structured_output":{"output":{"accepted":true},"edits":[]}}\n' "$resumed"
`
	if err := os.WriteFile(c.Binary, []byte(script2), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), c, config); err != nil {
		t.Fatalf("resumed turn failed: %v", err)
	}
	// In-scope edit from checkpoint must be restored and kept; out-of-scope edit reverted.
	if got := readFile(t, l.Worktree, "answer.go"); got != "package answer // checkpointed step 1\n" {
		t.Fatalf("expected checkpointed in-scope answer.go, got %q", got)
	}
	if got := readFile(t, l.Worktree, "other.go"); got != "package other\n" {
		t.Fatalf("expected out-of-scope other.go reverted, got %q", got)
	}
}

// FAE-RT-015(d) & FAE-HOST-011: AgenticToolbox shell tool verifies that role
// shims are first on PATH and PreToolUse hook settings are registered, and
// denies commands that bypass role shims.
func TestAgenticShellRequiresRoleShimsAndPreToolHook(t *testing.T) {
	root := t.TempDir()
	shimParent := t.TempDir()
	shims, err := commandshim.InstallRoleShims(shimParent, commandshim.RoleShims{
		Worktree: root,
		Broker:   "/bin/true",
		Ledger:   t.TempDir(),
	}, "/usr/bin:/bin")
	if err != nil {
		t.Fatal(err)
	}
	settingsPath, err := agentruntime.WriteClaudePreToolHook(shimParent, "/bin/true")
	if err != nil {
		t.Fatal(err)
	}
	tb := &AgenticToolbox{
		Worktree:       root,
		Profile:        workflow.Profile{Allowed: []string{"shell", "handoff"}},
		ChangeProducer: true,
		Stage:          "implement",
		Env:            []string{"PATH=" + shims + ":/usr/bin:/bin"},
		ClaudeSettings: settingsPath,
	}
	// Normal shell command succeeds with shims first on PATH.
	res, err := tb.Call(context.Background(), AgenticToolCall{
		Tool:    "shell",
		Command: "command -v go",
	})
	if err != nil || !strings.HasPrefix(strings.TrimSpace(res.Stdout), shims) {
		t.Fatalf("expected role shim go first on PATH, got stdout=%q err=%v", res.Stdout, err)
	}
	// Bypassing role shims via absolute path /usr/bin/git is denied by PreToolUse check.
	if _, err := tb.Call(context.Background(), AgenticToolCall{
		Tool:    "shell",
		Command: "/usr/bin/git status",
	}); err == nil || !strings.Contains(err.Error(), "PreToolUse hook denied") {
		t.Fatalf("expected PreToolUse hook denial for absolute git path, got %v", err)
	}
}

func TestAgenticToolboxReadGuardDenyPathsReadRootsAndLiveNotifications(t *testing.T) {
	worktree := t.TempDir()
	readRootDir := t.TempDir()
	sessionRoot := t.TempDir()
	logDir := t.TempDir()

	writeFile(t, worktree, "allowed.go", "package allowed\nconst X = 1\n")
	writeFile(t, worktree, "secret.env", "SECRET=123\n")
	writeFile(t, readRootDir, "lib.go", "package lib\n")

	guard := &ReadGuard{
		Worktree:     worktree,
		LogDirectory: logDir,
		TaskID:       "task-guard",
		Role:         "coder",
		DenyPaths:    []string{"secret.env"},
		ReadRoots: map[string]ReadRootView{
			"ext": {Name: "ext", Path: readRootDir},
		},
	}

	if _, err := WriteLiveNotification(sessionRoot, "task-guard", engine.TaskNotification{
		ID:       "notif-1",
		TaskID:   "task-guard",
		Body:     "please check ext:lib.go",
		Delivery: "in-flight",
	}); err != nil {
		t.Fatal(err)
	}

	tb := &AgenticToolbox{
		Worktree:       worktree,
		Profile:        workflow.Profile{Allowed: []string{"read", "edit", "write", "handoff"}},
		WriteScope:     []string{"allowed.go"},
		ChangeProducer: true,
		Stage:          "implement",
		Guard:          guard,
		InstanceRoot:   sessionRoot,
		TaskID:         "task-guard",
	}

	// Reading denied path is refused and audited.
	if _, err := tb.Call(context.Background(), AgenticToolCall{Tool: "read_range", Path: "secret.env", StartLine: 1, EndLine: 5}); err == nil {
		t.Fatal("expected read_range on secret.env to be refused")
	}
	// Writing to read_root is refused.
	if _, err := tb.Call(context.Background(), AgenticToolCall{Tool: "write", Path: "ext:lib.go", Content: "hacked"}); err == nil {
		t.Fatal("expected write to read_root ext:lib.go to be refused")
	}
	// Reading read_root succeeds and appends live <task_notifications> at tool boundary.
	res, err := tb.Call(context.Background(), AgenticToolCall{Tool: "read_range", Path: "ext:lib.go", StartLine: 1, EndLine: 5})
	if err != nil {
		t.Fatalf("expected read_range on ext:lib.go to succeed, got %v", err)
	}
	if !strings.Contains(res.Content, "<task_notifications>") || !strings.Contains(res.Content, "please check ext:lib.go") {
		t.Fatalf("expected live notification appended to tool output, got %q", res.Content)
	}
	if len(tb.DeliveredNotifications) != 1 || tb.DeliveredNotifications[0].ID != "notif-1" {
		t.Fatalf("expected DeliveredNotifications recorded, got %+v", tb.DeliveredNotifications)
	}
}
