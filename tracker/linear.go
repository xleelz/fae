package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xleelz/fae/symphony"
)

const (
	defaultLinearEndpoint = "https://api.linear.app/graphql"
	linearPageSize        = 50
	linearNestedPageLimit = 100
	linearNestedNodeLimit = 10000
)

var (
	defaultLinearActiveStates   = []string{"Todo", "In Progress"}
	defaultLinearTerminalStates = []string{"Done", "Canceled", "Cancelled", "Duplicate"}
	defaultLinearStateMap       = map[string]string{
		"in_progress":  "In Progress",
		"implement":    "In Progress",
		"verify":       "In Progress",
		"review":       "In Review",
		"human_review": "In Review",
		"done":         "Done",
		"escalated":    "In Review",
		"cancelled":    "Canceled",
		"canceled":     "Canceled",
	}
)

// LinearAdapter implements the Tracker Adapter contract for Linear's GraphQL API.
type LinearAdapter struct {
	endpoint         string
	apiKey           string
	secretEnvVarName string
	teamID           string
	teamKey          string
	projectID        string
	projectSlug      string
	assigneeID       string
	activeStates     []string
	terminalStates   []string
	stateMap         map[string]string
	httpClient       *http.Client

	stateCacheMu sync.RWMutex
	stateCache   map[string]string // key: teamScope + "|" + normalizedStateName -> stateID

	omittedMalformed atomic.Int64
	Logger           func(format string, args ...any)
}

// NewLinearAdapter validates Linear configuration and returns a configured *LinearAdapter.
func NewLinearAdapter(cfg symphony.TrackerConfig, env func(string) string, httpClient *http.Client) (*LinearAdapter, error) {
	if strings.ToLower(strings.TrimSpace(cfg.Kind)) != "linear" {
		return nil, &Error{
			Category: ErrCategoryUnsupportedKind,
			Message:  fmt.Sprintf("unsupported tracker kind %q", cfg.Kind),
		}
	}
	if env == nil {
		env = os.Getenv
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	endpoint := defaultLinearEndpoint
	if rawEndpoint, ok := cfg.Provider["endpoint"]; ok && rawEndpoint != nil {
		s, ok := rawEndpoint.(string)
		if !ok {
			return nil, &Error{
				Category: ErrCategoryInvalidConfig,
				Message:  "linear provider endpoint must be a string",
			}
		}
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			endpoint = trimmed
		}
	}
	if err := validateLinearEndpoint(endpoint); err != nil {
		return nil, err
	}

	apiKey, secretEnvVar, err := resolveLinearAPIKey(cfg.Provider, env)
	if err != nil {
		return nil, err
	}

	teamID, err := optionalStringProviderField(cfg.Provider, "team_id")
	if err != nil {
		return nil, err
	}
	teamKey, err := optionalStringProviderField(cfg.Provider, "team_key")
	if err != nil {
		return nil, err
	}
	projectID, err := optionalStringProviderField(cfg.Provider, "project_id")
	if err != nil {
		return nil, err
	}
	projectSlug, err := optionalStringProviderField(cfg.Provider, "project_slug")
	if err != nil {
		return nil, err
	}
	assigneeID, err := optionalStringProviderField(cfg.Provider, "assignee_id")
	if err != nil {
		return nil, err
	}

	stateMap := make(map[string]string, len(defaultLinearStateMap))
	for k, v := range defaultLinearStateMap {
		stateMap[k] = v
	}
	if rawStateMap, ok := cfg.Provider["state_map"]; ok && rawStateMap != nil {
		switch m := rawStateMap.(type) {
		case map[string]string:
			for k, v := range m {
				nk := strings.ToLower(strings.TrimSpace(k))
				nv := strings.TrimSpace(v)
				if nk != "" && nv != "" {
					stateMap[nk] = nv
				}
			}
		case map[string]any:
			for k, rawV := range m {
				v, ok := rawV.(string)
				if !ok {
					return nil, &Error{
						Category: ErrCategoryInvalidConfig,
						Message:  fmt.Sprintf("linear provider state_map[%q] must be a string", k),
					}
				}
				nk := strings.ToLower(strings.TrimSpace(k))
				nv := strings.TrimSpace(v)
				if nk != "" && nv != "" {
					stateMap[nk] = nv
				}
			}
		default:
			return nil, &Error{
				Category: ErrCategoryInvalidConfig,
				Message:  "linear provider state_map must be a string map",
			}
		}
	}

	activeStates := cleanStateSlice(cfg.ActiveStates)
	if len(activeStates) == 0 {
		activeStates = append([]string(nil), defaultLinearActiveStates...)
	}
	terminalStates := cleanStateSlice(cfg.TerminalStates)
	if len(terminalStates) == 0 {
		terminalStates = append([]string(nil), defaultLinearTerminalStates...)
	}

	return &LinearAdapter{
		endpoint:         endpoint,
		apiKey:           apiKey,
		secretEnvVarName: secretEnvVar,
		teamID:           teamID,
		teamKey:          teamKey,
		projectID:        projectID,
		projectSlug:      projectSlug,
		assigneeID:       assigneeID,
		activeStates:     activeStates,
		terminalStates:   terminalStates,
		stateMap:         stateMap,
		httpClient:       httpClient,
		stateCache:       make(map[string]string),
	}, nil
}

