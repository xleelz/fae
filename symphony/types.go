package symphony

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// Typed workflow and template error classes (Symphony §5.5).
var (
	ErrMissingWorkflowFile       = errors.New("missing_workflow_file")
	ErrWorkflowParseError        = errors.New("workflow_parse_error")
	ErrWorkflowFrontMatterNotMap = errors.New("workflow_front_matter_not_a_map")
	ErrTemplateParseError        = errors.New("template_parse_error")
	ErrTemplateRenderError       = errors.New("template_render_error")
)

// BlockerRef represents best-effort provider blocker metadata (Symphony §4.1.1).
type BlockerRef struct {
	Terminal   *bool   `json:"terminal,omitempty"`
	ID         *string `json:"id"`
	Identifier *string `json:"identifier"`
	State      *string `json:"state"`
}

// Issue is the normalized schedulable work item (Symphony §4.1.1).
type Issue struct {
	ID           string         `json:"id"`
	NativeRef    map[string]any `json:"native_ref"`
	Identifier   string         `json:"identifier"`
	Title        string         `json:"title"`
	Description  *string        `json:"description"`
	Priority     *int           `json:"priority"`
	State        string         `json:"state"`
	BranchName   *string        `json:"branch_name"`
	URL          *string        `json:"url"`
	AssigneeID   *string        `json:"assignee_id"`
	Labels       []string       `json:"labels"`
	BlockedBy    []BlockerRef   `json:"blocked_by"`
	Dispatchable bool           `json:"dispatchable"`
	CreatedAt    *time.Time     `json:"created_at"`
	UpdatedAt    *time.Time     `json:"updated_at"`
}

// WorkflowDefinition holds the parsed WORKFLOW.md payload (Symphony §4.1.2).
type WorkflowDefinition struct {
	Path           string         `json:"path,omitempty"`
	Config         map[string]any `json:"config"`
	PromptTemplate string         `json:"prompt_template"`
}

// TrackerConfig holds typed tracker configuration (Symphony §5.3.1).
type TrackerConfig struct {
	Kind           string         `json:"kind"`
	Provider       map[string]any `json:"provider"`
	RequiredLabels []string       `json:"required_labels"`
	ActiveStates   []string       `json:"active_states"`
	TerminalStates []string       `json:"terminal_states"`
}

// PollingConfig holds polling configuration (Symphony §5.3.2).
type PollingConfig struct {
	IntervalMS int `json:"interval_ms"`
}

// WorkspaceConfig holds workspace configuration (Symphony §5.3.3).
type WorkspaceConfig struct {
	Root string `json:"root"`
}

// HooksConfig holds workspace lifecycle hook scripts and timeout (Symphony §5.3.4).
type HooksConfig struct {
	AfterCreate  *string `json:"after_create,omitempty"`
	BeforeRun    *string `json:"before_run,omitempty"`
	AfterRun     *string `json:"after_run,omitempty"`
	BeforeRemove *string `json:"before_remove,omitempty"`
	TimeoutMS    int     `json:"timeout_ms"`
}

// AgentConfig holds agent concurrency, turn, and retry limits (Symphony §5.3.5).
type AgentConfig struct {
	MaxConcurrentAgents        int            `json:"max_concurrent_agents"`
	MaxTurns                   int            `json:"max_turns"`
	MaxRetryBackoffMS          int            `json:"max_retry_backoff_ms"`
	MaxConcurrentAgentsByState map[string]int `json:"max_concurrent_agents_by_state"`
}

// CodexConfig holds coding-agent app-server settings (Symphony §5.3.6).
type CodexConfig struct {
	Command           string `json:"command"`
	ApprovalPolicy    any    `json:"approval_policy,omitempty"`
	ThreadSandbox     any    `json:"thread_sandbox,omitempty"`
	TurnSandboxPolicy any    `json:"turn_sandbox_policy,omitempty"`
	TurnTimeoutMS     int    `json:"turn_timeout_ms"`
	ReadTimeoutMS     int    `json:"read_timeout_ms"`
	StallTimeoutMS    int    `json:"stall_timeout_ms"`
}

