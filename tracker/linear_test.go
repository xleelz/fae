package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xleelz/fae/symphony"
)

func assertTrackerErrorCategory(t *testing.T, err error, wantCategory string) *Error {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error with category %q, got nil", wantCategory)
	}
	var te *Error
	if !errors.As(err, &te) {
		t.Fatalf("expected *tracker.Error, got %T (%v)", err, err)
	}
	if te.Category != wantCategory {
		t.Fatalf("error category = %q, want %q (message: %s)", te.Category, wantCategory, te.Message)
	}
	return te
}

func TestNewAdapterAndConfigValidation(t *testing.T) {
	t.Parallel()

	_, err := NewAdapter(symphony.TrackerConfig{Kind: "jira"}, nil, nil)
	assertTrackerErrorCategory(t, err, ErrCategoryUnsupportedKind)

	_, err = NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"endpoint": "http://api.linear.app/graphql",
			"api_key":  "lin_api_test",
		},
	}, nil, nil)
	assertTrackerErrorCategory(t, err, ErrCategoryInvalidConfig)

	_, err = NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"api_key": "$CUSTOM_LINEAR_SECRET",
		},
	}, func(string) string { return "" }, nil)
	assertTrackerErrorCategory(t, err, ErrCategoryMissingSecret)

	_, err = NewLinearAdapter(symphony.TrackerConfig{
		Kind:     "linear",
		Provider: map[string]any{},
	}, func(string) string { return "" }, nil)
	assertTrackerErrorCategory(t, err, ErrCategoryMissingSecret)

	adapter, err := NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"endpoint":   "http://127.0.0.1:18080/graphql",
			"api_key":    "$CUSTOM_LINEAR_SECRET",
			"team_id":    "team-eng",
			"team_key":   "ENG",
			"project_id": "proj-1",
			"state_map": map[string]any{
				"review":    "Code Review",
				"escalated": "Blocked",
			},
		},
	}, func(k string) string {
		if k == "CUSTOM_LINEAR_SECRET" {
			return "lin_secret_value"
		}
		return ""
	}, nil)
	if err != nil {
		t.Fatalf("NewLinearAdapter unexpected error: %v", err)
	}
	if got := adapter.Kind(); got != "linear" {
		t.Fatalf("Kind() = %q, want linear", got)
	}
	if got := adapter.ActiveStates(); len(got) != 2 || got[0] != "Todo" || got[1] != "In Progress" {
		t.Fatalf("ActiveStates() = %v, want [Todo In Progress]", got)
	}
	if got := adapter.TerminalStates(); len(got) != 4 {
		t.Fatalf("TerminalStates() = %v, want 4 defaults", got)
	}
	if got := adapter.StateForWorkflowStage("in_progress"); got != "In Progress" {
		t.Fatalf("StateForWorkflowStage(in_progress) = %q, want In Progress", got)
	}
	if got := adapter.StateForWorkflowStage("review"); got != "Code Review" {
		t.Fatalf("StateForWorkflowStage(review) = %q, want Code Review", got)
	}
	if got := adapter.StateForWorkflowStage("escalated"); got != "Blocked" {
		t.Fatalf("StateForWorkflowStage(escalated) = %q, want Blocked", got)
	}
	if got := adapter.StateForWorkflowStage("done"); got != "Done" {
		t.Fatalf("StateForWorkflowStage(done) = %q, want Done", got)
	}

	secrets := adapter.SecretEnvironmentNames()
	wantSecrets := map[string]bool{
		"LINEAR_API_KEY":       true,
		"LINEAR_TOKEN":         true,
		"LINEAR_API_TOKEN":     true,
		"CUSTOM_LINEAR_SECRET": true,
	}
	for _, s := range secrets {
		delete(wantSecrets, s)
	}
	if len(wantSecrets) > 0 {
		t.Fatalf("SecretEnvironmentNames() = %v, missing %v", secrets, wantSecrets)
	}
}

