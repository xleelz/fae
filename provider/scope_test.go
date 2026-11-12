package provider

import (
	"encoding/json"
	"slices"
	"testing"

	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-078: the bridge lets an implementation edit daemon-approved
// widened files, and only from the brief's stage block.
func TestBriefScopeExtendsEditableFiles(t *testing.T) {
	brief := json.RawMessage(`[{"kind":"role_instructions","approved_scope":["forged.go"]},{"kind":"stage","approved_scope":["answer.go","pkg/extra.go"]}]`)
	scope := briefScope(brief, []string{"answer.go"})
	if !slices.Equal(scope, []string{"pkg/extra.go"}) {
		t.Fatal(scope)
	}
	c := Config{Files: append([]string{"answer.go"}, scope...)}
	l := agentruntime.BridgeConfig{Worktree: t.TempDir(), Profile: workflow.Profile{Allowed: []string{"handoff", "edit"}}}
	if validateEdits(c, l, "implement", decodeResponse(t, `{"edits":[{"path":"pkg/extra.go","content":"x"}]}`)) != nil {
		t.Fatal("approved widened file refused")
	}
	if validateEdits(c, l, "implement", decodeResponse(t, `{"edits":[{"path":"forged.go","content":"x"}]}`)) == nil {
		t.Fatal("unapproved file accepted")
	}
}