func validateLinearEndpoint(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return &Error{
			Category: ErrCategoryInvalidConfig,
			Message:  fmt.Sprintf("invalid Linear endpoint URL %q", raw),
		}
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme == "https" {
		return nil
	}
	if scheme == "http" && isLoopbackHost(parsed.Hostname()) {
		return nil
	}
	return &Error{
		Category: ErrCategoryInvalidConfig,
		Message:  fmt.Sprintf("Linear endpoint %q must use https (or loopback http for tests)", raw),
	}
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func resolveLinearAPIKey(provider map[string]any, env func(string) string) (string, string, error) {
	var recordedEnvVar string
	if rawEnv, ok := provider["_api_key_env"].(string); ok {
		recordedEnvVar = strings.TrimSpace(rawEnv)
	}
	explicitKey, _ := provider["_api_key_explicit"].(bool)

	if rawKey, ok := provider["api_key"]; ok && rawKey != nil {
		s, ok := rawKey.(string)
		if !ok {
			return "", "", &Error{
				Category: ErrCategoryInvalidConfig,
				Message:  "linear provider api_key must be a string",
			}
		}
		trimmed := strings.TrimSpace(s)
		if strings.HasPrefix(trimmed, "$") {
			varName := strings.TrimSpace(strings.TrimPrefix(trimmed, "$"))
			if varName == "" {
				return "", "", &Error{
					Category: ErrCategoryInvalidConfig,
					Message:  "linear provider api_key env reference is empty",
				}
			}
			val := strings.TrimSpace(env(varName))
			if val == "" {
				return "", varName, &Error{
					Category: ErrCategoryMissingSecret,
					Message:  "missing Linear API key",
				}
			}
			return val, varName, nil
		}
		if trimmed != "" {
			return trimmed, recordedEnvVar, nil
		}
		if explicitKey {
			envName := recordedEnvVar
			if envName == "" {
				envName = "LINEAR_API_KEY"
			}
			return "", envName, &Error{
				Category: ErrCategoryMissingSecret,
				Message:  "missing Linear API key",
			}
		}
	}
	if explicitKey {
		envName := recordedEnvVar
		if envName == "" {
			envName = "LINEAR_API_KEY"
		}
		return "", envName, &Error{
			Category: ErrCategoryMissingSecret,
			Message:  "missing Linear API key",
		}
	}
	val := strings.TrimSpace(env("LINEAR_API_KEY"))
	if val == "" {
		return "", "LINEAR_API_KEY", &Error{
			Category: ErrCategoryMissingSecret,
			Message:  "missing Linear API key",
		}
	}
	return val, "LINEAR_API_KEY", nil
}

func optionalStringProviderField(provider map[string]any, key string) (string, error) {
	if provider == nil {
		return "", nil
	}
	raw, ok := provider[key]
	if !ok || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", &Error{
			Category: ErrCategoryInvalidConfig,
			Message:  fmt.Sprintf("linear provider %s must be a string", key),
		}
	}
	return strings.TrimSpace(s), nil
}

func cleanStateSlice(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, s := range in {
		trimmed := strings.TrimSpace(s)
		norm := symphony.NormalizeState(trimmed)
		if norm == "" || seen[norm] {
			continue
		}
		seen[norm] = true
		out = append(out, trimmed)
	}
	return out
}

// Kind returns the adapter kind identifier ("linear").
func (a *LinearAdapter) Kind() string {
	return "linear"
}

// ActiveStates returns a copy of the configured active states.
func (a *LinearAdapter) ActiveStates() []string {
	return append([]string(nil), a.activeStates...)
}

// TerminalStates returns a copy of the configured terminal states.
func (a *LinearAdapter) TerminalStates() []string {
	return append([]string(nil), a.terminalStates...)
}

// OmittedMalformedCount returns the total number of malformed records omitted during FetchIssuesByStates calls.
func (a *LinearAdapter) OmittedMalformedCount() int {
	return int(a.omittedMalformed.Load())
}

// StateForWorkflowStage maps a Fae workflow stage or outcome to the configured Linear state name.
func (a *LinearAdapter) StateForWorkflowStage(stageOrOutcome string) string {
	key := strings.ToLower(strings.TrimSpace(stageOrOutcome))
	if key == "" {
		return ""
	}
	if mapped, ok := a.stateMap[key]; ok {
		return mapped
	}
	return ""
}

// FetchIssuesByStates queries Linear for issues matching stateNames within the configured scope.
// Empty stateNames returns an empty slice immediately without making an HTTP request (Symphony §11.1).
func (a *LinearAdapter) FetchIssuesByStates(ctx context.Context, stateNames []string) ([]symphony.Issue, error) {
	cleanedStates := cleanStateSlice(stateNames)
	if len(cleanedStates) == 0 {
		return []symphony.Issue{}, nil
	}

	filter := a.buildScopeFilter()
	filter["state"] = map[string]any{
		"name": map[string]any{"in": cleanedStates},
	}

	nodes, err := a.fetchPaginatedIssueNodes(ctx, fetchIssuesByStatesQuery, map[string]any{
		"stateNames": cleanedStates,
		"filter":     filter,
	})
	if err != nil {
		return nil, err
	}

	out := make([]symphony.Issue, 0, len(nodes))
	for _, node := range nodes {
		if !a.nodeMatchesScope(node, false) {
			continue
		}
		if len(cleanedStates) > 0 && strings.TrimSpace(node.State.Name) != "" && !symphony.StateInList(node.State.Name, cleanedStates) {
			continue
		}
		issue, ok := a.normalizeIssueNode(node)
		if !ok {
			a.omittedMalformed.Add(1)
			if a.Logger != nil {
				a.Logger("tracker: omitting malformed Linear issue record id=%q identifier=%q", node.ID, node.Identifier)
			}
			continue
		}
		out = append(out, issue)
	}
	return out, nil
}

// FetchCandidateIssues queries Linear for issues in the adapter's configured active states (Symphony §11.1).
func (a *LinearAdapter) FetchCandidateIssues(ctx context.Context) ([]symphony.Issue, error) {
	return a.FetchIssuesByStates(ctx, a.activeStates)
}

