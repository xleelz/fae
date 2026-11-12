package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	r "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// claudeScript emulates Claude Code stream-json. Shell variables set by extra
// select the announced tool surface, result count/status and identity.
func claudeScript(extra string) string {
	return `#!/bin/sh
sid=12345678-1234-4234-8234-123456789abc
for arg in "$@"; do
 printf '%s\n' "$arg" >> args.txt
 case "$arg" in --session-id=*) sid="${arg#--session-id=}";; --resume=*) sid="${arg#--resume=}";; esac
done
env > env.txt
cat > prompt.json
tools='["StructuredOutput"]'
mcp='[]'
inits=1
results=1
subtype=success
is_error=false
output='{"output":{"accepted":true},"edits":[{"path":"answer.go","content":"new content"}]}'
` + extra + `
rsid=${rsid:-$sid}
i=0; while [ $i -lt $inits ]; do printf '{"type":"system","subtype":"init","session_id":"%s","tools":%s,"mcp_servers":%s}\n' "$sid" "$tools" "$mcp"; i=$((i+1)); done
printf '{"type":"assistant","session_id":"%s","message":{"content":[{"type":"text","text":"{\\"type\\":\\"result\\"}"}]}}\n' "$sid"
i=0; while [ $i -lt $results ]; do printf '{"type":"result","subtype":"%s","is_error":%s,"session_id":"%s","structured_output":%s}\n' "$subtype" "$is_error" "$rsid" "$output"; i=$((i+1)); done
`
}

func claudeFixture(t *testing.T, extra string) (Config, string, r.BridgeConfig) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fixture-token")
	c, config, l := providerFixture(t, "")
	l.Binding = workflow.Binding{Provider: "claude", Model: "configured-model", Effort: "low"}
	raw, _ := json.Marshal(l)
	if e := os.WriteFile(config, raw, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(c.Binary, []byte(claudeScript(extra)), 0700); e != nil {
		t.Fatal(e)
	}
	c.Provider = "claude"
	return c, config, l
}

func providerFile(t *testing.T, dir, name string) string {
	t.Helper()
	paths, _ := filepath.Glob(filepath.Join(dir, "provider-*", "cwd", name))
	if len(paths) != 1 {
		t.Fatal(name, paths)
	}
	data, e := os.ReadFile(paths[0])
	if e != nil {
		t.Fatal(e)
	}
	return string(data)
}

func TestClaudeBridgeSubprocess(t *testing.T) {
	c, config, l := claudeFixture(t, "")
	if e := Run(context.Background(), c, config); e != nil {
		t.Fatal(e)
	}
	data, _ := os.ReadFile(filepath.Join(l.Worktree, "answer.go"))
	if string(data) != "new content" {
		t.Fatal(string(data))
	}
	var h r.Handoff
	data, _ = os.ReadFile(l.Handoff)
	if json.Unmarshal(data, &h) != nil || h.DispatchID != "d" || string(h.Output) != `{"accepted":true}` {
		t.Fatal(string(data))
	}
	args := strings.Split(strings.TrimSuffix(providerFile(t, l.LogDirectory, "args.txt"), "\n"), "\n")
	joined := "\n" + strings.Join(args, "\n") + "\n"
	for _, want := range []string{"-p", "--output-format=stream-json", "--model=configured-model", "--effort=low", "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--permission-mode=dontAsk", "--permission-prompts=none", "--no-session-persistence"} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Fatal("missing", want, args)
		}
	}
	// "--tools" followed by an empty value disables every built-in tool.
	if !strings.Contains(joined, "\n--tools\n\n") || strings.Contains(joined, "dangerously") || strings.Contains(joined, "--session-id") {
		t.Fatal(args)
	}
	if !strings.Contains(providerFile(t, l.LogDirectory, "env.txt"), "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1") {
		t.Fatal("nonessential traffic not disabled")
	}
	prompt := promptBlocks(t, []byte(providerFile(t, l.LogDirectory, "prompt.json")))
	if !strings.Contains(string(prompt["kernel_contract"]), "structured output") {
		t.Fatal(string(prompt["kernel_contract"]))
	}
	answers, _ := filepath.Glob(filepath.Join(l.LogDirectory, "provider-*", "answer.json"))
	if len(answers) != 1 {
		t.Fatal(answers)
	}
	if data, _ = os.ReadFile(answers[0]); !strings.Contains(string(data), `"edits"`) {
		t.Fatal(string(data))
	}
}

