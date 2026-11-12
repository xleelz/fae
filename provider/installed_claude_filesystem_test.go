package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/commandshim"
	agentruntime "github.com/xleelz/fae/runtime"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/workflow"
)

// This is an actual installed-CLI/model turn, not a scripted executable or
// help-only probe. Operators opt in using their configured provider account.
func TestInstalledClaudeReadAndShellFilesystemConformance(t *testing.T) {
	if os.Getenv("FAE_TEST_INSTALLED_CLAUDE") != "1" {
		t.Skip("set FAE_TEST_INSTALLED_CLAUDE=1 for actual configured Claude turn")
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bubblewrap unavailable; production filesystem view cannot run")
	}
	binary, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("Claude CLI unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root, readRoot, logs := t.TempDir(), t.TempDir(), t.TempDir()
	if err = os.WriteFile(filepath.Join(root, "allowed.txt"), []byte("WORKTREE_NATIVE_READ\nsecond\n"), 0640); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "denied.txt"), []byte("DENIED_CONTENT"), 0640); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(readRoot, "root.txt"), []byte("READROOT_NATIVE_READ\n"), 0640); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err = os.WriteFile(outside, []byte("OUTSIDE_CONTENT"), 0600); err != nil {
		t.Fatal(err)
	}
	outputDir, stagingDir := t.TempDir(), t.TempDir()
	outputFile, stagingFile := filepath.Join(outputDir, "generated", "result.txt"), filepath.Join(stagingDir, "stage.txt")
	if err = os.Mkdir(filepath.Dir(outputFile), 0700); err != nil {
		t.Fatal(err)
	}
	gitBinary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	prompt := "Perform these filesystem conformance checks with actual tools. Use Read on relative allowed.txt offset1 limit1. Use Read on absolute " + filepath.Join(readRoot, "root.txt") + ". Attempt Read of " + outside + " and denied.txt, both must fail. Use Bash `pwd` as your first shell command. Then use Write on " + outputFile + " with content OUTPUT_WRITE_OK and Write on " + stagingFile + " with content STAGING_WRITE_OK; these must succeed. Attempt Write on " + filepath.Join(root, "out-of-scope.txt") + " and " + filepath.Join(readRoot, "root.txt") + "; both must be denied. Finally Bash `" + gitBinary + " status` must be refused by the registered pre-tool hook. Report results. Do not modify anything else."
	// Build the actual broker/bridge and use Direct.Run: credentials, dedicated
	// HOME/TMPDIR, role shims, generated hooks, brief and collection all match
	// the production path. This opt-in test intentionally calls a real model.
	bridge, broker := filepath.Join(t.TempDir(), "fae-claude-bridge"), filepath.Join(t.TempDir(), "fae")
	for _, build := range []struct{ output, pkg string }{{bridge, "../cmd/fae-claude-bridge"}, {broker, "../cmd/fae"}} {
		if out, e := exec.CommandContext(ctx, "go", "build", "-o", build.output, build.pkg).CombinedOutput(); e != nil {
			t.Fatal(e, string(out))
		}
	}
	gitRepository(t, root, "allowed.txt", "denied.txt")
	brief, err := json.Marshal([]any{
		map[string]any{"kind": "stage", "stage": "spec", "instructions": prompt + " Return accepted:true using the required structured output."},
		map[string]any{"kind": "kernel_contract", "output_schema": json.RawMessage(`{"type":"object","properties":{"accepted":{"type":"boolean"}},"required":["accepted"],"additionalProperties":false}`)},
		map[string]any{"kind": "task_scope", "files": []string{}, "read_only_files": []string{"allowed.txt"}, "reads": workflow.ReadPolicy{Deny: []string{"denied.txt"}}, "read_roots": map[string]ReadRootView{"root": {Path: readRoot}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, key := range claudeCredentialVariables {
		if value := os.Getenv(key); value != "" {
			env[key] = value
		}
	}
	direct := agentruntime.Direct{Root: logs, Command: []string{bridge, "--claude", binary, "--config", "{config}"}, Environment: env, Timeout: 90 * time.Second, Shims: commandshim.RoleShims{Broker: broker, Ledger: filepath.Join(t.TempDir(), "unused-ledger")}}
	result, err := direct.Run(ctx, agentruntime.Launch{DispatchID: "installed-conformance", SessionID: "installed-conformance", TaskID: "installed-conformance", Role: "spec", Stage: "spec", Mode: "local", Session: "agentic", Worktree: root, IsolationRequired: true, Reads: &workflow.ReadPolicy{Deny: []string{"denied.txt"}}, ArtifactsOnly: true, OutputDir: outputDir, StagingDir: stagingDir, StagingWrite: true, WriteScopes: [][]string{{"generated/**"}}, ReadRootPaths: []string{readRoot}, NativeReadDirs: []string{readRoot}, Brief: brief, Binding: workflow.Binding{Provider: "claude", Model: os.Getenv("FAE_TEST_CLAUDE_MODEL")}, Profile: workflow.Profile{Allowed: []string{"read", "write", "edit", "shell", "handoff"}, ShellAllow: [][]string{{"pwd"}, {gitBinary, "status"}}}})
	if err != nil {
		t.Fatalf("production launch failed: %v\n%s", err, result.Tail)
	}
	paths, err := filepath.Glob(filepath.Join(result.LogDirectory, "provider-*", "events.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("provider transcript missing: %v %v", paths, err)
	}
	transcript, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	relativeRead, rootRead, firstShell := false, false, ""
	for _, line := range strings.Split(string(transcript), "\n") {
		var record struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type  string `json:"type"`
					Name  string `json:"name"`
					Input struct {
						Path    string `json:"file_path"`
						Offset  int    `json:"offset"`
						Limit   int    `json:"limit"`
						Command string `json:"command"`
					} `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &record) != nil || record.Type != "assistant" {
			continue
		}
		for _, block := range record.Message.Content {
			if block.Type != "tool_use" {
				continue
			}
			if block.Name == "Read" {
				relativeRead = relativeRead || (block.Input.Path == "allowed.txt" && block.Input.Offset == 1 && block.Input.Limit == 1)
				rootRead = rootRead || block.Input.Path == filepath.Join(readRoot, "root.txt")
			}
			if block.Name == "Bash" && firstShell == "" {
				firstShell = strings.TrimSpace(block.Input.Command)
			}
		}
	}
	if !relativeRead || !rootRead || firstShell != "pwd" {
		t.Errorf("installed native calls did not conform: relative range=%v rootRead=%v firstShell=%q", relativeRead, rootRead, firstShell)
	}
	text := string(transcript)
	for _, marker := range []string{"DENIED_CONTENT", "OUTSIDE_CONTENT"} {
		if strings.Contains(text, marker) {
			t.Errorf("forbidden content exposed: %s", marker)
		}
	}
	if err := assertInstalledConformanceResults(transcript, root, readRoot, outside, map[string]string{"output-write": outputFile, "staging-write": stagingFile, "worktree-write": filepath.Join(root, "out-of-scope.txt"), "root-write": filepath.Join(readRoot, "root.txt"), "shim-bypass": gitBinary + " status"}); err != nil {
		t.Error(err)
	}
	for _, marker := range []string{"WORKTREE_NATIVE_READ", "READROOT_NATIVE_READ", `"tool_use"`, `"tool_result"`} {
		if !strings.Contains(text, marker) {
			t.Errorf("installed CLI did not demonstrate %s", marker)
		}
	}
	for path, marker := range map[string]string{outputFile: "OUTPUT_WRITE_OK", stagingFile: "STAGING_WRITE_OK"} {
		data, e := os.ReadFile(path)
		if e != nil || strings.TrimSpace(string(data)) != marker {
			t.Errorf("successful private write not collected at %s: %s %v", path, data, e)
		}
	}
	audit, e := os.ReadFile(filepath.Join(result.LogDirectory, "shell-refusals.jsonl"))
	if e != nil || !strings.Contains(string(audit), "bypasses the role command shim") {
		t.Errorf("no hook-generated absolute shim bypass refusal: %s %v", audit, e)
	}
	if data, err := os.ReadFile(filepath.Join(readRoot, "root.txt")); err != nil || string(data) != "READROOT_NATIVE_READ\n" {
		t.Fatal("read root modified", err)
	}
	if data, err := os.ReadFile(filepath.Join(root, "denied.txt")); err != nil || string(data) != "DENIED_CONTENT" {
		t.Fatal("shared denied file changed", err)
	}
}

// Require correlated results, not merely a model announcing attempted tools.
func assertInstalledConformanceResults(transcript []byte, root, readRoot, outside string, extra ...map[string]string) error {
	extraPaths := map[string]string{}
	for _, paths := range extra {
		for key, path := range paths {
			extraPaths[key] = path
		}
	}
	calls := map[string]string{}
	outcomes := map[string]bool{}
	for _, line := range strings.Split(string(transcript), "\n") {
		var record struct {
			Message struct {
				Content []struct {
					Type, ID, Name string
					ToolUseID      string `json:"tool_use_id"`
					IsError        bool   `json:"is_error"`
					Input          struct {
						Path    string `json:"file_path"`
						Command string `json:"command"`
					} `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &record) != nil {
			continue
		}
		for _, block := range record.Message.Content {
			if block.Type == "tool_use" {
				key := ""
				if block.Name == "Read" {
					switch block.Input.Path {
					case "allowed.txt", filepath.Join(root, "allowed.txt"):
						key = "allowed"
					case filepath.Join(readRoot, "root.txt"):
						key = "readroot"
					case "denied.txt", filepath.Join(root, "denied.txt"):
						key = "denied"
					case outside:
						key = "outside"
					}
				}
				if block.Name == "Bash" && strings.Contains(block.Input.Command, filepath.Join(readRoot, "root.txt")) && strings.Contains(block.Input.Command, ">") {
					key = "root-write"
				}
				for candidate, path := range extraPaths {
					if block.Name == "Write" && block.Input.Path == path {
						key = candidate
					}
					if candidate == "shim-bypass" && block.Name == "Bash" && strings.TrimSpace(block.Input.Command) == path {
						key = candidate
					}
				}
				if key != "" {
					calls[block.ID] = key
				}
			}
			if block.Type == "tool_result" {
				if key := calls[block.ToolUseID]; key != "" {
					wantError := key == "denied" || key == "outside" || key == "root-write" || key == "worktree-write" || key == "shim-bypass"
					if block.IsError == wantError {
						outcomes[key] = true
					}
				}
			}
		}
	}
	required := []string{"allowed", "readroot", "denied", "outside", "root-write"}
	for key := range extraPaths {
		if !slices.Contains(required, key) {
			required = append(required, key)
		}
	}
	for _, key := range required {
		if !outcomes[key] {
			return fmt.Errorf("no correlated expected tool result for %s", key)
		}
	}
	return nil
}

func TestInstalledConformanceRequiresCorrelatedOutcomes(t *testing.T) {
	root, readRoot, outside := "/task", "/input", "/outside"
	var lines []string
	for _, c := range []struct {
		id, name, path, command string
		failed                  bool
	}{{"a", "Read", "allowed.txt", "", false}, {"b", "Read", "/input/root.txt", "", false}, {"c", "Read", "denied.txt", "", true}, {"d", "Read", "/outside", "", true}, {"e", "Bash", "", "printf changed > /input/root.txt", true}} {
		call, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "tool_use", "id": c.id, "name": c.name, "input": map[string]any{"file_path": c.path, "command": c.command}}}}})
		result, _ := json.Marshal(map[string]any{"message": map[string]any{"content": []any{map[string]any{"type": "tool_result", "tool_use_id": c.id, "is_error": c.failed}}}})
		lines = append(lines, string(call), string(result))
	}
	all := strings.Join(lines, "\n")
	if err := assertInstalledConformanceResults([]byte(all), root, readRoot, outside); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Join(lines[:len(lines)-1], "\n"), strings.ReplaceAll(all, `"is_error":true`, `"is_error":false`)} {
		if err := assertInstalledConformanceResults([]byte(bad), root, readRoot, outside); err == nil {
			t.Fatal("missing or successful denial result accepted")
		}
	}
}