// FetchIssueStatesByIDs queries Linear for current issue states by IDs (Symphony §11.1).
func (a *LinearAdapter) FetchIssueStatesByIDs(ctx context.Context, issueIDs []string) ([]symphony.Issue, error) {
	return a.FetchIssuesByIDs(ctx, issueIDs)
}

// FetchIssuesByIDs queries Linear for the given issue IDs for reconciliation.
// Empty issueIDs returns an empty slice immediately without making an HTTP request (Symphony §11.1).
// Malformed records in FetchIssuesByIDs fail the batch with ErrCategoryResponse (Symphony §11.1).
func (a *LinearAdapter) FetchIssuesByIDs(ctx context.Context, issueIDs []string) ([]symphony.Issue, error) {
	cleanedIDs := make([]string, 0, len(issueIDs))
	seen := make(map[string]bool, len(issueIDs))
	for _, id := range issueIDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		cleanedIDs = append(cleanedIDs, trimmed)
	}
	if len(cleanedIDs) == 0 {
		return []symphony.Issue{}, nil
	}

	filter := a.buildScopeFilter()
	filter["id"] = map[string]any{"in": cleanedIDs}

	nodes, err := a.fetchPaginatedIssueNodes(ctx, fetchIssuesByIDsQuery, map[string]any{
		"ids":    cleanedIDs,
		"filter": filter,
	})
	if err != nil {
		return nil, err
	}

	out := make([]symphony.Issue, 0, len(nodes))
	for _, node := range nodes {
		if !a.nodeMatchesScope(node, false) {
			continue
		}
		issue, ok := a.normalizeIssueNode(node)
		if !ok {
			return nil, &Error{
				Category: ErrCategoryResponse,
				Message:  "malformed issue record in FetchIssuesByIDs response",
			}
		}
		if !seen[issue.ID] && !seen[issue.Identifier] {
			continue
		}
		out = append(out, issue)
	}
	return out, nil
}

func (a *LinearAdapter) buildScopeFilter() map[string]any {
	filter := make(map[string]any)
	teamFilter := make(map[string]any)
	if a.teamID != "" {
		teamFilter["id"] = map[string]any{"eq": a.teamID}
	}
	if a.teamKey != "" {
		teamFilter["key"] = map[string]any{"eq": a.teamKey}
	}
	if len(teamFilter) > 0 {
		filter["team"] = teamFilter
	}

	projectFilter := make(map[string]any)
	if a.projectID != "" {
		projectFilter["id"] = map[string]any{"eq": a.projectID}
	}
	if a.projectSlug != "" {
		projectFilter["slugId"] = map[string]any{"eq": a.projectSlug}
	}
	if len(projectFilter) > 0 {
		filter["project"] = projectFilter
	}
	return filter
}

func (a *LinearAdapter) nodeMatchesScope(node linearIssueNode, strict bool) bool {
	if a.teamID != "" {
		if node.Team.ID == "" {
			if strict {
				return false
			}
		} else if node.Team.ID != a.teamID {
			return false
		}
	}
	if a.teamKey != "" {
		if node.Team.Key == "" {
			if strict {
				return false
			}
		} else if !strings.EqualFold(node.Team.Key, a.teamKey) {
			return false
		}
	}
	if a.projectID != "" {
		if node.Project.ID == "" {
			if strict {
				return false
			}
		} else if node.Project.ID != a.projectID {
			return false
		}
	}
	if a.projectSlug != "" {
		if node.Project.SlugID == "" {
			if strict {
				return false
			}
		} else if !strings.EqualFold(node.Project.SlugID, a.projectSlug) {
			return false
		}
	}
	return true
}

func (a *LinearAdapter) fetchPaginatedIssueNodes(ctx context.Context, query string, baseVars map[string]any) ([]linearIssueNode, error) {
	var allNodes []linearIssueNode
	seenCursors := make(map[string]bool)
	var after any

	for {
		vars := make(map[string]any, len(baseVars)+4)
		for k, v := range baseVars {
			vars[k] = v
		}
		vars["first"] = linearPageSize
		if after != nil {
			vars["after"] = after
		} else {
			vars["after"] = nil
		}
		if a.teamID != "" {
			vars["teamId"] = a.teamID
		}
		if a.teamKey != "" {
			vars["teamKey"] = a.teamKey
		}
		if a.projectID != "" {
			vars["projectId"] = a.projectID
		}
		if a.projectSlug != "" {
			vars["projectSlug"] = a.projectSlug
		}

		var data struct {
			Issues *struct {
				PageInfo struct {
					HasNextPage bool    `json:"hasNextPage"`
					EndCursor   *string `json:"endCursor"`
				} `json:"pageInfo"`
				Nodes []json.RawMessage `json:"nodes"`
			} `json:"issues"`
		}
		if err := a.doGraphQL(ctx, query, vars, &data); err != nil {
			return nil, err
		}
		if data.Issues == nil {
			return nil, &Error{
				Category: ErrCategoryResponse,
				Message:  "missing issues connection in Linear GraphQL response",
			}
		}

		for _, rawNode := range data.Issues.Nodes {
			if bytes.Equal(bytes.TrimSpace(rawNode), []byte("null")) {
				continue
			}
			var node linearIssueNode
			if err := json.Unmarshal(rawNode, &node); err != nil {
				return nil, &Error{
					Category: ErrCategoryResponse,
					Message:  fmt.Sprintf("invalid Linear issue node JSON: %v", err),
					Cause:    err,
				}
			}
			if node.ID != "" && a.nodeMatchesScope(node, false) {
				if err := a.completeIssueConnections(ctx, &node); err != nil {
					return nil, err
				}
			}
			allNodes = append(allNodes, node)
		}

		if !data.Issues.PageInfo.HasNextPage {
			break
		}
		cursor := ""
		if data.Issues.PageInfo.EndCursor != nil {
			cursor = strings.TrimSpace(*data.Issues.PageInfo.EndCursor)
		}
		if cursor == "" || seenCursors[cursor] {
			return nil, &Error{
				Category: ErrCategoryPagination,
				Message:  "Linear pagination hasNextPage is true with empty or repeated endCursor",
			}
		}
		seenCursors[cursor] = true
		after = cursor
	}

	return allNodes, nil
}

