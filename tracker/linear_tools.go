package tracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	ToolLinearGraphQL           = "linear_graphql"
	ToolLinearGetIssue          = "linear_get_issue"
	ToolLinearUpdateIssueState  = "linear_update_issue_state"
	ToolLinearCreateComment     = "linear_create_comment"
	ToolLinearAddAttachmentLink = "linear_add_attachment_link"
)

// SecretEnvironmentNames returns the environment variable names holding Linear credentials
// that must be stripped from child coding-agent processes (Symphony §10.5 & §15.3).
func (a *LinearAdapter) SecretEnvironmentNames() []string {
	defaults := []string{"LINEAR_API_KEY", "LINEAR_TOKEN", "LINEAR_API_TOKEN"}
	out := make([]string, 0, len(defaults)+1)
	seen := make(map[string]bool, len(defaults)+1)
	for _, name := range defaults {
		seen[name] = true
		out = append(out, name)
	}
	if custom := strings.TrimSpace(a.secretEnvVarName); custom != "" && !seen[custom] {
		out = append(out, custom)
	}
	return out
}

// AgentToolSpecs advertises the host-executed provider-native Linear tools (Symphony §10.5 & §11.5).
func (a *LinearAdapter) AgentToolSpecs() []ToolSpec {
	return []ToolSpec{
		{
			Name:        ToolLinearGraphQL,
			Description: "Execute a raw GraphQL query or mutation against Linear using Symphony's configured auth.",
			Mutates:     true,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "GraphQL query or mutation document to execute against Linear (single operation).",
					},
					"variables": map[string]any{
						"type":                 "object",
						"description":          "Optional GraphQL variables map.",
						"additionalProperties": true,
					},
				},
				"required":             []string{"query"},
				"additionalProperties": false,
			},
		},
		{
			Name:        ToolLinearGetIssue,
			Description: "Fetch current normalized Linear issue details, comments, and blocker relations.",
			Mutates:     false,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"issue_id": map[string]any{
						"type":        "string",
						"description": "Linear issue ID or identifier (defaults to the active issue).",
					},
				},
				"additionalProperties": false,
			},
		},
		{
			Name:        ToolLinearUpdateIssueState,
			Description: "Transition a Linear issue in the configured tracker scope to a workflow state name.",
			Mutates:     true,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"issue_id": map[string]any{
						"type":        "string",
						"description": "Linear issue ID or identifier (defaults to the active issue).",
					},
					"state": map[string]any{
						"type":        "string",
						"description": "Target Linear workflow state name (e.g. 'In Progress', 'In Review', 'Done').",
					},
				},
				"required":             []string{"state"},
				"additionalProperties": false,
			},
		},
		{
			Name:        ToolLinearCreateComment,
			Description: "Post a Markdown progress or review comment on a Linear issue in the configured scope.",
			Mutates:     true,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"issue_id": map[string]any{
						"type":        "string",
						"description": "Linear issue ID or identifier (defaults to the active issue).",
					},
					"body": map[string]any{
						"type":        "string",
						"description": "Markdown comment body.",
					},
				},
				"required":             []string{"body"},
				"additionalProperties": false,
			},
		},
		{
			Name:        ToolLinearAddAttachmentLink,
			Description: "Attach an external URL (PR, branch, commit, or verification artifact) to a Linear issue.",
			Mutates:     true,
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"issue_id": map[string]any{
						"type":        "string",
						"description": "Linear issue ID or identifier (defaults to the active issue).",
					},
					"url": map[string]any{
						"type":        "string",
						"description": "HTTPS URL to attach to the issue.",
					},
					"title": map[string]any{
						"type":        "string",
						"description": "Human-readable title for the attachment link.",
					},
				},
				"required":             []string{"url", "title"},
				"additionalProperties": false,
			},
		},
	}
}

// ExecuteAgentTool executes a provider-native Linear tool on the host using the adapter's credentials,
// enforcing configured team/project scope and returning a structured JSON-safe ToolResult (Symphony §10.5).
func (a *LinearAdapter) ExecuteAgentTool(ctx context.Context, name string, arguments map[string]any, toolCtx ToolContext) ToolResult {
	toolName := strings.TrimSpace(name)
	switch toolName {
	case ToolLinearGraphQL:
		return a.execToolLinearGraphQL(ctx, arguments)
	case ToolLinearGetIssue:
		return a.execToolGetIssue(ctx, arguments, toolCtx)
	case ToolLinearUpdateIssueState:
		return a.execToolUpdateIssueState(ctx, arguments, toolCtx)
	case ToolLinearCreateComment:
		return a.execToolCreateComment(ctx, arguments, toolCtx)
	case ToolLinearAddAttachmentLink:
		return a.execToolAddAttachmentLink(ctx, arguments, toolCtx)
	default:
		return ToolResult{
			Success: false,
			Error:   fmt.Sprintf("unsupported Linear agent tool %q", name),
		}
	}
}

