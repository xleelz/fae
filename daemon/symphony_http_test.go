package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/symphony"
)

func TestSymphonyHTTPServerEndpointsAndSpecCompliance(t *testing.T) {
	adapter := newFakeLinearAdapter()
	adapter.putIssue(symphony.Issue{
		ID: "abc123", Identifier: "MT-649", Title: "Active issue", State: "In Progress", Dispatchable: true,
		URL: strPtr("https://linear.app/team/issue/MT-649"),
	})
	adapter.putIssue(symphony.Issue{
		ID: "def456", Identifier: "MT-650", Title: "Retrying issue", State: "Todo", Dispatchable: true,
		URL: strPtr("https://linear.app/team/issue/MT-650"),
	})

	wsRoot := t.TempDir()
	fixedNow := time.Date(2026, 2, 24, 20, 15, 30, 0, time.UTC)

	orch := NewLinearOrchestrator(OrchestratorConfig{
		Adapter: adapter,
		ServiceConfig: symphony.ServiceConfig{
			Workspace: symphony.WorkspaceConfig{Root: wsRoot},
			Agent:     symphony.AgentConfig{MaxConcurrentAgents: 2},
		},
		Now: func() time.Time { return fixedNow },
	})

	if err := orch.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Record event and tokens on MT-649 (abc123), and fail MT-650 (def456) into the retry queue.
	orch.RecordAgentEvent("abc123", AgentEvent{
		Event:     "turn_completed",
		SessionID: "thread-1-turn-1",
		Message:   "Working on tests",
		Timestamp: fixedNow.Add(-31 * time.Second),
		Usage: &TokenUsagePayload{
			TotalTokenUsage: &TokenCounts{
				InputTokens:  1200,
				OutputTokens: 800,
				TotalTokens:  2000,
			},
		},
	})
	orch.OnWorkerExit("def456", false, errors.New("no available orchestrator slots"))

	srv := NewSymphonyHTTPServer(orch, "127.0.0.1", 0)
	if err := srv.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	defer func() {
		_ = srv.Close(context.Background())
	}()

	baseURL := srv.URL()
	client := &http.Client{Timeout: 5 * time.Second}

	// 1. GET / (HTML dashboard)
	resp, err := client.Get(baseURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", resp.StatusCode)
	}
	htmlBody := string(bodyBytes)
	for _, needle := range []string{"MT-649", "MT-650", "Working on tests", "no available orchestrator slots"} {
		if !strings.Contains(htmlBody, needle) {
			t.Fatalf("dashboard HTML missing %q:\n%s", needle, htmlBody)
		}
	}

	// 2. GET /api/v1/state (JSON state snapshot)
	resp, err = client.Get(baseURL + "/api/v1/state")
	if err != nil {
		t.Fatal(err)
	}
	var statePayload SymphonyStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&statePayload); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/state status = %d, want 200", resp.StatusCode)
	}
	if statePayload.Counts.Running != 1 || statePayload.Counts.Retrying != 1 {
		t.Fatalf("unexpected counts: %+v", statePayload.Counts)
	}
	if len(statePayload.Running) != 1 || statePayload.Running[0].IssueIdentifier != "MT-649" || statePayload.Running[0].Tokens.TotalTokens != 2000 {
		t.Fatalf("unexpected running entry: %+v", statePayload.Running)
	}
	if statePayload.Running[0].IssueURL == nil || *statePayload.Running[0].IssueURL != "https://linear.app/team/issue/MT-649" {
		t.Fatalf("expected Running[0].IssueURL populated, got %+v", statePayload.Running[0].IssueURL)
	}
	if len(statePayload.Retrying) != 1 || statePayload.Retrying[0].IssueIdentifier != "MT-650" {
		t.Fatalf("unexpected retrying entry: %+v", statePayload.Retrying)
	}
	if statePayload.Retrying[0].IssueURL == nil || *statePayload.Retrying[0].IssueURL != "https://linear.app/team/issue/MT-650" {
		t.Fatalf("expected Retrying[0].IssueURL populated, got %+v", statePayload.Retrying[0].IssueURL)
	}

	// 3. GET /api/v1/MT-649 (200 OK per-issue detail)
	resp, err = client.Get(baseURL + "/api/v1/MT-649")
	if err != nil {
		t.Fatal(err)
	}
	var detailPayload SymphonyIssueDetailResponse
	if err := json.NewDecoder(resp.Body).Decode(&detailPayload); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/MT-649 status = %d, want 200", resp.StatusCode)
	}
	if detailPayload.IssueIdentifier != "MT-649" || detailPayload.IssueID != "abc123" || detailPayload.Status != "running" {
		t.Fatalf("unexpected issue detail: %+v", detailPayload)
	}
	if detailPayload.Running == nil || detailPayload.Running.SessionID != "thread-1-turn-1" || detailPayload.Running.Tokens.InputTokens != 1200 {
		t.Fatalf("unexpected running detail: %+v", detailPayload.Running)
	}

	// 4. GET /api/v1/MT-999 (404 Not Found with issue_not_found error envelope)
	resp, err = client.Get(baseURL + "/api/v1/MT-999")
	if err != nil {
		t.Fatal(err)
	}
	var errEnv struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errEnv); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound || errEnv.Error.Code != "issue_not_found" {
		t.Fatalf("expected 404 issue_not_found, got status=%d env=%+v", resp.StatusCode, errEnv)
	}

	// 5. POST /api/v1/refresh (202 Accepted + coalescing)
	orch.refreshMu.Lock()
	orch.refreshInProgress = true
	orch.refreshMu.Unlock()

	resp, err = client.Post(baseURL+"/api/v1/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var refreshPayload SymphonyRefreshResponse
	if err := json.NewDecoder(resp.Body).Decode(&refreshPayload); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || !refreshPayload.Queued || !refreshPayload.Coalesced {
		t.Fatalf("expected 202 queued+coalesced refresh response, got status=%d payload=%+v", resp.StatusCode, refreshPayload)
	}
	orch.refreshMu.Lock()
	orch.refreshInProgress = false
	orch.refreshPending = false
	orch.refreshMu.Unlock()

	resp, err = client.Post(baseURL+"/api/v1/refresh", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(resp.Body).Decode(&refreshPayload); err != nil {
		resp.Body.Close()
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted || !refreshPayload.Queued {
		t.Fatalf("expected 202 queued refresh response, got status=%d payload=%+v", resp.StatusCode, refreshPayload)
	}

	// 6. Unsupported methods return 405 Method Not Allowed.
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/"},
		{http.MethodPost, "/api/v1/state"},
		{http.MethodGet, "/api/v1/refresh"},
		{http.MethodPost, "/api/v1/MT-649"},
	} {
		req, err := http.NewRequest(tc.method, baseURL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var methodErr struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.NewDecoder(res.Body).Decode(&methodErr)
		res.Body.Close()
		if res.StatusCode != http.StatusMethodNotAllowed || methodErr.Error.Code != "method_not_allowed" {
			t.Fatalf("%s %s: expected 405 method_not_allowed, got status=%d code=%q", tc.method, tc.path, res.StatusCode, methodErr.Error.Code)
		}
	}
}
