package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/processowner"
	r "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// refusalRecord reads the turn's refusal record, or returns nil without one.
func refusalRecord(t *testing.T, l r.BridgeConfig) map[string]string {
	t.Helper()
	raw, e := os.ReadFile(filepath.Join(l.LogDirectory, r.BridgeRefusalFile))
	if os.IsNotExist(e) {
		return nil
	}
	var record map[string]string
	if e != nil || json.Unmarshal(raw, &record) != nil {
		t.Fatal("unreadable refusal record", e, string(raw))
	}
	return record
}

// assertRefusal checks a refused turn: its class, the protocol exit status,
// a record naming the turn, and an untouched worktree with no handoff.
func assertRefusal(t *testing.T, e error, l r.BridgeConfig, class string) {
	t.Helper()
	var refusal *Refusal
	if !errors.As(e, &refusal) || refusal.Class != class || ExitCode(e) != r.BridgeRefusalExit {
		t.Fatalf("want a recorded %s refusal, got %v (exit %d)", class, e, ExitCode(e))
	}
	record := refusalRecord(t, l)
	if record["protocol"] != "fae-agent-v1" || record["class"] != class || record["dispatch_id"] != l.DispatchID || record["session_id"] != l.SessionID || record["task_id"] != l.TaskID || record["detail"] != e.Error() {
		t.Fatal("refusal record", record, e)
	}
	if got := readFile(t, l.Worktree, "answer.go"); got != "original" {
		t.Fatal("refused turn changed the task", got)
	}
	if _, err := os.Lstat(l.Handoff); !os.IsNotExist(err) {
		t.Fatal("refused turn published a handoff", err)
	}
}

func replacementEdit(path, old, new string) string {
	raw, _ := json.Marshal(map[string]any{"path": path, "content": nil, "replacements": []map[string]string{{"old": old, "new": new}}})
	return string(raw)
}

// FAE-WF-034: a completed provider turn whose answer the bridge refuses (the
// schema, the edit shape, an unmatched or ambiguous replacement, the RED and
// scope rules) is a recorded output refusal, and nothing is written.
func TestBridgeRefusesProviderOutputWithRecord(t *testing.T) {
	scoped := []any{taskScope([]string{"answer.go"}, "")}
	red := []any{taskScope([]string{"answer.go", "a_test.go", "b_test.go"}, "")}
	for _, tc := range []struct {
		name, stage, answer string
		blocks              []any
	}{
		{"schema", "implement", `{"output":{"accepted":"yes"},"edits":[]}`, scoped},
		{"unknown answer field", "implement", `{"output":{"accepted":true},"edits":[],"extra":1}`, scoped},
		{"not JSON", "implement", `{"output":`, scoped},
		{"both forms", "implement", `{"output":{"accepted":true},"edits":[{"path":"answer.go","content":"x","replacements":[{"old":"original","new":"y"}]}]}`, scoped},
		{"neither form", "implement", `{"output":{"accepted":true},"edits":[{"path":"answer.go","content":null,"replacements":[]}]}`, scoped},
		{"unmatched replacement", "implement", `{"output":{"accepted":true},"edits":[` + replacementEdit("answer.go", "missing text", "x") + `]}`, scoped},
		{"ambiguous replacement", "implement", `{"output":{"accepted":true},"edits":[` + replacementEdit("answer.go", "i", "x") + `]}`, scoped},
		{"unscoped edit", "implement", `{"output":{"accepted":true},"edits":[{"path":"other.go","content":"x","replacements":[]}]}`, scoped},
		{"two RED files", "test-first", `{"output":{"accepted":true},"edits":[{"path":"a_test.go","content":"package a\n","replacements":[]},{"path":"b_test.go","content":"package a\n","replacements":[]}]}`, red},
		{"RED non-test file", "test-first", `{"output":{"accepted":true},"edits":[{"path":"answer.go","content":"x","replacements":[]}]}`, red},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, config, l := turnFixture(t, tc.stage, tc.blocks, tc.answer)
			c.Files = nil
			e := Run(context.Background(), c, config)
			assertRefusal(t, e, l, r.RefusedOutput)
			for _, name := range []string{"other.go", "a_test.go", "b_test.go"} {
				if _, err := os.Stat(filepath.Join(l.Worktree, name)); !os.IsNotExist(err) {
					t.Fatal("refused turn wrote", name)
				}
			}
		})
	}
	// A provider that acknowledged the prompt and exits zero without an
	// answer returned nothing usable. (One that wrote nothing at all never
	// acknowledged the prompt: TestBridgeTypesAProviderThatNeverAcknowledgedThePrompt.)
	c, config, l := providerFixture(t, `printf '%s\n' '{"type":"thread.started","thread_id":"t"}'; exit 0`)
	assertRefusal(t, Run(context.Background(), c, config), l, r.RefusedOutput)
}