const getLinearIssueQuery = `query GetLinearIssue($id: String!) {
  issue(id: $id) {` + linearIssueFieldsFragment + `
    comments(first: 25) {
      nodes {
        id
        body
        createdAt
      }
    }
  }
}`

func (a *LinearAdapter) resolveAndCheckIssueScope(ctx context.Context, arguments map[string]any, toolCtx ToolContext) (string, *linearIssueNode, error) {
	defaultID := strings.TrimSpace(toolCtx.Issue.ID)
	defaultIdent := strings.TrimSpace(toolCtx.Issue.Identifier)

	requestedID := ""
	if arguments != nil {
		if raw, ok := arguments["issue_id"]; ok && raw != nil {
			s, ok := raw.(string)
			if !ok {
				return "", nil, fmt.Errorf("issue_id must be a string")
			}
			requestedID = strings.TrimSpace(s)
		}
	}

	if requestedID == "" {
		if defaultID != "" {
			return defaultID, nil, nil
		}
		if defaultIdent != "" {
			return defaultIdent, nil, nil
		}
		return "", nil, fmt.Errorf("issue_id is required")
	}

	if (defaultID != "" && requestedID == defaultID) || (defaultIdent != "" && strings.EqualFold(requestedID, defaultIdent)) {
		if defaultID != "" {
			return defaultID, nil, nil
		}
		return requestedID, nil, nil
	}

	node, err := a.fetchSingleIssueNode(ctx, requestedID)
	if err != nil {
		return "", nil, err
	}
	if node == nil || !a.foreignNodeInScope(*node, toolCtx) {
		return "", nil, errOutOfScope
	}
	resolvedID := strings.TrimSpace(node.ID)
	if resolvedID == "" {
		resolvedID = requestedID
	}
	return resolvedID, node, nil
}

var errOutOfScope = fmt.Errorf("issue outside configured tracker scope")

func (a *LinearAdapter) foreignNodeInScope(node linearIssueNode, toolCtx ToolContext) bool {
	if !a.nodeMatchesScope(node, true) {
		return false
	}
	if a.teamID == "" && a.teamKey == "" && a.projectID == "" && a.projectSlug == "" {
		if toolCtx.Issue.NativeRef != nil {
			if ctxTeamID, ok := toolCtx.Issue.NativeRef["team_id"].(string); ok && strings.TrimSpace(ctxTeamID) != "" {
				if node.Team.ID == "" || node.Team.ID != strings.TrimSpace(ctxTeamID) {
					return false
				}
			}
			if ctxTeamKey, ok := toolCtx.Issue.NativeRef["team_key"].(string); ok && strings.TrimSpace(ctxTeamKey) != "" {
				if node.Team.Key == "" || !strings.EqualFold(node.Team.Key, strings.TrimSpace(ctxTeamKey)) {
					return false
				}
			}
		}
	}
	return true
}

func (a *LinearAdapter) fetchSingleIssueNode(ctx context.Context, idOrIdentifier string) (*linearIssueNode, error) {
	var data struct {
		Issue  *linearIssueNode `json:"issue"`
		Issues *struct {
			Nodes []linearIssueNode `json:"nodes"`
		} `json:"issues"`
	}
	err := a.doGraphQL(ctx, getLinearIssueQuery, map[string]any{
		"id": idOrIdentifier,
	}, &data)
	if err != nil {
		return nil, err
	}
	if data.Issue != nil {
		return data.Issue, nil
	}
	if data.Issues != nil {
		for i := range data.Issues.Nodes {
			n := &data.Issues.Nodes[i]
			if n.ID == idOrIdentifier || strings.EqualFold(n.Identifier, idOrIdentifier) {
				return n, nil
			}
		}
	}
	return nil, nil
}

