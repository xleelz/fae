package provider

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	agentruntime "github.com/xleelz/fae/runtime"
)

// claudeTools is the complete tool surface a Claude Code turn may announce.
// StructuredOutput is how --json-schema results are returned; it cannot read,
// write or execute anything.
var claudeTools = map[string]bool{"StructuredOutput": true}

var claudeEfforts = map[string]bool{"": true, "low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// Credentials must be explicit in the pinned target environment; the runtime
// gives each session a fresh HOME, so ambient logins are never found.
var claudeCredentialVariables = []string{"CLAUDE_CODE_OAUTH_TOKEN", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"}

// Bounds one stream-json record. A terminal result holds the schema output
// twice (text and structured), each already limited to 1 MiB by collection.
const maxClaudeRecordBytes = 4 << 20

// Linux limits a single argv element to 128 KiB; the schema is one element.
const maxClaudeSchemaBytes = 64 << 10

func claudePreflight(l agentruntime.BridgeConfig, environ []string) error {
	// The CLI silently substitutes its default for an unknown effort, which
	// would run a different binding than the pinned workflow route.
	if !claudeEfforts[l.Binding.Effort] {
		return fmt.Errorf("unsupported Claude effort %q", l.Binding.Effort)
	}
	if strings.HasPrefix(l.Binding.Model, "-") || strings.ContainsAny(l.Binding.Model, " \t\r\n\x00") {
		return errors.New("invalid Claude model binding")
	}
	values := map[string]string{}
	for _, entry := range environ {
		if k, v, ok := strings.Cut(entry, "="); ok {
			values[k] = v
		}
	}
	if dir := values["CLAUDE_CONFIG_DIR"]; dir != "" && !filepath.IsAbs(dir) {
		return errors.New("CLAUDE_CONFIG_DIR must be absolute")
	}
	for _, k := range claudeCredentialVariables {
		if values[k] != "" {
			return nil
		}
	}
	return fmt.Errorf("Claude credentials must be supplied explicitly in the target environment (one of %s)", strings.Join(claudeCredentialVariables, ", "))
}

func claudeEnvironment(environ []string) []string {
	for _, entry := range environ {
		if strings.HasPrefix(entry, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=") {
			return environ
		}
	}
	// No updater, telemetry or error reporting from a supervised model turn.
	return append(environ, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
}

// claudeArgs returns the headless Claude Code argv and the session identity
// the provider must announce ("" when a fresh ephemeral session is allowed).
// Every per-turn restriction is repeated on resume; values use --flag=value so
// no binding value can be parsed as a separate option.
func claudeArgs(l agentruntime.BridgeConfig, session *providerSession, schema []byte) ([]string, string, error) {
	if len(schema) > maxClaudeSchemaBytes {
		return nil, "", errors.New("output schema exceeds provider argument bound")
	}
	toolsArg := ""
	if l.Session == "agentic" {
		toolsArg = strings.Join(agentruntime.ClaudeNativeTools(l.Profile, l.ChangeProducer || l.ArtifactsOnly || len(l.WriteScopes) > 0 || l.StagingWrite || len(l.Scratch) > 0), ",")
	}
	args := []string{"-p", "--output-format=stream-json", "--verbose", "--model=" + l.Binding.Model, "--tools", toolsArg, "--restricted", "--strict-mcp-config", "--disable-slash-commands", "--permission-mode=dontAsk", "--permission-prompts=none", "--json-schema=" + string(schema)}
	if l.Session == "agentic" {
		for _, path := range append(append([]string{l.OutputDir, l.StagingDir}, l.ReadRootPaths...), l.NativeReadDirs...) {
			if path != "" {
				args = append(args, "--add-dir", path)
			}
		}
	}
	if l.Binding.Effort != "" {
		args = append(args, "--effort="+l.Binding.Effort)
	}
	if l.Session == "agentic" && l.ClaudeSettings != "" {
		args = append(args, "--settings="+l.ClaudeSettings)
	}
	if l.TurnBudget != nil && l.TurnBudget.MaxAssistantTurns > 0 {
		args = append(args, "--max-turns="+strconv.Itoa(l.TurnBudget.MaxAssistantTurns))
	}
	switch {
	case !l.FreshRole && l.ResumeSession != "":
		if !threadIDPattern.MatchString(l.ResumeSession) {
			return nil, "", errors.New("invalid provider session identity")
		}
		return append(args, "--resume="+l.ResumeSession), l.ResumeSession, nil
	case session == nil && l.Session != "agentic":
		return append(args, "--no-session-persistence"), "", nil
	case !l.FreshRole && session != nil && session.resumed:
		return append(args, "--resume="+session.state.ThreadID), session.state.ThreadID, nil
	}
	id, err := newSessionUUID()
	if err != nil {
		return nil, "", err
	}
	return append(args, "--session-id="+id), id, nil
}

func newSessionUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

type claudeRecord struct {
	Message struct {
		ID      string            `json:"id"`
		Content []claudeToolBlock `json:"content"`
	} `json:"message"`
	ParentToolUseID  *string           `json:"parent_tool_use_id"`
	ReportedError    *bool             `json:"-"`
	NumTurns         *int              `json:"num_turns,omitempty"`
	DurationMS       *int64            `json:"duration_ms,omitempty"`
	TotalCostUSD     *float64          `json:"total_cost_usd,omitempty"`
	Type             string            `json:"type"`
	Subtype          string            `json:"subtype"`
	SessionID        string            `json:"session_id"`
	Tools            []string          `json:"tools"`
	MCPServers       []json.RawMessage `json:"mcp_servers"`
	IsError          bool              `json:"is_error"`
	StructuredOutput json.RawMessage   `json:"structured_output"`
	ToolCalls        []AgenticToolCall `json:"tool_calls,omitempty"`
}

// claudeStream retains only session identity, the announced tool surface and
// the terminal result from Claude Code stream-json. Work content in assistant
// records is never interpreted except for assistant turn counting and explicit
// Fae agentic tool_calls in agentic sessions. Non-JSON lines are ignored.
type claudeStream struct {
	mu                sync.Mutex
	messageIDs        map[string]bool
	nativeToolIDs     map[string]bool
	onBudgetExhausted func()
	terminalSeen      bool
	pending           []byte
	skipping          bool
	oversized         bool
	inits             []claudeRecord
	results           []claudeRecord
	allowedTools      map[string]bool
	maxAssistantTurns int
	assistantTurns    int
	budgetExhausted   bool
	onCheckpoint      func(sessionID string, assistantTurns int)
	toolbox           *AgenticToolbox
	toolErr           error
}

func (s *claudeStream) toolAllowed(tool string) bool {
	if s.allowedTools != nil {
		return s.allowedTools[tool]
	}
	return claudeTools[tool]
}

func (s *claudeStream) currentSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionIDUnlocked()
}
func (s *claudeStream) turns() int      { s.mu.Lock(); defer s.mu.Unlock(); return s.assistantTurns }
func (s *claudeStream) exhausted() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.budgetExhausted }
func (s *claudeStream) sessionIDUnlocked() string {
	if len(s.inits) > 0 {
		return s.inits[0].SessionID
	}
	return ""
}

func (s *claudeStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i]
		}
		if !s.skipping && len(s.pending)+len(chunk) > maxClaudeRecordBytes {
			s.skipping, s.oversized, s.pending = true, true, nil
		}
		if !s.skipping {
			s.pending = append(s.pending, chunk...)
		}
		if i < 0 {
			break
		}
		if !s.skipping {
			s.record(s.pending)
		}
		s.pending, s.skipping = s.pending[:0], false
		p = p[i+1:]
	}
	return n, nil
}