// FAE-RT-004/RT-009: a turn refused before the provider is launched (the
// task's scope or files, the brief, a local repository listing) is a recorded
// local refusal, and the provider never runs.
func TestBridgeRefusesBeforeProviderWithRecord(t *testing.T) {
	big := []string{"biga.go", "bigb.go", "bigc.go", "bigd.go", "bige.go"}
	for _, tc := range []struct {
		name   string
		blocks []any
		setup  func(t *testing.T, c *Config, l r.BridgeConfig)
	}{
		{"files over the bound", []any{taskScope(big, "")}, func(t *testing.T, _ *Config, l r.BridgeConfig) {
			for _, name := range big {
				writeFile(t, l.Worktree, name, strings.Repeat("x", 1000*1000))
			}
		}},
		{"pinned test file differs", []any{taskScope([]string{"answer.go", "answer_test.go"}, "answer_test.go")}, func(_ *testing.T, c *Config, _ r.BridgeConfig) {
			c.Files, c.TestFile = []string{"answer.go", "other_test.go"}, "other_test.go"
		}},
		{"unsafe brief path", []any{taskScope([]string{"answer.go", "../escape.go"}, "")}, nil},
		{"test file outside the block", []any{taskScope([]string{"answer.go"}, "answer_test.go")}, nil},
		{"task without a pinned scope", []any{taskScope([]string{"answer.go"}, "")}, func(_ *testing.T, c *Config, _ r.BridgeConfig) {
			c.TaskScopesJSON = `{"other":{"files":["answer.go"],"test_file":"answer_test.go"}}`
		}},
		{"repository listing fails", []any{taskScope([]string{"answer.go"}, "")}, func(t *testing.T, _ *Config, l r.BridgeConfig) {
			if e := os.RemoveAll(filepath.Join(l.Worktree, ".git")); e != nil {
				t.Fatal(e)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, config, l := turnFixture(t, "implement", tc.blocks, acceptedNoEdits)
			c.Files = nil
			if tc.setup != nil {
				tc.setup(t, &c, l)
			}
			e := Run(context.Background(), c, config)
			assertRefusal(t, e, l, r.RefusedBeforeProvider)
			if launched(l) {
				t.Fatal("provider launched for a turn refused before it")
			}
		})
	}
}

// FAE-RT-010: the Codex bridge refuses a binding it does not serve (the
// Claude half of a mixed chain on a Codex-only instance) or cannot run (no
// model), before the provider runs, as a binding refusal.
func TestCodexBridgeRefusesBindingsItCannotRun(t *testing.T) {
	for name, binding := range map[string]workflow.Binding{
		"claude binding": {Provider: "claude", Model: "claude-opus-5-5", Effort: "medium"},
		"no model":       {Provider: "codex"},
	} {
		t.Run(name, func(t *testing.T) {
			c, config, l := providerFixture(t, "")
			l.Binding = binding
			raw, _ := json.Marshal(l)
			if e := os.WriteFile(config, raw, 0600); e != nil {
				t.Fatal(e)
			}
			assertRefusal(t, Run(context.Background(), c, config), l, r.RefusedBinding)
			if launched(l) {
				t.Fatal("provider launched for a refused binding")
			}
		})
	}
}

// FAE-RT-004: a provider that provably never took the turn is a recorded
// ProviderNotStarted: an absolute executable that does not exist or cannot
// run, and a Codex or Claude Code CLI that wrote nothing to its event stream
// (before Claude's system/init record) and left no answer, however it ended:
// it exited on its own, with a failure or not; the start bound stopped it;
// or the bridge's own timeout did. The task is untouched. A provider that
// wrote any output before it failed, or before the bridge's timeout stopped
// it, is provider-failed instead
// (TestBridgeTypesAProviderThatFailedWithoutAnAnswer).
func TestBridgeTypesAProviderThatNeverAcknowledgedThePrompt(t *testing.T) {
	silentUntilTimeout := func(c Config, config string, l r.BridgeConfig) (Config, string, r.BridgeConfig) {
		c.Timeout = 300 * time.Millisecond
		return c, config, l
	}
	silentPastStartBound := func(c Config, config string, l r.BridgeConfig) (Config, string, r.BridgeConfig) {
		c.StartTimeout, c.Timeout = 300*time.Millisecond, 30*time.Second
		return c, config, l
	}
	for _, tc := range []struct {
		name, detail string
		fixture      func(t *testing.T) (Config, string, r.BridgeConfig)
	}{
		{"codex exits before any event", "provider exited before acknowledging the prompt", func(t *testing.T) (Config, string, r.BridgeConfig) {
			return providerFixture(t, "exit 3")
		}},
		{"claude exits before its init record", "provider exited before acknowledging the prompt", func(t *testing.T) (Config, string, r.BridgeConfig) {
			return claudeFixture(t, "exit 3")
		}},
		{"codex exits cleanly without an event or an answer", "provider exited without acknowledging the prompt or answering", func(t *testing.T) (Config, string, r.BridgeConfig) {
			return providerFixture(t, "exit 0")
		}},
		{"claude exits cleanly before its init record", "provider exited without acknowledging the prompt or answering", func(t *testing.T) (Config, string, r.BridgeConfig) {
			return claudeFixture(t, "exit 0")
		}},
		{"codex silent until the bridge timeout", "provider did not acknowledge the prompt before the bridge timeout of 300ms", func(t *testing.T) (Config, string, r.BridgeConfig) {
			return silentUntilTimeout(providerFixture(t, "exec sleep 20"))
		}},
		{"claude silent until the bridge timeout", "provider did not acknowledge the prompt before the bridge timeout of 300ms", func(t *testing.T) (Config, string, r.BridgeConfig) {
			return silentUntilTimeout(claudeFixture(t, "exec sleep 20"))
		}},
		{"codex silent past the start bound", "provider did not acknowledge the prompt within the start bound of 300ms", func(t *testing.T) (Config, string, r.BridgeConfig) {
			return silentPastStartBound(providerFixture(t, "exec sleep 20"))
		}},
		{"claude silent past the start bound", "provider did not acknowledge the prompt within the start bound of 300ms", func(t *testing.T) (Config, string, r.BridgeConfig) {
			return silentPastStartBound(claudeFixture(t, "exec sleep 20"))
		}},
		{"absolute executable missing", "provider executable did not start", func(t *testing.T) (Config, string, r.BridgeConfig) {
			c, config, l := providerFixture(t, "")
			c.Binary = filepath.Join(t.TempDir(), "missing-provider")
			return c, config, l
		}},
		{"executable not runnable", "provider executable did not start", func(t *testing.T) (Config, string, r.BridgeConfig) {
			c, config, l := providerFixture(t, "")
			if e := os.Chmod(c.Binary, 0600); e != nil {
				t.Fatal(e)
			}
			return c, config, l
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, config, l := tc.fixture(t)
			began := time.Now()
			e := Run(context.Background(), c, config)
			assertRefusal(t, e, l, r.ProviderNotStarted)
			if !strings.Contains(e.Error(), tc.detail) {
				t.Fatalf("detail %q lacks %q", e, tc.detail)
			}
			// Each silent provider is stopped at its bound, not left to run.
			if took := time.Since(began); took > 10*time.Second {
				t.Fatalf("refusal took %s", took)
			}
		})
	}
}

// FAE-RT-004 (Appendix A, agent start 180 s per attempt): the start bound
// stops a provider that has not acknowledged the prompt in time together with
// the processes it started, here a child that holds its event stream open,
// and types it provider-not-started; and it never stops a provider that
// acknowledged the prompt in time, however long that provider then takes.
func TestBridgeStartBoundStopsOnlyAnUnacknowledgedProvider(t *testing.T) {
	t.Run("silent provider and its child are stopped", func(t *testing.T) {
		c, config, l := providerFixture(t, `sleep 20 & echo $! > "$0.child"; wait`)
		c.StartTimeout, c.Timeout = 300*time.Millisecond, 30*time.Second
		began := time.Now()
		e := Run(context.Background(), c, config)
		took := time.Since(began)
		assertRefusal(t, e, l, r.ProviderNotStarted)
		if took > 10*time.Second {
			t.Fatalf("the start bound took %s to stop the provider", took)
		}
		raw, err := os.ReadFile(c.Binary + ".child")
		if err != nil {
			t.Fatal(err)
		}
		child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			rows, err := processowner.CensusOf([]int{child})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) == 0 || rows[0].Defunct() {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("the provider's child %d outlived the start bound: %+v", child, rows)
			}
		}
	})
	t.Run("acknowledged provider runs past the bound", func(t *testing.T) {
		c, config, l := providerFixture(t, `printf '%s\n' '{"type":"thread.started","thread_id":"t"}'; sleep 1`)
		c.StartTimeout = 300 * time.Millisecond
		if e := Run(context.Background(), c, config); e != nil {
			t.Fatal(e)
		}
		if got := readFile(t, l.Worktree, "answer.go"); got != "new content" {
			t.Fatal("acknowledged turn not applied", got)
		}
	})
	t.Run("a negative bound is refused as configuration", func(t *testing.T) {
		c, config, l := providerFixture(t, "")
		c.StartTimeout = -time.Second
		assertRefusal(t, Run(context.Background(), c, config), l, r.RefusedBinding)
	})
}

