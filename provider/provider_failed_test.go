package provider

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	r "github.com/xleelz/fae/runtime"
)

// providerFailedClass is runtime.ProviderFailed, spelled out so the test pins
// the protocol's wire value.
const providerFailedClass = "provider-failed"

// claudeFailedScript announces one Claude Code session with the given tool
// and MCP surface and exits 3 with no result.
func claudeFailedScript(tools, mcp string) string {
	return `#!/bin/sh
cat > /dev/null
printf '%s\n' '{"type":"system","subtype":"init","session_id":"12345678-1234-4234-8234-123456789abc","tools":` + tools + `,"mcp_servers":` + mcp + `}'
echo 'fatal: stream closed' >&2
exit 3
`
}

// FAE-WF-034: a provider that acknowledged the prompt and then ended without
// an answer, failing on its own (Codex after its first event, Claude Code
// after a system/init record announcing only the allowed surface) or stopped
// by the bridge's own timeout after it wrote something, is a recorded
// provider-failed refusal: exit 65, the task untouched, no handoff, and a
// detail that names no per-turn path, so a repeated failure counts toward
// SameGate. (One the timeout stopped before it wrote anything never
// acknowledged the prompt: TestBridgeTypesAProviderThatNeverAcknowledgedThePrompt.) A Claude Code session that announced a tool or an MCP
// server beyond the allowed surface, or two sessions, or wrote a record over
// the bound (which hides what it announced), stays untyped: such a session
// may have had a way to write.
func TestBridgeTypesAProviderThatFailedWithoutAnAnswer(t *testing.T) {
	for _, tc := range []struct {
		name, detail string
		fixture      func(t *testing.T) (Config, string, r.BridgeConfig)
	}{
		{"codex failed after an event", "provider exited with a failure after acknowledging the prompt: exit status 3", func(t *testing.T) (Config, string, r.BridgeConfig) {
			return providerFixture(t, `printf '%s\n' '{"type":"thread.started","thread_id":"t"}'; exit 3`)
		}},
		{"codex wrote text and failed", "provider exited with a failure after acknowledging the prompt: exit status 2", func(t *testing.T) (Config, string, r.BridgeConfig) {
			return providerFixture(t, `echo 'Error: stream disconnected'; exit 2`)
		}},
		{"claude failed after its init record", "provider exited with a failure after acknowledging the prompt: exit status 3", func(t *testing.T) (Config, string, r.BridgeConfig) {
			c, config, l := claudeFixture(t, "")
			if e := os.WriteFile(c.Binary, []byte(claudeFailedScript(`["StructuredOutput"]`, `[]`)), 0700); e != nil {
				t.Fatal(e)
			}
			return c, config, l
		}},
		{"stopped by the bridge timeout after an event", "provider timed out after 300ms", func(t *testing.T) (Config, string, r.BridgeConfig) {
			c, config, l := providerFixture(t, `printf '%s\n' '{"type":"thread.started","thread_id":"t"}'; exec sleep 5`)
			c.Timeout = 300 * time.Millisecond
			return c, config, l
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, config, l := tc.fixture(t)
			e := Run(context.Background(), c, config)
			assertRefusal(t, e, l, providerFailedClass)
			if !strings.HasPrefix(e.Error(), tc.detail) || strings.Contains(e.Error(), l.LogDirectory) {
				t.Fatalf("detail %q: want %q and no per-turn path", e, tc.detail)
			}
		})
	}
	for _, tc := range []struct{ name, tools, mcp string }{
		{"claude announced a command tool", `["StructuredOutput","Bash"]`, `[]`},
		{"claude loaded an MCP server", `["StructuredOutput"]`, `[{"name":"remote","status":"connected"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, config, l := claudeFixture(t, "")
			if e := os.WriteFile(c.Binary, []byte(claudeFailedScript(tc.tools, tc.mcp)), 0700); e != nil {
				t.Fatal(e)
			}
			e := Run(context.Background(), c, config)
			var refusal *Refusal
			if e == nil || errors.As(e, &refusal) || ExitCode(e) != 1 || refusalRecord(t, l) != nil {
				t.Fatalf("want an untyped failure, got %v (exit %d)", e, ExitCode(e))
			}
		})
	}
	t.Run("claude record over the bound", func(t *testing.T) {
		c, config, l := claudeFixture(t, "")
		script := strings.Replace(claudeFailedScript(`["StructuredOutput"]`, `[]`), "echo 'fatal", "head -c 4200000 /dev/zero | tr '\\0' x; echo\necho 'fatal", 1)
		if e := os.WriteFile(c.Binary, []byte(script), 0700); e != nil {
			t.Fatal(e)
		}
		e := Run(context.Background(), c, config)
		var refusal *Refusal
		if e == nil || errors.As(e, &refusal) || ExitCode(e) != 1 || refusalRecord(t, l) != nil {
			t.Fatalf("want an untyped failure, got %v (exit %d)", e, ExitCode(e))
		}
	})
	t.Run("claude announced two sessions", func(t *testing.T) {
		c, config, l := claudeFixture(t, "")
		script := strings.Replace(claudeFailedScript(`["StructuredOutput"]`, `[]`), "echo 'fatal", `printf '%s\n' '{"type":"system","subtype":"init","session_id":"87654321-4321-4321-8321-cba987654321","tools":[],"mcp_servers":[]}'`+"\necho 'fatal", 1)
		if e := os.WriteFile(c.Binary, []byte(script), 0700); e != nil {
			t.Fatal(e)
		}
		e := Run(context.Background(), c, config)
		var refusal *Refusal
		if e == nil || errors.As(e, &refusal) || ExitCode(e) != 1 || refusalRecord(t, l) != nil {
			t.Fatalf("want an untyped failure, got %v (exit %d)", e, ExitCode(e))
		}
	})
}

func TestBridgeWaitDelayBoundsLingeringChildStdout(t *testing.T) {
	c, config, l := turnFixture(t, "implement", []any{taskScope([]string{"answer.go"}, "")}, `{"output":{"accepted":true},"edits":[]}`)
	c.Files = nil
	c.WaitDelay = 80 * time.Millisecond
	raw, err := os.ReadFile(c.Binary)
	if err != nil {
		t.Fatal(err)
	}
	// Spawn a background child that inherits stdout/stderr and outlives the
	// provider exit; WaitDelay bounds cmd.Wait and clears exec.ErrWaitDelay.
	script := string(raw) + "\n(sleep 5) &\nexit 0\n"
	if err := os.WriteFile(c.Binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := Run(context.Background(), c, config); err != nil {
		t.Fatalf("Run failed on ErrWaitDelay: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("WaitDelay did not bound lingering child stdout: %v", elapsed)
	}
	if _, err := os.Stat(l.Handoff); err != nil {
		t.Fatalf("missing handoff after WaitDelay exit: %v", err)
	}
}