func TestClaudeBridgeRefusesUnsafeOrIncompleteTurns(t *testing.T) {
	for name, extra := range map[string]string{
		"command tool":     `tools='["StructuredOutput","Bash"]'`,
		"mcp server":       `mcp='[{"name":"remote","status":"connected"}]'`,
		"error result":     `subtype=error_during_execution; is_error=true`,
		"api error":        `is_error=true`,
		"no result":        `results=0`,
		"two results":      `results=2`,
		"two sessions":     `inits=2`,
		"no session":       `inits=0`,
		"foreign result":   `rsid=87654321-4321-4321-8321-cba987654321`,
		"invalid identity": `sid=not-a-session`,
		"null output":      `output=null`,
		"schema invalid":   `output='{"output":{"accepted":"yes"},"edits":[]}'`,
		"exit failure":     `exit 3`,
		"unscoped edit":    `output='{"output":{"accepted":true},"edits":[{"path":"other.go","content":"x"}]}'`,
	} {
		t.Run(name, func(t *testing.T) {
			c, config, l := claudeFixture(t, extra)
			if e := Run(context.Background(), c, config); e == nil {
				t.Fatal("unsafe provider turn accepted")
			}
			data, _ := os.ReadFile(filepath.Join(l.Worktree, "answer.go"))
			if string(data) != "original" {
				t.Fatal("refused turn changed task", string(data))
			}
			if _, e := os.Lstat(l.Handoff); !os.IsNotExist(e) {
				t.Fatal("refused turn published handoff", e)
			}
		})
	}
}

// FAE-RT-010, FAE-RT-009: a binding the Claude bridge cannot run as the
// instance configures it (no or relative credentials, an effort Claude Code
// lacks, an option-shaped model, another provider's binding) is a recorded
// binding refusal, made before the provider is launched; the engine fails
// it over to the role's next route.
func TestClaudePreflightRefusesBeforeLaunch(t *testing.T) {
	for _, tc := range []struct{ name, model, effort, provider, env string }{
		{"no credentials", "m", "", "claude", ""},
		{"relative config", "m", "", "claude", "CLAUDE_CONFIG_DIR=relative"},
		{"unknown effort", "m", "extreme", "claude", "CLAUDE_CODE_OAUTH_TOKEN=x"},
		{"minimal effort", "m", "minimal", "claude", "CLAUDE_CODE_OAUTH_TOKEN=x"},
		{"flag model", "--dangerously-skip-permissions", "", "claude", "CLAUDE_CODE_OAUTH_TOKEN=x"},
		{"foreign binding", "m", "", "codex", "CLAUDE_CODE_OAUTH_TOKEN=x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, config, l := claudeFixture(t, `touch executed`)
			for _, k := range claudeCredentialVariables {
				t.Setenv(k, "")
			}
			if k, v, ok := strings.Cut(tc.env, "="); ok {
				t.Setenv(k, v)
			}
			l.Binding = workflow.Binding{Provider: tc.provider, Model: tc.model, Effort: tc.effort}
			raw, _ := json.Marshal(l)
			os.WriteFile(config, raw, 0600)
			assertRefusal(t, Run(context.Background(), c, config), l, r.RefusedBinding)
			if providers, _ := filepath.Glob(filepath.Join(l.LogDirectory, "provider-*")); len(providers) != 0 {
				t.Fatal("provider launched despite refused preflight", providers)
			}
		})
	}
	ok := r.BridgeConfig{Launch: r.Launch{Binding: workflow.Binding{Provider: "claude", Model: "claude-opus-5-5", Effort: "xhigh"}}}
	if e := claudePreflight(ok, []string{"CLAUDE_CONFIG_DIR=/abs/config"}); e != nil {
		t.Fatal(e)
	}
}