// Failures that are neither stay untyped (exit 1, no record): a provider
// whose bridge was cancelled, another writer's handoff, and a refusal whose
// record cannot be written. (A failed edit write is typed edits-not-applied,
// with the worktree restored:
// TestBridgeEditBatchWhoseThirdWriteFailsLeavesTheTreeUnchanged.)
func TestBridgeUntypedFailuresKeepExitOne(t *testing.T) {
	untyped := func(t *testing.T, e error, l r.BridgeConfig) {
		t.Helper()
		var refusal *Refusal
		if e == nil || errors.As(e, &refusal) || ExitCode(e) != 1 {
			t.Fatalf("want an untyped failure, got %v (exit %d)", e, ExitCode(e))
		}
		if record := refusalRecord(t, l); record != nil && record["detail"] != "occupied" {
			t.Fatal("untyped failure recorded a refusal", record)
		}
	}
	t.Run("bridge cancelled", func(t *testing.T) {
		c, config, l := providerFixture(t, `printf '%s\n' '{"type":"thread.started","thread_id":"t"}'; exec sleep 5`)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		time.AfterFunc(300*time.Millisecond, cancel)
		untyped(t, Run(ctx, c, config), l)
	})
	t.Run("existing handoff", func(t *testing.T) {
		c, config, l := providerFixture(t, "")
		if e := os.WriteFile(l.Handoff, []byte("old"), 0600); e != nil {
			t.Fatal(e)
		}
		untyped(t, Run(context.Background(), c, config), l)
	})
	t.Run("record cannot be written", func(t *testing.T) {
		c, config, l := turnFixture(t, "implement", []any{taskScope([]string{"answer.go"}, "")}, `{"output":{"accepted":"yes"},"edits":[]}`)
		c.Files = nil
		if e := os.WriteFile(filepath.Join(l.LogDirectory, r.BridgeRefusalFile), []byte(`{"detail":"occupied"}`), 0600); e != nil {
			t.Fatal(e)
		}
		untyped(t, Run(context.Background(), c, config), l)
		if record := refusalRecord(t, l); record["detail"] != "occupied" {
			t.Fatal("existing record replaced", record)
		}
	})
	if ExitCode(nil) != 0 || ExitCode(errors.New("x")) != 1 || ExitCode(&Refusal{Class: r.RefusedOutput, Err: errors.New("unrecorded")}) != 1 {
		t.Fatal("exit status mapping")
	}
}