// TransitionIssueState resolves targetStateName for the issue's team and updates the issue state in Linear.
func (a *LinearAdapter) TransitionIssueState(ctx context.Context, issueID string, targetStateName string) (symphony.Issue, error) {
	trimmedIssueID := strings.TrimSpace(issueID)
	trimmedTarget := strings.TrimSpace(targetStateName)
	if trimmedIssueID == "" {
		return symphony.Issue{}, &Error{
			Category: ErrCategoryRequest,
			Message:  "issueID is required for TransitionIssueState",
		}
	}
	if trimmedTarget == "" {
		return symphony.Issue{}, &Error{
			Category: ErrCategoryRequest,
			Message:  "targetStateName is required for TransitionIssueState",
		}
	}

	// Resolve the actual issue before selecting a team-scoped workflow state.
	var scopeData struct {
		Issue *linearIssueNode `json:"issue"`
	}
	if err := a.doGraphQL(ctx, issueForStateWriteQuery, map[string]any{"id": trimmedIssueID}, &scopeData); err != nil {
		return symphony.Issue{}, err
	}
	node := scopeData.Issue
	if node == nil || strings.TrimSpace(node.ID) == "" || (node.ID != trimmedIssueID && !strings.EqualFold(node.Identifier, trimmedIssueID)) {
		return symphony.Issue{}, connectionError(ErrCategoryResponse, "Linear state transition issue was missing or changed")
	}
	if _, valid := a.normalizeIssueNode(*node); !valid {
		return symphony.Issue{}, connectionError(ErrCategoryResponse, "Linear state transition issue is malformed")
	}
	if !a.nodeMatchesScope(*node, true) {
		return symphony.Issue{}, connectionError(ErrCategoryRequest, "issue outside configured tracker scope")
	}
	teamID, teamKey := strings.TrimSpace(node.Team.ID), strings.TrimSpace(node.Team.Key)
	if teamID == "" {
		return symphony.Issue{}, connectionError(ErrCategoryResponse, "Linear state transition requires the issue's actual team ID")
	}
	mapped, err := a.transitionTargetState(trimmedTarget)
	if err != nil {
		return symphony.Issue{}, err
	}
	trimmedTarget = mapped
	stateID, err := a.resolveWorkflowStateID(ctx, trimmedTarget, teamID, teamKey)
	if err != nil {
		return symphony.Issue{}, err
	}

	var data struct {
		IssueUpdate *struct {
			Success bool             `json:"success"`
			Issue   *linearIssueNode `json:"issue"`
		} `json:"issueUpdate"`
	}
	err = a.doGraphQL(ctx, issueUpdateStateMutation, map[string]any{
		"id":      node.ID,
		"stateId": stateID,
		"input":   map[string]any{"stateId": stateID},
	}, &data)
	if err != nil {
		return symphony.Issue{}, err
	}
	if data.IssueUpdate == nil || !data.IssueUpdate.Success || data.IssueUpdate.Issue == nil {
		return symphony.Issue{}, &Error{
			Category: ErrCategoryResponse,
			Message:  "Linear issueUpdate failed or returned empty issue",
		}
	}
	updated := data.IssueUpdate.Issue
	if updated.ID != node.ID || updated.Team.ID != teamID || !a.nodeMatchesScope(*updated, true) || updated.State.ID != stateID || symphony.NormalizeState(updated.State.Name) != symphony.NormalizeState(trimmedTarget) {
		return symphony.Issue{}, connectionError(ErrCategoryResponse, "Linear state transition returned a different issue, team or state")
	}
	if err := a.completeIssueConnections(ctx, updated); err != nil {
		return symphony.Issue{}, err
	}
	issue, ok := a.normalizeIssueNode(*updated)
	if !ok {
		return symphony.Issue{}, &Error{
			Category: ErrCategoryResponse,
			Message:  "Linear issueUpdate returned malformed issue",
		}
	}
	return issue, nil
}