func TestEmptyInputsMakeZeroHTTPCalls(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(linearFixtureHandler(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	adapter, err := NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"endpoint": srv.URL,
			"api_key":  "lin_key",
		},
	}, nil, srv.Client())
	if err != nil {
		t.Fatalf("NewLinearAdapter: %v", err)
	}

	issues, err := adapter.FetchIssuesByStates(context.Background(), nil)
	if err != nil || issues == nil || len(issues) != 0 {
		t.Fatalf("FetchIssuesByStates(nil) = (%v, %v), want empty non-nil slice and nil err", issues, err)
	}
	issues, err = adapter.FetchIssuesByStates(context.Background(), []string{"  "})
	if err != nil || issues == nil || len(issues) != 0 {
		t.Fatalf("FetchIssuesByStates(blank) = (%v, %v), want empty non-nil slice and nil err", issues, err)
	}
	issues, err = adapter.FetchIssuesByIDs(context.Background(), []string{})
	if err != nil || issues == nil || len(issues) != 0 {
		t.Fatalf("FetchIssuesByIDs([]) = (%v, %v), want empty non-nil slice and nil err", issues, err)
	}

	if got := calls.Load(); got != 0 {
		t.Fatalf("HTTP calls = %d, want 0 for empty inputs", got)
	}
}