// A Claude Code turn whose session and tool surface are as requested but
// whose result is unusable is refused output; a session or tool-surface
// failure stays untyped, so the turn still parks for the operator.
func TestClaudeUnusableResultIsRefusedOutput(t *testing.T) {
	for name, tc := range map[string]struct {
		extra string
		typed bool
	}{
		"schema invalid": {`output='{"output":{"accepted":"yes"},"edits":[]}'`, true},
		"null output":    {`output=null`, true},
		"error result":   {`subtype=error_during_execution; is_error=true`, true},
		"no result":      {`results=0`, true},
		"two results":    {`results=2`, true},
		"unscoped edit":  {`output='{"output":{"accepted":true},"edits":[{"path":"other.go","content":"x","replacements":[]}]}'`, true},
		"command tool":   {`tools='["StructuredOutput","Bash"]'`, false},
		"mcp server":     {`mcp='[{"name":"remote","status":"connected"}]'`, false},
		"two sessions":   {`inits=2`, false},
		"foreign result": {`rsid=87654321-4321-4321-8321-cba987654321`, false},
	} {
		t.Run(name, func(t *testing.T) {
			c, config, l := claudeFixture(t, tc.extra)
			e := Run(context.Background(), c, config)
			if tc.typed {
				assertRefusal(t, e, l, r.RefusedOutput)
				return
			}
			if e == nil || ExitCode(e) != 1 || refusalRecord(t, l) != nil {
				t.Fatalf("want an untyped refusal, got %v (exit %d)", e, ExitCode(e))
			}
		})
	}
}