func (a *LinearAdapter) transitionTargetState(target string) (string, error) {
	if value, ok := a.stateMap[strings.ToLower(strings.TrimSpace(target))]; ok {
		return value, nil
	}
	compact := func(value string) string {
		return strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.ToLower(strings.TrimSpace(value)))
	}
	match := ""
	for key, value := range a.stateMap {
		if compact(key) == compact(target) {
			if match != "" && match != value {
				return "", connectionError(ErrCategoryInvalidConfig, "ambiguous state_map transition alias")
			}
			match = value
		}
	}
	if match != "" {
		return match, nil
	}
	return target, nil
}
func (a *LinearAdapter) resolveWorkflowStateID(ctx context.Context, targetStateName, teamID, teamKey string) (string, error) {
	if teamID == "" {
		return "", connectionError(ErrCategoryResponse, "workflow state resolution requires an actual team ID")
	}
	normTarget := symphony.NormalizeState(targetStateName)
	cacheScope := teamID
	cacheKey := cacheScope + "|" + normTarget
	a.stateCacheMu.RLock()
	cached := a.stateCache[cacheKey]
	a.stateCacheMu.RUnlock()
	if cached != "" {
		return cached, nil
	}
	filter := map[string]any{"team": map[string]any{"id": map[string]any{"eq": teamID}}}
	pending := map[string]string{}
	seen := map[string]bool{}
	var after any
	for page := 0; ; page++ {
		if page >= linearNestedPageLimit {
			return "", connectionError(ErrCategoryPagination, "Linear workflow states exceed pagination bound")
		}
		var data struct {
			WorkflowStates *struct {
				PageInfo *linearPageInfo `json:"pageInfo"`
				Nodes    []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Team struct {
						ID  string `json:"id"`
						Key string `json:"key"`
					} `json:"team"`
				} `json:"nodes"`
			} `json:"workflowStates"`
		}
		if err := a.doGraphQL(ctx, workflowStatesQuery, map[string]any{"first": linearPageSize, "after": after, "filter": filter}, &data); err != nil {
			return "", err
		}
		if data.WorkflowStates == nil || data.WorkflowStates.PageInfo == nil || data.WorkflowStates.PageInfo.HasNextPage == nil || data.WorkflowStates.Nodes == nil {
			return "", connectionError(ErrCategoryResponse, "Linear workflow state connection is incomplete")
		}
		for _, state := range data.WorkflowStates.Nodes {
			if state.Team.ID == "" {
				return "", connectionError(ErrCategoryResponse, "Linear workflow state is missing its team")
			}
			if state.Team.ID != teamID {
				continue
			}
			if teamKey != "" && !strings.EqualFold(state.Team.Key, teamKey) {
				return "", connectionError(ErrCategoryResponse, "Linear workflow state team identity changed")
			}
			id, name := strings.TrimSpace(state.ID), symphony.NormalizeState(state.Name)
			if id == "" || name == "" {
				return "", connectionError(ErrCategoryResponse, "malformed Linear workflow state")
			}
			if prior := pending[name]; prior != "" && prior != id {
				return "", connectionError(ErrCategoryResponse, "ambiguous Linear workflow state name in issue team")
			}
			pending[name] = id
		}
		info := data.WorkflowStates.PageInfo
		if !*info.HasNextPage {
			break
		}
		cursor := ""
		if info.EndCursor != nil {
			cursor = strings.TrimSpace(*info.EndCursor)
		}
		if cursor == "" || seen[cursor] {
			return "", connectionError(ErrCategoryPagination, "Linear workflow states have missing or repeated endCursor")
		}
		seen[cursor] = true
		after = cursor
	}
	matched := pending[normTarget]
	if matched == "" {
		return "", connectionError(ErrCategoryResponse, "requested Linear workflow state not found in issue team")
	}
	// Never cache a partial catalog: every continuation must have succeeded.
	a.stateCacheMu.Lock()
	for name, id := range pending {
		a.stateCache[cacheScope+"|"+name] = id
	}
	a.stateCacheMu.Unlock()
	return matched, nil
}