func TestFetchIssuesByStatesPaginationNormalizationAndMalformedOmission(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(linearFixtureHandler(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("Authorization"); got != "lin_secret_token" {
			t.Errorf("Authorization header = %q, want lin_secret_token", got)
		}
		var req linearGraphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		after, _ := req.Variables["after"].(string)
		w.Header().Set("Content-Type", "application/json")
		if after == "" {
			_, _ = w.Write([]byte(`{
				"data": {
					"issues": {
						"pageInfo": { "hasNextPage": true, "endCursor": "cursor-page-1" },
						"nodes": [
							{
								"id": "issue-1",
								"identifier": "ENG-101",
								"number": 101,
								"title": "First issue blocked by active",
								"description": "Needs blocker first",
								"priority": 1,
								"branchName": "eng-101-first",
								"url": "https://linear.app/eng/issue/ENG-101",
								"createdAt": "2026-05-01T10:00:00Z",
								"updatedAt": "2026-05-01T11:00:00Z",
								"state": { "id": "st-todo", "name": "Todo", "type": "unstarted" },
								"assignee": { "id": "user-1" },
								"team": { "id": "team-eng", "key": "ENG" },
								"project": { "id": "proj-1", "slugId": "fae-symphony" },
								"labels": {
									"nodes": [
										{ "name": "  Backend " },
										{ "name": "backend" },
										{ "name": "P0-Urgent" },
										{ "name": "   " }
									]
								},
								"inverseRelations": {
									"nodes": [
										{
											"type": "blocks",
											"issue": {
												"id": "issue-blocker-1",
												"identifier": "ENG-99",
												"state": { "name": "In Progress", "type": "started" }
											}
										}
									]
								}
							},
							{
								"id": "issue-malformed-no-title",
								"identifier": "ENG-999",
								"title": "   ",
								"state": { "id": "st-todo", "name": "Todo" },
								"team": { "id": "team-eng", "key": "ENG" }
							}
						]
					}
				}
			}`))
			return
		}
		if after == "cursor-page-1" {
			_, _ = w.Write([]byte(`{
				"data": {
					"issues": {
						"pageInfo": { "hasNextPage": false, "endCursor": null },
						"nodes": [
							{
								"id": "issue-2",
								"identifier": "ENG-102",
								"number": 102,
								"title": "Second issue blocker done and priority 0",
								"priority": 0,
								"createdAt": "2026-05-01T12:00:00Z",
								"state": { "id": "st-ip", "name": "In Progress", "type": "started" },
								"assignee": { "id": "user-1" },
								"team": { "id": "team-eng", "key": "ENG" },
								"labels": { "nodes": [] },
								"inverseRelations": {
									"nodes": [
										{
											"type": "blocks",
											"issue": {
												"id": "issue-blocker-2",
												"identifier": "ENG-100",
												"state": { "name": "Done", "type": "completed" }
											}
										}
									]
								}
							}
						]
					}
				}
			}`))
			return
		}
		t.Fatalf("unexpected cursor %q", after)
	}))
	defer srv.Close()

	adapter, err := NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"endpoint":    srv.URL,
			"api_key":     "lin_secret_token",
			"team_id":     "team-eng",
			"assignee_id": "user-1",
		},
	}, nil, srv.Client())
	if err != nil {
		t.Fatalf("NewLinearAdapter: %v", err)
	}

	issues, err := adapter.FetchIssuesByStates(context.Background(), []string{"Todo", "In Progress"})
	if err != nil {
		t.Fatalf("FetchIssuesByStates error: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("expected 2 paginated GraphQL calls, got %d", got)
	}
	if len(issues) != 2 {
		t.Fatalf("len(issues) = %d, want 2 (malformed omitted)", len(issues))
	}
	if got := adapter.OmittedMalformedCount(); got != 1 {
		t.Fatalf("OmittedMalformedCount() = %d, want 1", got)
	}

	first := issues[0]
	if first.ID != "issue-1" || first.Identifier != "ENG-101" || first.State != "Todo" {
		t.Fatalf("unexpected first issue identity/state: %+v", first)
	}
	if first.Priority == nil || *first.Priority != 1 {
		t.Fatalf("first.Priority = %v, want 1", first.Priority)
	}
	if len(first.Labels) != 2 || first.Labels[0] != "backend" || first.Labels[1] != "p0-urgent" {
		t.Fatalf("first.Labels = %v, want [backend p0-urgent]", first.Labels)
	}
	if len(first.BlockedBy) != 1 || first.BlockedBy[0].Identifier == nil || *first.BlockedBy[0].Identifier != "ENG-99" {
		t.Fatalf("first.BlockedBy = %+v, want ENG-99", first.BlockedBy)
	}
	if first.Dispatchable {
		t.Fatalf("first.Dispatchable = true, want false because blocker ENG-99 is In Progress")
	}
	if first.NativeRef["team_id"] != "team-eng" || first.NativeRef["team_key"] != "ENG" || first.NativeRef["number"] != 101 {
		t.Fatalf("first.NativeRef = %+v", first.NativeRef)
	}
	if first.CreatedAt == nil || first.UpdatedAt == nil {
		t.Fatalf("expected CreatedAt and UpdatedAt to be parsed, got %v / %v", first.CreatedAt, first.UpdatedAt)
	}

	second := issues[1]
	if second.ID != "issue-2" || second.Identifier != "ENG-102" {
		t.Fatalf("unexpected second issue: %+v", second)
	}
	if second.Priority != nil {
		t.Fatalf("second.Priority = %v, want nil for Linear priority 0", *second.Priority)
	}
	if second.Labels == nil || len(second.Labels) != 0 {
		t.Fatalf("second.Labels = %v, want empty non-nil slice", second.Labels)
	}
	if !second.Dispatchable {
		t.Fatalf("second.Dispatchable = false, want true because blocker ENG-100 is Done")
	}
}