func (a *LinearAdapter) execToolGetIssue(ctx context.Context, arguments map[string]any, toolCtx ToolContext) ToolResult {
	targetID, preFetched, err := a.resolveAndCheckIssueScope(ctx, arguments, toolCtx)
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}

	node := preFetched
	if node == nil {
		node, err = a.fetchSingleIssueNode(ctx, targetID)
		if err != nil {
			return ToolResult{Success: false, Error: err.Error()}
		}
		if node == nil {
			return ToolResult{Success: false, Error: fmt.Sprintf("Linear issue %q not found", targetID)}
		}
	}

	if !a.nodeMatchesScope(*node, false) {
		return ToolResult{Success: false, Error: errOutOfScope.Error()}
	}

	issue, ok := a.normalizeIssueNode(*node)
	if !ok {
		return ToolResult{Success: false, Error: "malformed Linear issue response"}
	}

	comments := make([]map[string]any, 0, len(node.Comments.Nodes))
	for _, c := range node.Comments.Nodes {
		comments = append(comments, map[string]any{
			"id":         c.ID,
			"body":       c.Body,
			"created_at": c.CreatedAt,
		})
	}

	return ToolResult{
		Success: true,
		Output: map[string]any{
			"issue":    issue,
			"comments": comments,
		},
	}
}

func (a *LinearAdapter) execToolUpdateIssueState(ctx context.Context, arguments map[string]any, toolCtx ToolContext) ToolResult {
	stateName, err := requiredStringArg(arguments, "state")
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}

	targetID, _, err := a.resolveAndCheckIssueScope(ctx, arguments, toolCtx)
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}

	updated, err := a.TransitionIssueState(ctx, targetID, stateName)
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}
	return ToolResult{
		Success: true,
		Output: map[string]any{
			"issue":      updated,
			"issue_id":   updated.ID,
			"identifier": updated.Identifier,
			"state":      updated.State,
		},
	}
}

func (a *LinearAdapter) execToolCreateComment(ctx context.Context, arguments map[string]any, toolCtx ToolContext) ToolResult {
	body, err := requiredStringArg(arguments, "body")
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}

	targetID, _, err := a.resolveAndCheckIssueScope(ctx, arguments, toolCtx)
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}

	commentID, err := a.AddIssueComment(ctx, targetID, body)
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}
	return ToolResult{
		Success: true,
		Output: map[string]any{
			"issue_id":   targetID,
			"comment_id": commentID,
		},
	}
}

const attachmentLinkURLMutation = `mutation AttachmentLinkURL($issueId: String!, $url: String!, $title: String) {
  attachmentLinkURL(issueId: $issueId, url: $url, title: $title) {
    success
    attachment {
      id
      url
      title
    }
  }
}`

func (a *LinearAdapter) execToolAddAttachmentLink(ctx context.Context, arguments map[string]any, toolCtx ToolContext) ToolResult {
	rawURL, err := requiredStringArg(arguments, "url")
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}
	parsedURL, parseErr := url.Parse(rawURL)
	if parseErr != nil || (parsedURL.Scheme != "https" && parsedURL.Scheme != "http") || parsedURL.Host == "" {
		return ToolResult{Success: false, Error: fmt.Sprintf("invalid attachment url %q", rawURL)}
	}

	title, err := requiredStringArg(arguments, "title")
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}

	targetID, _, err := a.resolveAndCheckIssueScope(ctx, arguments, toolCtx)
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}

	type attachmentPayload struct {
		Success    bool `json:"success"`
		Attachment *struct {
			ID    string `json:"id"`
			URL   string `json:"url"`
			Title string `json:"title"`
		} `json:"attachment"`
	}
	var data struct {
		AttachmentLinkURL *attachmentPayload `json:"attachmentLinkURL"`
		AttachmentCreate  *attachmentPayload `json:"attachmentCreate"`
	}
	err = a.doGraphQL(ctx, attachmentLinkURLMutation, map[string]any{
		"issueId": targetID,
		"url":     rawURL,
		"title":   title,
		"input": map[string]any{
			"issueId": targetID,
			"url":     rawURL,
			"title":   title,
		},
	}, &data)
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}

	res := data.AttachmentLinkURL
	if res == nil {
		res = data.AttachmentCreate
	}
	if res == nil || !res.Success || res.Attachment == nil || strings.TrimSpace(res.Attachment.ID) == "" {
		return ToolResult{
			Success: false,
			Error:   "Linear attachment mutation failed or returned empty attachment ID",
		}
	}

	return ToolResult{
		Success: true,
		Output: map[string]any{
			"issue_id":      targetID,
			"attachment_id": strings.TrimSpace(res.Attachment.ID),
			"url":           rawURL,
			"title":         title,
		},
	}
}