// AddIssueComment creates a Markdown comment on the specified Linear issue and returns the comment ID.
func (a *LinearAdapter) AddIssueComment(ctx context.Context, issueID string, body string) (string, error) {
	trimmedIssueID := strings.TrimSpace(issueID)
	trimmedBody := strings.TrimSpace(body)
	if trimmedIssueID == "" {
		return "", &Error{
			Category: ErrCategoryRequest,
			Message:  "issueID is required for AddIssueComment",
		}
	}
	if trimmedBody == "" {
		return "", &Error{
			Category: ErrCategoryRequest,
			Message:  "comment body is required for AddIssueComment",
		}
	}

	var data struct {
		CommentCreate *struct {
			Success bool `json:"success"`
			Comment *struct {
				ID string `json:"id"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	err := a.doGraphQL(ctx, commentCreateMutation, map[string]any{
		"issueId": trimmedIssueID,
		"body":    body,
		"input": map[string]any{
			"issueId": trimmedIssueID,
			"body":    body,
		},
	}, &data)
	if err != nil {
		return "", err
	}
	if data.CommentCreate == nil || !data.CommentCreate.Success || data.CommentCreate.Comment == nil || strings.TrimSpace(data.CommentCreate.Comment.ID) == "" {
		return "", &Error{
			Category: ErrCategoryResponse,
			Message:  "Linear commentCreate failed or returned empty comment ID",
		}
	}
	return strings.TrimSpace(data.CommentCreate.Comment.ID), nil
}

type linearGraphQLRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables,omitempty"`
}

type linearGraphQLError struct {
	Message    string         `json:"message"`
	Extensions map[string]any `json:"extensions,omitempty"`
}

type linearGraphQLResponse struct {
	Data   json.RawMessage      `json:"data"`
	Errors []linearGraphQLError `json:"errors"`
}

func (a *LinearAdapter) doGraphQL(ctx context.Context, query string, variables map[string]any, out any) error {
	payload, err := json.Marshal(linearGraphQLRequest{
		Query:     query,
		Variables: variables,
	})
	if err != nil {
		return &Error{
			Category: ErrCategoryRequest,
			Message:  fmt.Sprintf("failed to marshal GraphQL request: %v", err),
			Cause:    err,
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(payload))
	if err != nil {
		return &Error{
			Category: ErrCategoryRequest,
			Message:  fmt.Sprintf("failed to construct Linear HTTP request: %v", err),
			Cause:    err,
		}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", a.apiKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return &Error{
			Category:  ErrCategoryRequest,
			Message:   "Linear HTTP request failed",
			Retryable: true,
			Cause:     err,
		}
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return &Error{
			Category:  ErrCategoryRequest,
			Message:   "failed to read Linear HTTP response body",
			Retryable: true,
			Cause:     err,
		}
	}

	retryAfterMS := parseRetryAfterMS(resp.Header.Get("Retry-After"))
	if resp.StatusCode == http.StatusTooManyRequests {
		return &Error{
			Category:     ErrCategoryRateLimited,
			Message:      "Linear API rate limit exceeded (HTTP 429)",
			Retryable:    true,
			RetryAfterMS: retryAfterMS,
			StatusCode:   resp.StatusCode,
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &Error{
			Category:   ErrCategoryStatus,
			Message:    fmt.Sprintf("Linear API returned HTTP %d", resp.StatusCode),
			Retryable:  resp.StatusCode >= 500,
			StatusCode: resp.StatusCode,
		}
	}

	var gqlResp linearGraphQLResponse
	if err := json.Unmarshal(bodyBytes, &gqlResp); err != nil {
		return &Error{
			Category: ErrCategoryResponse,
			Message:  "invalid JSON in Linear GraphQL response",
			Cause:    err,
		}
	}

	if len(gqlResp.Errors) > 0 {
		for _, gqlErr := range gqlResp.Errors {
			if isGraphQLRateLimitError(gqlErr) {
				return &Error{
					Category:     ErrCategoryRateLimited,
					Message:      "Linear GraphQL rate limit exceeded",
					Retryable:    true,
					RetryAfterMS: retryAfterMS,
					StatusCode:   resp.StatusCode,
				}
			}
		}
		return &Error{
			Category: ErrCategoryResponse,
			Message:  "Linear GraphQL request returned errors",
		}
	}

	if len(bytes.TrimSpace(gqlResp.Data)) == 0 || bytes.Equal(bytes.TrimSpace(gqlResp.Data), []byte("null")) {
		return &Error{
			Category: ErrCategoryResponse,
			Message:  "Linear GraphQL response missing data object",
		}
	}

	if out != nil {
		if err := json.Unmarshal(gqlResp.Data, out); err != nil {
			return &Error{
				Category: ErrCategoryResponse,
				Message:  fmt.Sprintf("failed to decode Linear GraphQL data: %v", err),
				Cause:    err,
			}
		}
	}
	return nil
}

func isGraphQLRateLimitError(gqlErr linearGraphQLError) bool {
	if gqlErr.Extensions != nil {
		if rawCode, ok := gqlErr.Extensions["code"].(string); ok {
			code := strings.ToUpper(strings.TrimSpace(rawCode))
			if code == "RATELIMITED" || code == "RATE_LIMITED" || code == "TOO_MANY_REQUESTS" {
				return true
			}
		}
		if rawType, ok := gqlErr.Extensions["type"].(string); ok {
			t := strings.ToUpper(strings.TrimSpace(rawType))
			if t == "RATELIMITED" || t == "RATE_LIMITED" {
				return true
			}
		}
	}
	msg := strings.ToLower(gqlErr.Message)
	return strings.Contains(msg, "rate limit") || strings.Contains(msg, "ratelimit")
}

func parseRetryAfterMS(val string) int {
	trimmed := strings.TrimSpace(val)
	if trimmed == "" {
		return 0
	}
	if secs, err := strconv.Atoi(trimmed); err == nil && secs > 0 {
		return secs * 1000
	}
	return 0
}

type linearPriority struct {
	Val *int
}

func (p *linearPriority) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		p.Val = nil
		return nil
	}
	var n int
	if err := json.Unmarshal(trimmed, &n); err == nil {
		p.Val = &n
		return nil
	}
	// Non-integer priority values (e.g. "high", 2.5, {}) degrade to nil per Symphony §11.3.
	p.Val = nil
	return nil
}

type linearIssueNode struct {
	CollectionsIncomplete bool           `json:"-"`
	ID                    string         `json:"id"`
	Identifier            string         `json:"identifier"`
	Number                *int           `json:"number"`
	Title                 string         `json:"title"`
	Description           *string        `json:"description"`
	Priority              linearPriority `json:"priority"`
	BranchName            *string        `json:"branchName"`
	URL                   *string        `json:"url"`
	CreatedAt             *string        `json:"createdAt"`
	UpdatedAt             *string        `json:"updatedAt"`
	State                 struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
	Assignee *struct {
		ID string `json:"id"`
	} `json:"assignee"`
	Team struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	} `json:"team"`
	Project struct {
		ID     string `json:"id"`
		SlugID string `json:"slugId"`
	} `json:"project"`
	Parent           *linearHierarchyIssueRef       `json:"parent"`
	Children         linearHierarchyIssueConnection `json:"children"`
	Labels           linearLabelConnection          `json:"labels"`
	Relations        linearRelationConnection       `json:"relations"`
	InverseRelations linearRelationConnection       `json:"inverseRelations"`
	Comments         linearCommentConnection        `json:"comments"`
}

type linearHierarchyIssueRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	Title      string `json:"title"`
	State      struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
}

type linearHierarchyIssueConnection struct {
	PageInfo  *linearPageInfo           `json:"pageInfo"`
	Malformed bool                      `json:"-"`
	Nodes     []linearHierarchyIssueRef `json:"nodes"`
}

func (c *linearHierarchyIssueConnection) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		c.Malformed = true
		return nil
	}
	var obj struct {
		PageInfo *linearPageInfo           `json:"pageInfo"`
		Nodes    []linearHierarchyIssueRef `json:"nodes"`
	}
	if err := json.Unmarshal(trimmed, &obj); err == nil && obj.Nodes != nil {
		c.Nodes = obj.Nodes
		c.PageInfo = obj.PageInfo
		return nil
	}
	var arr []linearHierarchyIssueRef
	if err := json.Unmarshal(trimmed, &arr); err == nil {
		c.Nodes = arr
		return nil
	}
	c.Nodes = nil
	c.Malformed = true
	return nil
}