func TestBrokenPaginationCursorReturnsTrackerPaginationError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(linearFixtureHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"issues": {
					"pageInfo": { "hasNextPage": true, "endCursor": "" },
					"nodes": []
				}
			}
		}`))
	}))
	defer srv.Close()

	adapter, err := NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"endpoint": srv.URL,
			"api_key":  "lin_key",
		},
	}, nil, srv.Client())
	if err != nil {
		t.Fatalf("NewLinearAdapter: %v", err)
	}

	_, err = adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
	assertTrackerErrorCategory(t, err, ErrCategoryPagination)
}

func TestFetchIssuesByIDsScopeAndMalformedFailure(t *testing.T) {
	t.Parallel()

	var returnMalformed atomic.Bool
	srv := httptest.NewServer(linearFixtureHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if returnMalformed.Load() {
			_, _ = w.Write([]byte(`{
				"data": {
					"issues": {
						"pageInfo": { "hasNextPage": false, "endCursor": null },
						"nodes": [
							{
								"id": "issue-1",
								"identifier": "",
								"title": "Missing identifier",
								"state": { "id": "st-1", "name": "Todo" },
								"team": { "id": "team-eng", "key": "ENG" }
							}
						]
					}
				}
			}`))
			return
		}
		_, _ = w.Write([]byte(`{
			"data": {
				"issues": {
					"pageInfo": { "hasNextPage": false, "endCursor": null },
					"nodes": [
						{
							"id": "issue-1",
							"identifier": "ENG-101",
							"title": "In-scope issue",
							"state": { "id": "st-done", "name": "Done" },
							"team": { "id": "team-eng", "key": "ENG" }
						},
						{
							"id": "issue-moved",
							"identifier": "OPS-202",
							"title": "Moved to another team",
							"state": { "id": "st-todo", "name": "Todo" },
							"team": { "id": "team-ops", "key": "OPS" }
						}
					]
				}
			}
		}`))
	}))
	defer srv.Close()

	adapter, err := NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"endpoint": srv.URL,
			"api_key":  "lin_key",
			"team_id":  "team-eng",
		},
	}, nil, srv.Client())
	if err != nil {
		t.Fatalf("NewLinearAdapter: %v", err)
	}

	issues, err := adapter.FetchIssuesByIDs(context.Background(), []string{"issue-1", "issue-moved", "issue-1"})
	if err != nil {
		t.Fatalf("FetchIssuesByIDs unexpected error: %v", err)
	}
	if len(issues) != 1 || issues[0].ID != "issue-1" {
		t.Fatalf("FetchIssuesByIDs = %+v, want only in-scope issue-1", issues)
	}

	returnMalformed.Store(true)
	_, err = adapter.FetchIssuesByIDs(context.Background(), []string{"issue-1"})
	assertTrackerErrorCategory(t, err, ErrCategoryResponse)
}

func TestErrorCategoryMapping(t *testing.T) {
	t.Parallel()

	var mode atomic.Int32
	srv := httptest.NewServer(linearFixtureHandler(func(w http.ResponseWriter, r *http.Request) {
		switch mode.Load() {
		case 1:
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"errors":[{"message":"rate limit"}]}`))
		case 2:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`bad gateway`))
		case 3:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`not-json`))
		case 4:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"errors":[{"message":"Rate limit exceeded","extensions":{"code":"RATELIMITED"}}]}`))
		case 5:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"errors":[{"message":"syntax error"}]}`))
		}
	}))
	defer srv.Close()

	adapter, err := NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"endpoint": srv.URL,
			"api_key":  "lin_key",
		},
	}, nil, srv.Client())
	if err != nil {
		t.Fatalf("NewLinearAdapter: %v", err)
	}

	mode.Store(1)
	_, err = adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
	te := assertTrackerErrorCategory(t, err, ErrCategoryRateLimited)
	if te.RetryAfterMS != 4000 {
		t.Fatalf("RetryAfterMS = %d, want 4000", te.RetryAfterMS)
	}

	mode.Store(2)
	_, err = adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
	assertTrackerErrorCategory(t, err, ErrCategoryStatus)

	mode.Store(3)
	_, err = adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
	assertTrackerErrorCategory(t, err, ErrCategoryResponse)

	mode.Store(4)
	_, err = adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
	assertTrackerErrorCategory(t, err, ErrCategoryRateLimited)

	mode.Store(5)
	_, err = adapter.FetchIssuesByStates(context.Background(), []string{"Todo"})
	assertTrackerErrorCategory(t, err, ErrCategoryResponse)
}

