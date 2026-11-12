package provider

import (
	"context"
	"errors"
	"fmt"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAssistantMessageBudgetCountsMainIDsOnce(t *testing.T) {
	killed := 0
	s := claudeStream{maxAssistantTurns: 5, onBudgetExhausted: func() { killed++ }}
	s.Write([]byte(`{"type":"system","subtype":"init","session_id":"main"}` + "\n"))
	for i := 1; i <= 5; i++ {
		for block := 0; block < 3; block++ {
			s.Write([]byte(fmt.Sprintf("{\"type\":\"assistant\",\"session_id\":\"main\",\"message\":{\"id\":\"m%d\"}}\n", i)))
		}
	}
	s.Write([]byte("{\"type\":\"assistant\",\"session_id\":\"main\",\"parent_tool_use_id\":\"subtask\",\"message\":{\"id\":\"sub\"}}\n"))
	s.Write([]byte("{\"type\":\"assistant\",\"session_id\":\"side\",\"message\":{\"id\":\"side\"}}\n"))
	if s.turns() != 5 || s.exhausted() || killed != 0 {
		t.Fatal(s.turns(), s.exhausted(), killed)
	}
	s.Write([]byte("{\"type\":\"result\",\"session_id\":\"main\",\"subtype\":\"success\",\"structured_output\":{}}\n"))
	s.Write([]byte("{\"type\":\"assistant\",\"message\":{\"id\":\"late\"}}\n"))
	if s.turns() != 5 || s.exhausted() {
		t.Fatal("successful at-bound result reclassified")
	}
	fresh := claudeStream{maxAssistantTurns: 5, onBudgetExhausted: func() { killed++ }}
	for i := 0; i < 6; i++ {
		fresh.Write([]byte(fmt.Sprintf("{\"type\":\"assistant\",\"message\":{\"id\":\"m%d\"}}\n", i)))
	}
	if !fresh.exhausted() || killed != 1 {
		t.Fatal("sixth distinct message did not stop provider", killed)
	}
}

func TestSixthDistinctMessageStopsProviderTreeWithinBound(t *testing.T) {
	c, config, l := turnFixture(t, "implement", []any{taskScope([]string{"answer.go"}, "")}, acceptedNoEdits)
	c.Provider = "claude"
	c.Binary = filepath.Join(t.TempDir(), "provider")
	c.SessionRoot = filepath.Join(t.TempDir(), "sessions")
	l.Binding.Provider = "claude"
	l.Binding.Model = "claude-sonnet-4-6"
	l.WorkflowID = "wf"
	l.WorkflowRevision = "rev"
	l.Role = "writer"
	l.Session = "agentic"
	l.SessionPolicy = "task-bound"
	l.ChangeProducer = true
	l.Profile.Allowed = []string{"read", "handoff"}
	l.TurnBudget = &workflow.TurnBudgetSpec{MaxAssistantTurns: 5}
	writeLaunch(t, config, l)
	marker := filepath.Join(t.TempDir(), "survived")
	script := `#!/bin/sh
sid=""
for arg in "$@"; do case "$arg" in --session-id=*) sid="${arg#--session-id=}";; esac; done
printf '{"type":"system","subtype":"init","session_id":"%s","tools":["StructuredOutput","Read"],"mcp_servers":[]}\n' "$sid"
(sleep 2; printf escaped > '` + marker + `') &
i=1
while [ "$i" -le 6 ]; do printf '{"type":"assistant","session_id":"%s","message":{"id":"m%s"}}\n' "$sid" "$i"; i=$((i+1)); done
sleep 30
`
	if err := os.WriteFile(c.Binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-credential")
	started := time.Now()
	err := Run(context.Background(), c, config)
	var refusal *Refusal
	if !errors.As(err, &refusal) || refusal.Class != agentruntime.TurnBudgetExhausted {
		t.Fatal(err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("provider exceeded kill bound", time.Since(started))
	}
	time.Sleep(2100 * time.Millisecond)
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("provider descendant survived", err)
	}
}
