package provider

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProviderStatedTurnStatsExactOrAbsent(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		cost       bool
	}{{"stated", `{"type":"result","subtype":"success","session_id":"provider-session","is_error":false,"num_turns":7,"duration_ms":1234,"total_cost_usd":0.42}`, true}, {"missing", `{"type":"result","subtype":"success"}`, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s := &claudeStream{}
			s.record([]byte(tc.line))
			dir := t.TempDir()
			writeProviderStats(dir, s)
			raw, err := os.ReadFile(filepath.Join(dir, "turn-stats.json"))
			if err != nil {
				t.Fatal(err)
			}
			var p domain.ProviderSessionStats
			if err = json.Unmarshal(raw, &p); err != nil {
				t.Fatal(err)
			}
			if tc.cost {
				if p.CostUSD == nil || *p.CostUSD != 0.42 || p.NumTurns == nil || *p.NumTurns != 7 || p.IsError == nil {
					t.Fatal(string(raw))
				}
			} else if p.CostUSD != nil || p.NumTurns != nil || p.IsError != nil {
				t.Fatal("invented provider readings", string(raw))
			}
		})
	}
}
func TestToolAuditBoundedAndRefusalsAlwaysEmittedWithoutContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.jsonl")
	tb := AgenticToolbox{Worktree: t.TempDir(), ToolAuditPath: path, Profile: workflow.Profile{Allowed: []string{"read"}}}
	for i := 0; i < 400; i++ {
		tb.recordToolEvent(AgenticToolCall{Tool: "read", Path: "a.go", Content: "secret-content"}, true)
	}
	_, _ = tb.Call(context.Background(), AgenticToolCall{Tool: "write", Path: "secret.txt", Content: "secret-content"})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) > 201 || len(lines) < 2 {
		t.Fatal("unbounded or empty audit", len(lines))
	}
	var last domain.ToolEvent
	if err = json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil || last.Allowed || last.Tool != "write" {
		t.Fatal(string(raw), err)
	}
	if strings.Contains(string(raw), "secret-content") {
		t.Fatal("file content leaked")
	}
}

func TestClaudeNativeToolUseEmitsBoundedContentFreeMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "calls.jsonl")
	tb := &AgenticToolbox{Worktree: t.TempDir(), ToolAuditPath: path, Profile: workflow.Profile{Allowed: []string{"read"}}}
	stream := &claudeStream{toolbox: tb, allowedTools: map[string]bool{"Read": true, "Write": false}}
	record := []byte(`{"type":"assistant","message":{"id":"message","content":[{"type":"tool_use","id":"call1","name":"Read","input":{"file_path":"a.go","content":"secret-content"}},{"type":"tool_use","id":"call2","name":"Read","input":{"file_path":"b.go"}},{"type":"tool_use","id":"call3","name":"Read","input":{"file_path":"c.go"}}]}}`)
	stream.record(record)
	stream.record(record)
	tb.toolEventCount, tb.toolEventRate = 200, 20
	tb.Guard = &ReadGuard{Worktree: tb.Worktree, DenyPaths: []string{"private.go"}, LogDirectory: t.TempDir()}
	stream.record([]byte(`{"type":"assistant","message":{"id":"denied-read","content":[{"type":"tool_use","id":"call4","name":"Read","input":{"file_path":"private.go","content":"secret-content"}}]}}`))
	stream.record([]byte(`{"type":"assistant","message":{"id":"denied","content":[{"type":"tool_use","id":"call5","name":"Write","input":{"file_path":"secret.txt","content":"secret-content"}}]}}`))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-content") || strings.Contains(string(raw), "tool_input") {
		t.Fatal("content leaked", string(raw))
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 5 {
		t.Fatal("native tool ID replay was not deduplicated", string(raw))
	}
	var deniedRead domain.ToolEvent
	_ = json.Unmarshal([]byte(lines[3]), &deniedRead)
	if deniedRead.Allowed || deniedRead.Tool != "read" {
		t.Fatal("rate limit swallowed refused native read", string(raw))
	}
	var first, last domain.ToolEvent
	_ = json.Unmarshal([]byte(lines[0]), &first)
	_ = json.Unmarshal([]byte(lines[4]), &last)
	if first.Tool != "read" || !first.Allowed || len(first.Paths) != 1 || first.Paths[0] != "a.go" || last.Tool != "write" || last.Allowed {
		t.Fatal(string(raw))
	}
}