func TestMutationsAndHostExecutedAgentTools(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(linearFixtureHandler(func(w http.ResponseWriter, r *http.Request) {
		var req linearGraphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode GraphQL: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		q := req.Query

		switch {
		case strings.Contains(q, "workflowStates"):
			_, _ = w.Write([]byte(`{
				"data": {
					"workflowStates": {
						"nodes": [
							{ "id": "st-progress", "name": "In Progress", "type": "started", "team": { "id": "team-eng", "key": "ENG" } },
							{ "id": "st-review", "name": "In Review", "type": "started", "team": { "id": "team-eng", "key": "ENG" } },
							{ "id": "st-done", "name": "Done", "type": "completed", "team": { "id": "team-eng", "key": "ENG" } }
						]
					}
				}
			}`))
		case strings.Contains(q, "issueUpdate"):
			stateID, _ := req.Variables["stateId"].(string)
			stateName := "In Progress"
			if stateID == "st-review" {
				stateName = "In Review"
			} else if stateID == "st-done" {
				stateName = "Done"
			}
			_, _ = w.Write([]byte(`{
				"data": {
					"issueUpdate": {
						"success": true,
						"issue": {
							"id": "issue-1",
							"identifier": "ENG-101",
							"title": "Active issue",
							"state": { "id": "` + stateID + `", "name": "` + stateName + `", "type": "started" },
							"team": { "id": "team-eng", "key": "ENG" }
						}
					}
				}
			}`))
		case strings.Contains(q, "commentCreate"):
			_, _ = w.Write([]byte(`{
				"data": {
					"commentCreate": {
						"success": true,
						"comment": {
							"id": "comment-42",
							"body": "Verification passed",
							"url": "https://linear.app/eng/issue/ENG-101#comment-42"
						}
					}
				}
			}`))
		case strings.Contains(q, "attachmentLinkURL"):
			_, _ = w.Write([]byte(`{
				"data": {
					"attachmentLinkURL": {
						"success": true,
						"attachment": {
							"id": "attach-99",
							"url": "https://github.com/org/repo/pull/101",
							"title": "PR #101"
						}
					}
				}
			}`))
		case strings.Contains(q, "GetLinearIssue"):
			id, _ := req.Variables["id"].(string)
			if id == "issue-foreign" || id == "OPS-999" {
				_, _ = w.Write([]byte(`{
					"data": {
						"issue": {
							"id": "issue-foreign",
							"identifier": "OPS-999",
							"title": "Foreign team issue",
							"state": { "id": "st-ops", "name": "Todo" },
							"team": { "id": "team-ops", "key": "OPS" }
						}
					}
				}`))
				return
			}
			_, _ = w.Write([]byte(`{
				"data": {
					"issue": {
						"id": "issue-1",
						"identifier": "ENG-101",
						"title": "Active issue",
						"state": { "id": "st-progress", "name": "In Progress", "type": "started" },
						"team": { "id": "team-eng", "key": "ENG" },
						"comments": {
							"nodes": [
								{ "id": "c-1", "body": "Initial context", "createdAt": "2026-05-01T10:00:00Z" }
							]
						}
					}
				}
			}`))
		default:
			t.Fatalf("unexpected query: %s", q)
		}
	}))
	defer srv.Close()

	adapter, err := NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"endpoint": srv.URL,
			"api_key":  "lin_secret_token",
			"team_id":  "team-eng",
			"team_key": "ENG",
		},
	}, nil, srv.Client())
	if err != nil {
		t.Fatalf("NewLinearAdapter: %v", err)
	}

	// Direct daemon mutations
	updated, err := adapter.TransitionIssueState(context.Background(), "issue-1", "in review")
	if err != nil {
		t.Fatalf("TransitionIssueState: %v", err)
	}
	if updated.State != "In Review" {
		t.Fatalf("updated.State = %q, want In Review", updated.State)
	}

	commentID, err := adapter.AddIssueComment(context.Background(), "issue-1", "Automated verification passed")
	if err != nil || commentID != "comment-42" {
		t.Fatalf("AddIssueComment = (%q, %v), want comment-42", commentID, err)
	}

	// Host-executed agent tools
	specs := adapter.AgentToolSpecs()
	if len(specs) != 5 {
		t.Fatalf("len(AgentToolSpecs()) = %d, want 5", len(specs))
	}

	toolCtx := ToolContext{
		Issue: symphony.Issue{
			ID:         "issue-1",
			Identifier: "ENG-101",
			Title:      "Active issue",
			State:      "In Progress",
			NativeRef:  map[string]any{"team_id": "team-eng", "team_key": "ENG"},
		},
	}

	// 1. linear_get_issue (defaults to toolCtx.Issue.ID)
	res := adapter.ExecuteAgentTool(context.Background(), ToolLinearGetIssue, map[string]any{}, toolCtx)
	if !res.Success {
		t.Fatalf("linear_get_issue failed: %s", res.Error)
	}
	comments, ok := res.Output["comments"].([]map[string]any)
	if !ok || len(comments) != 1 {
		t.Fatalf("linear_get_issue comments = %#v, want 1 comment", res.Output["comments"])
	}

	// 2. linear_update_issue_state
	res = adapter.ExecuteAgentTool(context.Background(), ToolLinearUpdateIssueState, map[string]any{
		"state": "Done",
	}, toolCtx)
	if !res.Success || res.Output["state"] != "Done" {
		t.Fatalf("linear_update_issue_state = %+v", res)
	}

	// 3. linear_create_comment
	res = adapter.ExecuteAgentTool(context.Background(), ToolLinearCreateComment, map[string]any{
		"body": "Posted via host tool",
	}, toolCtx)
	if !res.Success || res.Output["comment_id"] != "comment-42" {
		t.Fatalf("linear_create_comment = %+v", res)
	}

	// 4. linear_add_attachment_link
	res = adapter.ExecuteAgentTool(context.Background(), ToolLinearAddAttachmentLink, map[string]any{
		"url":   "https://github.com/org/repo/pull/101",
		"title": "PR #101",
	}, toolCtx)
	if !res.Success || res.Output["attachment_id"] != "attach-99" {
		t.Fatalf("linear_add_attachment_link = %+v", res)
	}

	// 5. Out-of-scope enforcement
	res = adapter.ExecuteAgentTool(context.Background(), ToolLinearUpdateIssueState, map[string]any{
		"issue_id": "OPS-999",
		"state":    "Done",
	}, toolCtx)
	if res.Success || !strings.Contains(res.Error, "outside configured tracker scope") {
		t.Fatalf("expected out-of-scope failure, got %+v", res)
	}

	// 6. Unsupported tool graceful failure
	res = adapter.ExecuteAgentTool(context.Background(), "linear_delete_workspace", map[string]any{}, toolCtx)
	if res.Success || res.Error == "" {
		t.Fatalf("expected unsupported tool error, got %+v", res)
	}
}

