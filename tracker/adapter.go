package tracker

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/xleelz/fae/symphony"
)

// Portable adapter error categories (Symphony §11.4).
const (
	ErrCategoryUnsupportedKind = "unsupported_tracker_kind"
	ErrCategoryInvalidConfig   = "invalid_tracker_config"
	ErrCategoryMissingSecret   = "missing_tracker_secret"
	ErrCategoryRequest         = "tracker_request"
	ErrCategoryStatus          = "tracker_status"
	ErrCategoryResponse        = "tracker_response"
	ErrCategoryPagination      = "tracker_pagination"
	ErrCategoryRateLimited     = "tracker_rate_limited"
)

// Error is the portable tracker error carrying a stable Category and human-readable Message.
type Error struct {
	Category     string `json:"category"`
	Message      string `json:"message"`
	Retryable    bool   `json:"retryable,omitempty"`
	RetryAfterMS int    `json:"retry_after_ms,omitempty"`
	StatusCode   int    `json:"status_code,omitempty"`
	Cause        error  `json:"-"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Category, e.Message)
	}
	return e.Category
}

func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// ToolSpec describes a provider-native coding-agent tool (Symphony §10.5).
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Mutates     bool           `json:"mutates"`
	InputSchema map[string]any `json:"input_schema"`
}

// ToolContext supplies the current normalized issue to host-side tool execution (Symphony §10.5).
type ToolContext struct {
	Issue symphony.Issue `json:"issue"`
}

// ToolResult is the JSON-safe structured result of a host-executed provider tool (Symphony §10.5).
type ToolResult struct {
	Success bool           `json:"success"`
	Output  map[string]any `json:"output,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// Adapter is the issue tracker integration contract (Symphony §11.1 + Fae workflow state/comment sync).
type Adapter interface {
	Kind() string
	ActiveStates() []string
	TerminalStates() []string
	FetchIssuesByStates(ctx context.Context, stateNames []string) ([]symphony.Issue, error)
	FetchIssuesByIDs(ctx context.Context, issueIDs []string) ([]symphony.Issue, error)
	TransitionIssueState(ctx context.Context, issueID string, targetStateName string) (symphony.Issue, error)
	AddIssueComment(ctx context.Context, issueID string, body string) (string, error)
	AgentToolSpecs() []ToolSpec
	SecretEnvironmentNames() []string
	ExecuteAgentTool(ctx context.Context, name string, arguments map[string]any, toolCtx ToolContext) ToolResult
}

// NewAdapter constructs the configured tracker adapter (Symphony §5.3.1 & §11.1).
func NewAdapter(cfg symphony.TrackerConfig, env func(string) string, httpClient *http.Client) (Adapter, error) {
	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	switch kind {
	case "linear":
		return NewLinearAdapter(cfg, env, httpClient)
	default:
		return nil, &Error{
			Category: ErrCategoryUnsupportedKind,
			Message:  fmt.Sprintf("unsupported tracker kind %q", cfg.Kind),
		}
	}
}
