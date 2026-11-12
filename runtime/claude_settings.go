package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/xleelz/fae/workflow"
)

// TurnBudgetExhausted is the typed outcome class recorded when an agentic
// turn exhausts its configured wall_seconds or max_assistant_turns budget
// (FAE-RT-015).
const TurnBudgetExhausted = "turn-budget-exhausted"

// TurnCheckpoint records the latest durable session and worktree tree snapshot
// of an agentic turn (FAE-RT-019).
type TurnCheckpoint struct {
	ProviderSession string `json:"provider_session"`
	WorktreeTree    string `json:"worktree_tree"`
	AssistantTurns  int    `json:"assistant_turns,omitempty"`
	UpdatedAt       string `json:"updated_at,omitempty"`
}

// ReadTurnCheckpoint reads checkpoint.json from a session's log directory if
// present (FAE-RT-019).
func ReadTurnCheckpoint(logDir string) (*TurnCheckpoint, error) {
	if logDir == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(filepath.Join(logDir, "checkpoint.json"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cp TurnCheckpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// IsTurnBudgetExhaustedClass reports whether class is TurnBudgetExhausted.
func IsTurnBudgetExhaustedClass(class string) bool {
	switch class {
	case TurnBudgetExhausted:
		return true
	}
	return false
}

// AgenticShellAllowed reports whether profile grants the shell capability.
func AgenticShellAllowed(p workflow.Profile) bool {
	return slices.Contains(p.Allowed, "shell") && !slices.Contains(p.Denied, "shell")
}

// AllowedAgenticTools returns the deduplicated Fae tool capabilities granted by
// profile. Write tools ("edit", "write") are included only when changeProducer
// is true (FAE-RT-015).
func AllowedAgenticTools(p workflow.Profile, changeProducer bool) []string {
	out := []string{}
	for _, c := range p.Allowed {
		if slices.Contains(p.Denied, c) {
			continue
		}
		if (c == "edit" || c == "write") && !changeProducer {
			continue
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// ClaudeNativeTools maps Fae's capability vocabulary (FAE-RT-015) to the
// provider's native tool surface names for an agentic session.
func ClaudeNativeTools(p workflow.Profile, changeProducer bool) []string {
	tools := []string{"StructuredOutput"}
	add := func(names ...string) {
		for _, n := range names {
			if !slices.Contains(tools, n) {
				tools = append(tools, n)
			}
		}
	}
	for _, c := range AllowedAgenticTools(p, changeProducer) {
		switch c {
		case "read":
			add("Read")
		case "list":
			add("Glob")
		case "search_glob":
			add("Glob")
		case "search_regex":
			add("Grep")
		case "edit":
			add("Edit")
		case "write":
			add("Write")
		case "shell":
			add("Bash")
		case "handoff":
			add("StructuredOutput")
		}
	}
	return tools
}

type claudeHookCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

type claudeHookMatcher struct {
	Matcher string              `json:"matcher"`
	Hooks   []claudeHookCommand `json:"hooks"`
}

type claudeSettingsDoc struct {
	Hooks struct {
		PreToolUse []claudeHookMatcher `json:"PreToolUse"`
	} `json:"hooks"`
}

// WriteClaudePreToolHook writes a Claude Code settings.json file in dir that
// registers the Fae PreToolUse hook for Bash/shell invocations (FAE-HOST-011,
// FAE-RT-015) and returns its absolute path.
func WriteClaudePreToolHook(dir, brokerBin string) (string, error) {
	if !filepath.IsAbs(dir) {
		return "", errors.New("claude settings directory must be absolute")
	}
	shimDir := filepath.Join(dir, "command-shims")
	escapedShim := "'" + strings.ReplaceAll(shimDir, "'", `'\''`) + "'"
	hookCmd := "fae heavy-op hook --shim-dir " + escapedShim
	if brokerBin != "" {
		hookCmd = "'" + strings.ReplaceAll(brokerBin, "'", `'\''`) + "' heavy-op hook --shim-dir " + escapedShim
	}
	var doc claudeSettingsDoc
	doc.Hooks.PreToolUse = []claudeHookMatcher{
		{
			Matcher: "Bash",
			Hooks: []claudeHookCommand{
				{
					Type:    "command",
					Command: hookCmd,
				},
			},
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "claude-settings.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		return "", err
	}
	return path, nil
}

// VerifyAgenticShellLaunch verifies that the role shim directory is first on
// PATH in env and that settingsPath registers the PreToolUse hook before an
// agentic session with shell is launched (FAE-HOST-011, FAE-RT-015).
func VerifyAgenticShellLaunch(env []string, settingsPath string) error {
	pathVal := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			pathVal = v
		}
	}
	parts := filepath.SplitList(pathVal)
	if len(parts) == 0 || !filepath.IsAbs(parts[0]) || filepath.Base(parts[0]) != "command-shims" {
		return errors.New("agentic session with shell requires role command shims first on PATH")
	}
	for _, tool := range []string{"go", "git", "gofmt"} {
		st, err := os.Stat(filepath.Join(parts[0], tool))
		if err != nil || !st.Mode().IsRegular() || st.Mode()&0111 == 0 {
			return fmt.Errorf("agentic session with shell missing executable role shim %s in %s", tool, parts[0])
		}
	}
	if settingsPath == "" || !filepath.IsAbs(settingsPath) {
		return errors.New("agentic session with shell requires registered PreToolUse hook")
	}
	st, err := os.Lstat(settingsPath)
	if err != nil || !st.Mode().IsRegular() {
		return errors.New("agentic session with shell requires regular PreToolUse hook settings file")
	}
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		return err
	}
	var doc claudeSettingsDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("invalid PreToolUse hook settings: %w", err)
	}
	registered := false
	for _, m := range doc.Hooks.PreToolUse {
		if m.Matcher == "Bash" || m.Matcher == "*" || m.Matcher == "shell" {
			for _, h := range m.Hooks {
				if h.Type == "command" && strings.Contains(h.Command, "heavy-op hook") && strings.Contains(h.Command, "--shim-dir") {
					registered = true
				}
			}
		}
	}
	if !registered {
		return errors.New("agentic session with shell requires PreToolUse hook registered for Bash")
	}
	return nil
}