func requiredStringArg(arguments map[string]any, key string) (string, error) {
	if arguments == nil {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	raw, ok := arguments[key]
	if !ok || raw == nil {
		return "", fmt.Errorf("missing required argument %q", key)
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("argument %q must be a string", key)
	}
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return "", fmt.Errorf("argument %q must not be empty", key)
	}
	return trimmed, nil
}

func (a *LinearAdapter) execToolLinearGraphQL(ctx context.Context, arguments map[string]any) ToolResult {
	query, err := requiredStringArg(arguments, "query")
	if err != nil {
		return ToolResult{Success: false, Error: err.Error()}
	}

	var variables map[string]any
	if rawVars, ok := arguments["variables"]; ok && rawVars != nil {
		vm, ok := rawVars.(map[string]any)
		if !ok {
			return ToolResult{Success: false, Error: `argument "variables" must be an object`}
		}
		variables = vm
	}

	opCount := countGraphQLOperations(query)
	if opCount == 0 {
		return ToolResult{Success: false, Error: "query must contain a valid GraphQL operation"}
	}
	if opCount > 1 {
		return ToolResult{Success: false, Error: "linear_graphql only accepts a single GraphQL operation per request"}
	}

	payload, err := json.Marshal(linearGraphQLRequest{
		Query:     query,
		Variables: variables,
	})
	if err != nil {
		return ToolResult{Success: false, Error: fmt.Sprintf("failed to marshal GraphQL request: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(payload))
	if err != nil {
		return ToolResult{Success: false, Error: fmt.Sprintf("failed to construct Linear HTTP request: %v", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", a.apiKey)

	resp, err := a.httpClient.Do(req)
	if err != nil {
		return ToolResult{Success: false, Error: fmt.Sprintf("Linear HTTP request failed: %v", err)}
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return ToolResult{Success: false, Error: fmt.Sprintf("failed to read Linear HTTP response: %v", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Linear API returned HTTP %d", resp.StatusCode),
			Output: map[string]any{
				"status": resp.StatusCode,
				"body":   string(bodyBytes),
			},
		}
	}

	var decoded map[string]any
	if err := json.Unmarshal(bodyBytes, &decoded); err != nil {
		return ToolResult{Success: false, Error: "invalid JSON in Linear GraphQL response"}
	}

	out := map[string]any{
		"data": decoded["data"],
	}
	if errs, hasErrs := decoded["errors"]; hasErrs && errs != nil {
		out["errors"] = errs
		if errSlice, ok := errs.([]any); ok && len(errSlice) > 0 {
			errMsg := "Linear GraphQL returned errors"
			if firstErr, ok := errSlice[0].(map[string]any); ok {
				if msg, ok := firstErr["message"].(string); ok && strings.TrimSpace(msg) != "" {
					errMsg = strings.TrimSpace(msg)
				}
			}
			return ToolResult{
				Success: false,
				Output:  out,
				Error:   errMsg,
			}
		}
	}

	return ToolResult{
		Success: true,
		Output:  out,
	}
}

func countGraphQLOperations(doc string) int {
	n := len(doc)
	i := 0
	depth := 0
	ops := 0
	awaitingOpBrace := false

	isIdentChar := func(b byte) bool {
		return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
	}

	for i < n {
		c := doc[i]
		// Skip whitespace and commas
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ',' {
			i++
			continue
		}
		// Skip comments
		if c == '#' {
			for i < n && doc[i] != '\n' {
				i++
			}
			continue
		}
		// Skip strings
		if c == '"' {
			if i+2 < n && doc[i+1] == '"' && doc[i+2] == '"' {
				i += 3
				for i+2 < n && !(doc[i] == '"' && doc[i+1] == '"' && doc[i+2] == '"') {
					i++
				}
				i += 3
				continue
			}
			i++
			for i < n && doc[i] != '"' {
				if doc[i] == '\\' {
					i++
				}
				i++
			}
			i++
			continue
		}
		if c == '{' {
			if depth == 0 {
				if !awaitingOpBrace {
					ops++
				}
				awaitingOpBrace = false
			}
			depth++
			i++
			continue
		}
		if c == '}' {
			if depth > 0 {
				depth--
			}
			i++
			continue
		}
		if depth == 0 && isIdentChar(c) {
			start := i
			for i < n && isIdentChar(doc[i]) {
				i++
			}
			word := doc[start:i]
			switch word {
			case "query", "mutation", "subscription":
				if !awaitingOpBrace {
					ops++
					awaitingOpBrace = true
				}
			case "fragment":
				awaitingOpBrace = true
			}
			continue
		}
		i++
	}
	return ops
}