func TestParentAndChildrenHierarchyParsing(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(linearFixtureHandler(func(w http.ResponseWriter, r *http.Request) {
		var req linearGraphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode GraphQL: %v", err)
		}
		if !strings.Contains(req.Query, "parent {") || !strings.Contains(req.Query, "children(first: 50)") {
			t.Fatalf("expected GraphQL query to include parent and children(first: 50), got:\n%s", req.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"data": {
				"issues": {
					"pageInfo": { "hasNextPage": false, "endCursor": null },
					"nodes": [
						{
							"id": "issue-parent-epic",
							"identifier": "ENG-200",
							"number": 200,
							"title": "Parent epic with sub-issues",
							"priority": 1,
							"state": { "id": "st-ip", "name": "In Progress", "type": "started" },
							"team": { "id": "team-eng", "key": "ENG" },
							"parent": {
								"id": "issue-root-initiative",
								"identifier": "ENG-100",
								"title": "Root initiative",
								"state": { "name": "In Progress" }
							},
							"children": {
								"nodes": [
									{
										"id": "issue-sub-1",
										"identifier": "ENG-201",
										"title": "Implement engine fan-out",
										"state": { "name": "Done" }
									},
									{
										"id": "issue-sub-2",
										"identifier": "ENG-202",
										"title": "Implement daemon reconciliation",
										"state": { "name": "In Progress" }
									},
									{
										"id": "issue-sub-3",
										"identifier": "ENG-203",
										"title": "Add hierarchy tests",
										"state": { "name": "Todo" }
									}
								]
							},
							"labels": { "nodes": [{ "name": "symphony" }] }
						}
					]
				}
			}
		}`))
	}))
	defer srv.Close()

	adapter, err := NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"endpoint": srv.URL,
			"api_key":  "lin_secret_token",
			"team_id":  "team-eng",
		},
	}, nil, srv.Client())
	if err != nil {
		t.Fatalf("NewLinearAdapter: %v", err)
	}

	trackerIssues, err := adapter.FetchTrackerIssuesByStates(context.Background(), []string{"In Progress"})
	if err != nil {
		t.Fatalf("FetchTrackerIssuesByStates: %v", err)
	}
	if len(trackerIssues) != 1 {
		t.Fatalf("len(trackerIssues) = %d, want 1", len(trackerIssues))
	}
	iss := trackerIssues[0]
	if iss.Parent == nil || iss.Parent.ID != "issue-root-initiative" || iss.Parent.Identifier != "ENG-100" || iss.Parent.Title != "Root initiative" || iss.Parent.State != "In Progress" {
		t.Fatalf("unexpected Parent: %+v", iss.Parent)
	}
	if len(iss.Children) != 3 {
		t.Fatalf("len(iss.Children) = %d, want 3: %+v", len(iss.Children), iss.Children)
	}
	if iss.Children[0].Identifier != "ENG-201" || iss.Children[0].State != "Done" ||
		iss.Children[1].Identifier != "ENG-202" || iss.Children[1].State != "In Progress" ||
		iss.Children[2].Identifier != "ENG-203" || iss.Children[2].State != "Todo" {
		t.Fatalf("unexpected Children: %+v", iss.Children)
	}

	parentRef, childRefs := ExtractHierarchy(iss.ToSymphonyIssue())
	if parentRef == nil || parentRef.Identifier != "ENG-100" || len(childRefs) != 3 {
		t.Fatalf("ExtractHierarchy mismatch: parent=%+v children=%+v", parentRef, childRefs)
	}
}

func TestLinearGraphQLToolAndGracefulDegradation(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(linearFixtureHandler(func(w http.ResponseWriter, r *http.Request) {
		var req linearGraphQLRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode GraphQL: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(req.Query, "viewer"):
			_, _ = w.Write([]byte(`{"data":{"viewer":{"id":"usr-1","name":"Symphony"}}}`))
		case strings.Contains(req.Query, "triggerGqlError"):
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"Field 'triggerGqlError' doesn't exist"}]}`))
		case strings.Contains(req.Query, "FetchIssuesByStates"):
			_, _ = w.Write([]byte(`{
				"data": {
					"issues": {
						"pageInfo": { "hasNextPage": false, "endCursor": null },
						"nodes": [
							{
								"id": "iss-nonint-prio",
								"identifier": "ENG-301",
								"title": "Non-integer priority and malformed optional metadata",
								"priority": "high",
								"state": { "id": "st-1", "name": "Todo", "type": "unstarted" },
								"team": { "id": "team-eng", "key": "ENG" },
								"labels": "malformed-labels-string",
								"children": 12345,
								"relations": "bad-relations",
								"inverseRelations": {"nodes": "not-an-array"}
							},
							{
								"id": "",
								"identifier": "ENG-MALFORMED",
								"title": "Missing ID"
							}
						]
					}
				}
			}`))
		case strings.Contains(req.Query, "FetchIssuesByIDs"):
			if strings.Contains(req.Query, "$ids:") || !strings.Contains(req.Query, "$filter: IssueFilter") {
				t.Fatalf("expected FetchIssuesByIDs filter query without unused $ids variable, got:\n%s", req.Query)
			}
			_, _ = w.Write([]byte(`{
				"data": {
					"issues": {
						"pageInfo": { "hasNextPage": false, "endCursor": null },
						"nodes": [
							{
								"id": "iss-nonint-prio",
								"identifier": "ENG-301",
								"title": "Non-integer priority and malformed optional metadata",
								"priority": 2.5,
								"state": { "id": "st-2", "name": "Done", "type": "completed" },
								"team": { "id": "team-eng", "key": "ENG" }
							}
						]
					}
				}
			}`))
		default:
			t.Fatalf("unexpected query: %s", req.Query)
		}
	}))
	defer srv.Close()

	adapter, err := NewLinearAdapter(symphony.TrackerConfig{
		Kind: "linear",
		Provider: map[string]any{
			"endpoint": srv.URL,
			"api_key":  "lin_token",
			"team_id":  "team-eng",
		},
	}, nil, srv.Client())
	if err != nil {
		t.Fatalf("NewLinearAdapter: %v", err)
	}

	var loggedWarnings []string
	adapter.Logger = func(format string, args ...any) {
		loggedWarnings = append(loggedWarnings, fmt.Sprintf(format, args...))
	}

	// 1. FetchCandidateIssues with non-integer priority ("high"), malformed optional connections, and malformed record warning
	candidates, err := adapter.FetchCandidateIssues(context.Background())
	if err != nil {
		t.Fatalf("FetchCandidateIssues: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 valid candidate issue, got %d", len(candidates))
	}
	if candidates[0].Priority != nil {
		t.Fatalf("expected non-integer priority 'high' to normalize to nil, got %v", *candidates[0].Priority)
	}
	if len(loggedWarnings) == 0 {
		t.Fatal("expected warning log when omitting malformed record in FetchIssuesByStates")
	}

	// 2. FetchIssueStatesByIDs with float priority (2.5 -> nil)
	refreshed, err := adapter.FetchIssueStatesByIDs(context.Background(), []string{"iss-nonint-prio"})
	if err != nil {
		t.Fatalf("FetchIssueStatesByIDs: %v", err)
	}
	if len(refreshed) != 1 || refreshed[0].State != "Done" || refreshed[0].Priority != nil {
		t.Fatalf("unexpected FetchIssueStatesByIDs result: %+v", refreshed)
	}

	// 3. ToolLinearGraphQL single valid query
	res := adapter.ExecuteAgentTool(context.Background(), ToolLinearGraphQL, map[string]any{
		"query": "query GetViewer { viewer { id name } }",
	}, ToolContext{})
	if !res.Success || res.Output["data"] == nil {
		t.Fatalf("expected linear_graphql success, got %+v", res)
	}

	// 4. ToolLinearGraphQL with GraphQL-level errors -> Success: false, Output preserves errors
	resErr := adapter.ExecuteAgentTool(context.Background(), ToolLinearGraphQL, map[string]any{
		"query": "query { triggerGqlError }",
	}, ToolContext{})
	if resErr.Success || resErr.Output["errors"] == nil {
		t.Fatalf("expected linear_graphql Success=false with errors preserved, got %+v", resErr)
	}

	// 5. ToolLinearGraphQL rejects multiple operations
	resMulti := adapter.ExecuteAgentTool(context.Background(), ToolLinearGraphQL, map[string]any{
		"query": "query A { viewer { id } } query B { viewer { name } }",
	}, ToolContext{})
	if resMulti.Success || !strings.Contains(resMulti.Error, "single GraphQL operation") {
		t.Fatalf("expected multiple operations to be rejected, got %+v", resMulti)
	}
}