func (s *claudeStream) record(line []byte) {
	var v claudeRecord
	if json.Unmarshal(line, &v) != nil {
		return
	}
	switch {
	case v.Type == "system" && v.Subtype == "init":
		s.inits = append(s.inits, v)
		if s.onCheckpoint != nil && threadIDPattern.MatchString(v.SessionID) {
			s.onCheckpoint(v.SessionID, s.assistantTurns)
		}
	case v.Type == "assistant":
		main := v.ParentToolUseID == nil || *v.ParentToolUseID == ""
		if v.SessionID != "" && s.sessionIDUnlocked() != "" && v.SessionID != s.sessionIDUnlocked() {
			main = false
		}
		if main && !s.terminalSeen {
			// Old bridges without IDs retain their former record count. Native
			// stream-json IDs identify split content blocks exactly once.
			fresh := v.Message.ID == ""
			if v.Message.ID != "" {
				if s.messageIDs == nil {
					s.messageIDs = map[string]bool{}
				}
				fresh = !s.messageIDs[v.Message.ID]
				s.messageIDs[v.Message.ID] = true
			}
			if fresh {
				s.assistantTurns++
			}
			if s.maxAssistantTurns > 0 && s.assistantTurns > s.maxAssistantTurns && !s.budgetExhausted {
				s.budgetExhausted = true
				if s.onBudgetExhausted != nil {
					s.onBudgetExhausted()
				}
			}
		}
		if main && !s.terminalSeen {
			s.recordNativeTools(v.Message.Content)
		}
		if s.toolbox != nil && len(v.ToolCalls) > 0 && !s.budgetExhausted {
			for _, tc := range v.ToolCalls {
				if _, err := s.toolbox.Call(context.Background(), tc); err != nil && s.toolErr == nil {
					s.toolErr = err
				}
			}
		}
		sid := v.SessionID
		if sid == "" {
			sid = s.sessionIDUnlocked()
		}
		if s.onCheckpoint != nil && threadIDPattern.MatchString(sid) {
			s.onCheckpoint(sid, s.assistantTurns)
		}
	case v.Type == "result":
		if v.ParentToolUseID != nil && *v.ParentToolUseID != "" {
			return
		}
		s.terminalSeen = true
		var fields map[string]json.RawMessage
		_ = json.Unmarshal(line, &fields)
		if _, ok := fields["is_error"]; ok {
			value := v.IsError
			v.ReportedError = &value
		}
		if v.Subtype == "error_max_turns" {
			s.budgetExhausted = true
		}
		if (len(v.StructuredOutput) == 0 || string(v.StructuredOutput) == "null") && s.toolbox != nil {
			if ho := s.toolbox.HandoffOutput(); len(ho) > 0 {
				v.StructuredOutput = ho
			}
		}
		s.results = append(s.results, v)
	}
}