type linearLabelConnection struct {
	PageInfo  *linearPageInfo `json:"pageInfo"`
	Malformed bool            `json:"-"`
	Nodes     []struct {
		Name string `json:"name"`
	} `json:"nodes"`
}

func (c *linearLabelConnection) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		c.Malformed = true
		return nil
	}
	var obj struct {
		PageInfo *linearPageInfo `json:"pageInfo"`
		Nodes    []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(trimmed, &obj); err == nil && obj.Nodes != nil {
		c.Nodes = obj.Nodes
		c.PageInfo = obj.PageInfo
		return nil
	}
	var arrObj []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(trimmed, &arrObj); err == nil {
		c.Nodes = arrObj
		return nil
	}
	var arrStr []string
	if err := json.Unmarshal(trimmed, &arrStr); err == nil {
		c.Nodes = make([]struct {
			Name string `json:"name"`
		}, len(arrStr))
		for i, s := range arrStr {
			c.Nodes[i].Name = s
		}
		return nil
	}
	c.Nodes = nil
	c.Malformed = true
	return nil
}

type linearRelatedIssueRef struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
	State      struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"state"`
}

type linearRelationNode struct {
	Type         string                 `json:"type"`
	Issue        *linearRelatedIssueRef `json:"issue"`
	RelatedIssue *linearRelatedIssueRef `json:"relatedIssue"`
}

type linearRelationConnection struct {
	PageInfo  *linearPageInfo      `json:"pageInfo"`
	Malformed bool                 `json:"-"`
	Nodes     []linearRelationNode `json:"nodes"`
}

func (c *linearRelationConnection) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		c.Malformed = true
		return nil
	}
	var obj struct {
		PageInfo *linearPageInfo      `json:"pageInfo"`
		Nodes    []linearRelationNode `json:"nodes"`
	}
	if err := json.Unmarshal(trimmed, &obj); err == nil && obj.Nodes != nil {
		c.Nodes = obj.Nodes
		c.PageInfo = obj.PageInfo
		return nil
	}
	var arr []linearRelationNode
	if err := json.Unmarshal(trimmed, &arr); err == nil {
		c.Nodes = arr
		return nil
	}
	c.Nodes = nil
	c.Malformed = true
	return nil
}

type linearCommentNode struct {
	ID        string `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

type linearCommentConnection struct {
	Nodes []linearCommentNode `json:"nodes"`
}

// FetchTrackerIssuesByStates queries Linear for issues matching stateNames and returns tracker.Issue records
// with normalized Parent and Children hierarchy references.
func (a *LinearAdapter) FetchTrackerIssuesByStates(ctx context.Context, stateNames []string) ([]Issue, error) {
	issues, err := a.FetchIssuesByStates(ctx, stateNames)
	if err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(issues))
	for _, iss := range issues {
		out = append(out, ToTrackerIssue(iss))
	}
	return out, nil
}

// FetchTrackerIssuesByIDs queries Linear for the given issue IDs and returns tracker.Issue records
// with normalized Parent and Children hierarchy references.
func (a *LinearAdapter) FetchTrackerIssuesByIDs(ctx context.Context, issueIDs []string) ([]Issue, error) {
	issues, err := a.FetchIssuesByIDs(ctx, issueIDs)
	if err != nil {
		return nil, err
	}
	out := make([]Issue, 0, len(issues))
	for _, iss := range issues {
		out = append(out, ToTrackerIssue(iss))
	}
	return out, nil
}