func TestClaudePersistentSessionResumeAndRotation(t *testing.T) {
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "fixture-token")
	l := persistentLaunch(t)
	l.Binding = workflow.Binding{Provider: "claude", Model: "fixture"}
	root := sessionRoot(t)
	fake := filepath.Join(l.Worktree, "fake-claude")
	write := func(extra string) {
		if e := os.WriteFile(fake, []byte(claudeScript(extra+"\noutput='{\"output\":{\"accepted\":true},\"edits\":[]}'")), 0700); e != nil {
			t.Fatal(e)
		}
	}
	c := Config{Provider: "claude", Binary: fake, SessionRoot: root, Files: []string{"answer.go"}, Timeout: 5 * time.Second}
	turn := func(i int) (string, error) {
		l.DispatchID = "dispatch-" + string(rune('a'+i))
		l.Worktree = sessionRoot(t)
		gitRepository(t, l.Worktree)
		l.Handoff = filepath.Join(l.Worktree, "handoff.json")
		l.LogDirectory = l.Worktree
		config := filepath.Join(l.Worktree, "config.json")
		raw, _ := json.Marshal(l)
		os.WriteFile(config, raw, 0600)
		e := Run(context.Background(), c, config)
		dirs, _ := filepath.Glob(filepath.Join(root, "*", "cwd"))
		if len(dirs) != 1 {
			t.Fatal(dirs)
		}
		args, _ := os.ReadFile(filepath.Join(dirs[0], "args.txt"))
		os.Remove(filepath.Join(dirs[0], "args.txt"))
		return string(args), e
	}
	write("")
	args, e := turn(0)
	if e != nil || !strings.Contains(args, "--session-id=") || strings.Contains(args, "--resume") || strings.Contains(args, "--no-session-persistence") {
		t.Fatal(e, args)
	}
	audits, _ := filepath.Glob(filepath.Join(l.LogDirectory, "provider-*", "provider-session.json"))
	var first SessionAudit
	if data, _ := os.ReadFile(audits[0]); json.Unmarshal(data, &first) != nil || first.Resumed || !strings.Contains(args, "--session-id="+first.ThreadID+"\n") {
		t.Fatal(first, args)
	}
	args, e = turn(1)
	if e != nil || !strings.Contains(args, "--resume="+first.ThreadID+"\n") {
		t.Fatal(e, args)
	}
	// A provider that answers from another session is refused, and the
	// uncertain context is rotated rather than resumed again.
	write(`sid=87654321-4321-4321-8321-cba987654321`)
	if _, e = turn(2); e == nil || !strings.Contains(e.Error(), "different session") {
		t.Fatal(e)
	}
	write("")
	if args, e = turn(3); e != nil || strings.Contains(args, "--resume") || !strings.Contains(args, "--session-id=") {
		t.Fatal(e, args)
	}
}

func TestClaudeStreamChunkingAndBounds(t *testing.T) {
	records := `{"type":"system","subtype":"init","session_id":"12345678-1234-4234-8234-123456789abc","tools":["StructuredOutput"],"mcp_servers":[]}
not json
{"type":"result","subtype":"success","is_error":false,"session_id":"12345678-1234-4234-8234-123456789abc","structured_output":{"output":{},"edits":[]}}`
	s := &claudeStream{}
	for i := 0; i < len(records); i++ {
		if n, e := s.Write([]byte(records[i : i+1])); n != 1 || e != nil {
			t.Fatal(n, e)
		}
	}
	answer := filepath.Join(t.TempDir(), "answer.json")
	if id, e := s.finish("", answer); e != nil || id != "12345678-1234-4234-8234-123456789abc" {
		t.Fatal(id, e)
	}
	if data, _ := os.ReadFile(answer); string(data) != `{"output":{},"edits":[]}` {
		t.Fatal(string(data))
	}
	big := &claudeStream{}
	big.Write([]byte(`{"type":"assistant","text":"` + strings.Repeat("x", maxClaudeRecordBytes) + "\"}\n" + records + "\n"))
	if _, e := big.finish("", filepath.Join(t.TempDir(), "answer.json")); e == nil || !strings.Contains(e.Error(), "bound") {
		t.Fatal(e)
	}
	id, e := newSessionUUID()
	if e != nil || !threadIDPattern.MatchString(id) || id[14] != '4' {
		t.Fatal(id, e)
	}
}