// claudeUnusableResult is a turn with the requested session and tool surface
// whose result is missing, repeated, oversized, an error or not structured.
type claudeUnusableResult struct{ reason string }

func (e *claudeUnusableResult) Error() string { return e.reason }

// failedSurfaceAllowed reports whether a turn that failed before it finished
// announced nothing beyond the allowed surface: no record skipped as
// oversized, at most one session, and that session's tools only the allowed
// ones, with no MCP servers. Only then is the failure typed provider-failed
// (FAE-WF-034); otherwise it stays untyped, like a tool-surface refusal of a
// finished turn.
func (s *claudeStream) failedSurfaceAllowed() bool {
	if len(s.pending) > 0 && !s.skipping {
		s.record(s.pending)
		s.pending = nil
	}
	if s.oversized || len(s.inits) > 1 {
		return false
	}
	for _, init := range s.inits {
		if len(init.MCPServers) != 0 {
			return false
		}
		for _, tool := range init.Tools {
			if !s.toolAllowed(tool) {
				return false
			}
		}
	}
	return true
}

// finish validates one complete turn and writes its structured output to
// answer. expected is the session the bridge requested, or "" for ephemeral.
func (s *claudeStream) finish(expected, answer string) (string, error) {
	if len(s.pending) > 0 && !s.skipping {
		s.record(s.pending)
		s.pending = nil
	}
	var init, result claudeRecord
	if len(s.inits) == 1 {
		init = s.inits[0]
	}
	if len(s.results) == 1 {
		result = s.results[0]
	}
	unsupported := ""
	for _, tool := range init.Tools {
		if !s.toolAllowed(tool) && unsupported == "" {
			unsupported = tool
		}
	}
	// Every condition is evaluated; the first failure in this order is reported.
	reason := ""
	check := func(ok bool, why string) bool {
		if !ok && reason == "" {
			reason = why
		}
		return ok
	}
	bounded := check(!s.oversized, "provider record exceeds bound")
	oneSession := check(len(s.inits) == 1, fmt.Sprintf("provider announced %d sessions", len(s.inits)))
	validSession := check(threadIDPattern.MatchString(init.SessionID), "invalid provider session identity")
	requested := check(expected == "" || init.SessionID == expected, "provider resumed a different session")
	tools := check(unsupported == "", fmt.Sprintf("provider exposed unsupported tool %q", unsupported))
	servers := check(len(init.MCPServers) == 0, "provider loaded MCP servers")
	oneResult := check(len(s.results) == 1, fmt.Sprintf("provider produced %d results", len(s.results)))
	sameSession := check(result.SessionID == init.SessionID, "provider result belongs to a different session")
	succeeded := check(!result.IsError && result.Subtype == "success" && len(result.StructuredOutput) > 0 && string(result.StructuredOutput) != "null", "provider turn did not produce structured output")
	if !CanAcceptClaudeTurn(oneSession && validSession && requested && sameSession, tools && servers, bounded && oneResult && succeeded) {
		// The requested session with the allowed surface ended without one
		// usable result of its own: the provider's answer is refused.
		if oneSession && validSession && requested && tools && servers && (len(s.results) != 1 || sameSession) {
			return "", &claudeUnusableResult{reason}
		}
		return "", errors.New(reason)
	}
	if s.toolErr != nil {
		return "", &claudeUnusableResult{s.toolErr.Error()}
	}
	f, err := os.OpenFile(answer, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(result.StructuredOutput)
	if err = errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	return init.SessionID, nil
}