// ServerConfig holds optional HTTP extension settings (Symphony §13.7).
type ServerConfig struct {
	Port *int   `json:"port,omitempty"`
	Host string `json:"host,omitempty"`
}

// WorkerConfig holds optional SSH worker pool extension settings (Symphony Appendix A).
type WorkerConfig struct {
	SSHHosts                   []string `json:"ssh_hosts,omitempty"`
	MaxConcurrentAgentsPerHost int      `json:"max_concurrent_agents_per_host,omitempty"`
	hasExplicitPerHostLimit    bool
}

// FaeConfig selects the existing workflow engine for the embedded daemon
// bridge. Standalone Symphony retains its own runner. Tracker writes are opt-in.
type FaeConfig struct {
	WorkflowID         string `json:"workflow_id,omitempty"`
	WorkflowRevision   int64  `json:"workflow_revision,omitempty"`
	TargetConfig       string `json:"target_config,omitempty"`
	TargetRevision     int64  `json:"target_revision,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
	Mode               string `json:"mode"`
	SyncState          bool   `json:"sync_state"`
	PostComments       bool   `json:"post_comments"`
}

// ServiceConfig is the typed view of WorkflowDefinition.Config (Symphony §4.1.3 & §6.4).
type ServiceConfig struct {
	Fae       FaeConfig       `json:"fae"`
	Tracker   TrackerConfig   `json:"tracker"`
	Polling   PollingConfig   `json:"polling"`
	Workspace WorkspaceConfig `json:"workspace"`
	Hooks     HooksConfig     `json:"hooks"`
	Agent     AgentConfig     `json:"agent"`
	Codex     CodexConfig     `json:"codex"`
	Server    ServerConfig    `json:"server"`
	Worker    WorkerConfig    `json:"worker"`
}

// Workspace represents a per-issue directory (Symphony §4.1.4).
type Workspace struct {
	Path         string `json:"path"`
	WorkspaceKey string `json:"workspace_key"`
	CreatedNow   bool   `json:"created_now"`
}

// NormalizeState trims surrounding whitespace and lowercases a state name (Symphony §4.2).
func NormalizeState(state string) string {
	return strings.ToLower(strings.TrimSpace(state))
}

// StateInList checks whether state matches any entry in list case-insensitively.
func StateInList(state string, list []string) bool {
	want := NormalizeState(state)
	if want == "" {
		return false
	}
	for _, s := range list {
		if NormalizeState(s) == want {
			return true
		}
	}
	return false
}

// IssueRoutable checks whether adapter-provided Dispatchable is true and all
// requiredLabels match case-insensitively (Symphony §5.3.1 & §8.2).
func IssueRoutable(issue Issue, requiredLabels []string) bool {
	if !issue.Dispatchable {
		return false
	}
	if len(requiredLabels) == 0 {
		return true
	}
	have := make(map[string]bool, len(issue.Labels))
	for _, l := range issue.Labels {
		norm := NormalizeState(l)
		if norm != "" {
			have[norm] = true
		}
	}
	for _, req := range requiredLabels {
		norm := NormalizeState(req)
		if norm == "" || !have[norm] {
			return false
		}
	}
	return true
}

// SortIssuesForDispatch returns a sorted copy of issues ordered by:
// 1. priority ascending for 1..4; all other integers and nil sort after 1..4
// 2. created_at oldest first; nil sorts last
// 3. identifier lexicographic tie-breaker (Symphony §8.2).
func SortIssuesForDispatch(issues []Issue) []Issue {
	out := make([]Issue, len(issues))
	copy(out, issues)
	priorityRank := func(p *int) int {
		if p != nil && *p >= 1 && *p <= 4 {
			return *p
		}
		return 5
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		pa, pb := priorityRank(a.Priority), priorityRank(b.Priority)
		if pa != pb {
			return pa < pb
		}
		if a.CreatedAt != nil && b.CreatedAt != nil {
			if !a.CreatedAt.Equal(*b.CreatedAt) {
				return a.CreatedAt.Before(*b.CreatedAt)
			}
		} else if a.CreatedAt != nil && b.CreatedAt == nil {
			return true
		} else if a.CreatedAt == nil && b.CreatedAt != nil {
			return false
		}
		return a.Identifier < b.Identifier
	})
	return out
}