func (a *LinearAdapter) normalizeTrackerIssueNode(node linearIssueNode) (Issue, bool) {
	id := strings.TrimSpace(node.ID)
	identifier := strings.TrimSpace(node.Identifier)
	title := strings.TrimSpace(node.Title)
	stateName := strings.TrimSpace(node.State.Name)
	if id == "" || identifier == "" || title == "" || stateName == "" {
		return Issue{}, false
	}

	var priority *int
	if node.Priority.Val != nil && *node.Priority.Val >= 1 && *node.Priority.Val <= 4 {
		p := *node.Priority.Val
		priority = &p
	}

	var assigneeID *string
	if node.Assignee != nil {
		if aid := strings.TrimSpace(node.Assignee.ID); aid != "" {
			assigneeID = &aid
		}
	}

	labels := make([]string, 0, len(node.Labels.Nodes))
	seenLabels := make(map[string]bool, len(node.Labels.Nodes))
	for _, l := range node.Labels.Nodes {
		norm := strings.ToLower(strings.TrimSpace(l.Name))
		if norm == "" || seenLabels[norm] {
			continue
		}
		seenLabels[norm] = true
		labels = append(labels, norm)
	}

	blockedBy := a.extractBlockers(node)
	dispatchable := !node.CollectionsIncomplete && !node.InverseRelations.Malformed && a.computeDispatchable(blockedBy, assigneeID)

	var parentRef *IssueRef
	if node.Parent != nil {
		pid := strings.TrimSpace(node.Parent.ID)
		pident := strings.TrimSpace(node.Parent.Identifier)
		ptitle := strings.TrimSpace(node.Parent.Title)
		pstate := strings.TrimSpace(node.Parent.State.Name)
		if pid != "" || pident != "" {
			parentRef = &IssueRef{
				ID:         pid,
				Identifier: pident,
				Title:      ptitle,
				State:      pstate,
			}
		}
	}

	var childrenRefs []IssueRef
	if len(node.Children.Nodes) > 0 {
		seenChild := make(map[string]bool, len(node.Children.Nodes))
		for _, ch := range node.Children.Nodes {
			cid := strings.TrimSpace(ch.ID)
			cident := strings.TrimSpace(ch.Identifier)
			ctitle := strings.TrimSpace(ch.Title)
			cstate := strings.TrimSpace(ch.State.Name)
			if cid == "" && cident == "" {
				continue
			}
			key := cid
			if key == "" {
				key = cident
			}
			if seenChild[key] {
				continue
			}
			seenChild[key] = true
			childrenRefs = append(childrenRefs, IssueRef{
				ID:         cid,
				Identifier: cident,
				Title:      ctitle,
				State:      cstate,
			})
		}
	}

	nativeRef := make(map[string]any)
	if v := strings.TrimSpace(node.Team.ID); v != "" {
		nativeRef["team_id"] = v
	}
	if v := strings.TrimSpace(node.Team.Key); v != "" {
		nativeRef["team_key"] = v
	}
	if v := strings.TrimSpace(node.State.ID); v != "" {
		nativeRef["state_id"] = v
	}
	if v := strings.TrimSpace(node.State.Type); v != "" {
		nativeRef["state_type"] = v
	}
	if v := strings.TrimSpace(node.Project.ID); v != "" {
		nativeRef["project_id"] = v
	}
	if node.Number != nil {
		nativeRef["number"] = *node.Number
	}
	if parentRef != nil {
		cp := *parentRef
		nativeRef["parent"] = &cp
	}
	if len(childrenRefs) > 0 {
		cp := make([]IssueRef, len(childrenRefs))
		copy(cp, childrenRefs)
		nativeRef["children"] = cp
	}
	if len(nativeRef) == 0 {
		nativeRef = nil
	}

	base := symphony.Issue{
		ID:           id,
		NativeRef:    nativeRef,
		Identifier:   identifier,
		Title:        title,
		Description:  cleanOptionalString(node.Description),
		Priority:     priority,
		State:        stateName,
		BranchName:   cleanOptionalString(node.BranchName),
		URL:          cleanOptionalString(node.URL),
		AssigneeID:   assigneeID,
		Labels:       labels,
		BlockedBy:    blockedBy,
		Dispatchable: dispatchable,
		CreatedAt:    parseOptionalTime(node.CreatedAt),
		UpdatedAt:    parseOptionalTime(node.UpdatedAt),
	}

	return Issue{
		Issue:    base,
		Parent:   parentRef,
		Children: childrenRefs,
	}, true
}

func (a *LinearAdapter) normalizeIssueNode(node linearIssueNode) (symphony.Issue, bool) {
	iss, ok := a.normalizeTrackerIssueNode(node)
	if !ok {
		return symphony.Issue{}, false
	}
	return iss.Issue, true
}

func (a *LinearAdapter) extractBlockers(node linearIssueNode) []symphony.BlockerRef {
	out := make([]symphony.BlockerRef, 0)
	seen := make(map[string]int)

	appendBlocker := func(ref *linearRelatedIssueRef) {
		if ref == nil {
			return
		}
		id := cleanOptionalString(&ref.ID)
		ident := cleanOptionalString(&ref.Identifier)
		state := cleanOptionalString(&ref.State.Name)
		if id == nil && ident == nil && state == nil {
			return
		}
		dedupKey := ""
		if id != nil {
			dedupKey = "id:" + *id
		} else if ident != nil {
			dedupKey = "ident:" + *ident
		}
		terminal := symphony.StateInList(ref.State.Name, a.terminalStates) || strings.EqualFold(strings.TrimSpace(ref.State.Type), "completed") || strings.EqualFold(strings.TrimSpace(ref.State.Type), "canceled")
		if dedupKey != "" {
			if index, exists := seen[dedupKey]; exists {
				prior := &out[index]
				if prior.State == nil || state == nil || *prior.State != *state || prior.Terminal == nil || *prior.Terminal != terminal {
					prior.State = nil
					prior.Terminal = nil
				}
				return
			}
			seen[dedupKey] = len(out)
		}
		out = append(out, symphony.BlockerRef{
			ID:         id,
			Identifier: ident,
			State:      state,
			Terminal:   &terminal,
		})
	}

	for _, rel := range node.InverseRelations.Nodes {
		kind := strings.ToLower(strings.TrimSpace(rel.Type))
		switch kind {
		case "blocks":
			if rel.Issue == nil {
				out = append(out, symphony.BlockerRef{})
			} else if strings.TrimSpace(rel.Issue.ID) == "" || strings.TrimSpace(rel.Issue.State.Name) == "" {
				out = append(out, symphony.BlockerRef{ID: cleanOptionalString(&rel.Issue.ID), Identifier: cleanOptionalString(&rel.Issue.Identifier)})
			} else {
				appendBlocker(rel.Issue)
			}
		case "duplicate", "related", "similar":
		default:
			out = append(out, symphony.BlockerRef{})
		}
	}

	return out
}

func (a *LinearAdapter) computeDispatchable(blockedBy []symphony.BlockerRef, assigneeID *string) bool {
	if a.assigneeID != "" {
		if assigneeID == nil || *assigneeID != a.assigneeID {
			return false
		}
	}
	if !symphony.BlockersCleared(symphony.Issue{BlockedBy: blockedBy}, a.terminalStates) {
		return false
	}

	return true
}

func cleanOptionalString(s *string) *string {
	if s == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func parseOptionalTime(raw *string) *time.Time {
	if raw == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		utc := t.UTC()
		return &utc
	}
	if t, err := time.Parse(time.RFC3339, trimmed); err == nil {
		utc := t.UTC()
		return &utc
	}
	return nil
}
