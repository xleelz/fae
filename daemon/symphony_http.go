package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"math"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// SymphonyStateResponse is the wire format for GET /api/v1/state (Symphony §13.7.2).
type SymphonyStateResponse struct {
	GeneratedAt string                   `json:"generated_at"`
	Counts      SymphonyCounts           `json:"counts"`
	Running     []SymphonyRunningSummary `json:"running"`
	Retrying    []SymphonyRetrySummary   `json:"retrying"`
	CodexTotals CodexTotals              `json:"codex_totals"`
	RateLimits  any                      `json:"rate_limits"`
}

// SymphonyCounts reports running and retrying issue counts (Symphony §13.7.2).
type SymphonyCounts struct {
	Running  int `json:"running"`
	Retrying int `json:"retrying"`
}

// SymphonyRunningSummary describes one running issue row in /api/v1/state (Symphony §13.7.2).
type SymphonyRunningSummary struct {
	IssueID         string      `json:"issue_id"`
	IssueIdentifier string      `json:"issue_identifier"`
	IssueURL        *string     `json:"issue_url,omitempty"`
	State           string      `json:"state"`
	Stage           string      `json:"stage,omitempty"`
	TaskID          string      `json:"task_id,omitempty"`
	SessionID       string      `json:"session_id"`
	TurnCount       int         `json:"turn_count"`
	LastEvent       string      `json:"last_event"`
	LastMessage     string      `json:"last_message"`
	StartedAt       string      `json:"started_at"`
	LastEventAt     *string     `json:"last_event_at"`
	Tokens          TokenCounts `json:"tokens"`
}

// SymphonyRetrySummary describes one retry queue row in /api/v1/state (Symphony §13.7.2).
type SymphonyRetrySummary struct {
	IssueID         string  `json:"issue_id"`
	IssueIdentifier string  `json:"issue_identifier"`
	IssueURL        *string `json:"issue_url,omitempty"`
	Attempt         int     `json:"attempt"`
	DueAt           string  `json:"due_at"`
	Error           string  `json:"error"`
}

// SymphonyIssueDetailResponse is the wire format for GET /api/v1/<issue_identifier> (Symphony §13.7.2).
type SymphonyIssueDetailResponse struct {
	IssueIdentifier string                      `json:"issue_identifier"`
	IssueID         string                      `json:"issue_id"`
	Status          string                      `json:"status"`
	Workspace       SymphonyWorkspaceRef        `json:"workspace"`
	Attempts        SymphonyAttemptSummary      `json:"attempts"`
	Running         *SymphonyIssueRunningDetail `json:"running"`
	Retry           *SymphonyIssueRetryDetail   `json:"retry"`
	Logs            SymphonyLogsEnvelope        `json:"logs"`
	RecentEvents    []SymphonyEventItem         `json:"recent_events"`
	LastError       *string                     `json:"last_error"`
	Tracked         map[string]any              `json:"tracked"`
}

// SymphonyWorkspaceRef holds the per-issue workspace path.
type SymphonyWorkspaceRef struct {
	Path string `json:"path"`
}

// SymphonyAttemptSummary holds restart and current retry attempt counters.
type SymphonyAttemptSummary struct {
	RestartCount        int  `json:"restart_count"`
	CurrentRetryAttempt *int `json:"current_retry_attempt"`
}

// SymphonyIssueRunningDetail describes active session state for a single issue.
type SymphonyIssueRunningDetail struct {
	SessionID   string      `json:"session_id"`
	TurnCount   int         `json:"turn_count"`
	State       string      `json:"state"`
	Stage       string      `json:"stage,omitempty"`
	TaskID      string      `json:"task_id,omitempty"`
	StartedAt   string      `json:"started_at"`
	LastEvent   string      `json:"last_event"`
	LastMessage string      `json:"last_message"`
	LastEventAt *string     `json:"last_event_at"`
	Tokens      TokenCounts `json:"tokens"`
}

// SymphonyIssueRetryDetail describes pending retry state for a single issue.
type SymphonyIssueRetryDetail struct {
	Attempt int    `json:"attempt"`
	DueAt   string `json:"due_at"`
	Error   string `json:"error"`
}

// SymphonyLogsEnvelope holds structured session log references.
type SymphonyLogsEnvelope struct {
	CodexSessionLogs []any `json:"codex_session_logs"`
}

// SymphonyEventItem is one formatted event entry in GET /api/v1/<issue_identifier>.
type SymphonyEventItem struct {
	At      string `json:"at"`
	Event   string `json:"event"`
	Message string `json:"message"`
}

// SymphonyRefreshResponse is the wire format for POST /api/v1/refresh (Symphony §13.7.2).
type SymphonyRefreshResponse struct {
	Queued      bool     `json:"queued"`
	Coalesced   bool     `json:"coalesced"`
	RequestedAt string   `json:"requested_at"`
	Operations  []string `json:"operations"`
}

// StateSnapshot returns a point-in-time snapshot of the orchestrator state for HTTP/CLI observability.
func (o *LinearOrchestrator) StateSnapshot() SymphonyStateResponse {
	o.mu.RLock()
	defer o.mu.RUnlock()

	now := o.now().UTC()
	runningList := make([]SymphonyRunningSummary, 0, len(o.Running))
	activeRuntimeSec := 0.0
	for _, entry := range o.Running {
		if entry == nil {
			continue
		}
		var lastEventAtStr *string
		if entry.LastEventAt != nil && !entry.LastEventAt.IsZero() {
			s := entry.LastEventAt.UTC().Format(time.RFC3339)
			lastEventAtStr = &s
		}
		elapsed := now.Sub(entry.StartedAt).Seconds()
		if elapsed > 0 {
			activeRuntimeSec += elapsed
		}
		var issueURL *string
		if entry.Issue.URL != nil && strings.TrimSpace(*entry.Issue.URL) != "" {
			u := strings.TrimSpace(*entry.Issue.URL)
			issueURL = &u
		} else if known, ok := o.KnownIssues[entry.IssueID]; ok && known.URL != nil && strings.TrimSpace(*known.URL) != "" {
			u := strings.TrimSpace(*known.URL)
			issueURL = &u
		}
		runningList = append(runningList, SymphonyRunningSummary{
			IssueID:         entry.IssueID,
			IssueIdentifier: entry.Identifier,
			IssueURL:        issueURL,
			State:           entry.Issue.State,
			Stage:           entry.Stage,
			TaskID:          entry.TaskID,
			SessionID:       entry.SessionID,
			TurnCount:       entry.TurnCount,
			LastEvent:       entry.LastEvent,
			LastMessage:     entry.LastMessage,
			StartedAt:       entry.StartedAt.UTC().Format(time.RFC3339),
			LastEventAt:     lastEventAtStr,
			Tokens: TokenCounts{
				InputTokens:  entry.InputTokens,
				OutputTokens: entry.OutputTokens,
				TotalTokens:  entry.TotalTokens,
			},
		})
	}
	sort.Slice(runningList, func(i, j int) bool {
		return runningList[i].IssueIdentifier < runningList[j].IssueIdentifier
	})

	retryingList := make([]SymphonyRetrySummary, 0, len(o.RetryAttempts))
	for _, r := range o.RetryAttempts {
		if r == nil {
			continue
		}
		var retryIssueURL *string
		if known, ok := o.KnownIssues[r.IssueID]; ok && known.URL != nil && strings.TrimSpace(*known.URL) != "" {
			u := strings.TrimSpace(*known.URL)
			retryIssueURL = &u
		}
		retryingList = append(retryingList, SymphonyRetrySummary{
			IssueID:         r.IssueID,
			IssueIdentifier: r.Identifier,
			IssueURL:        retryIssueURL,
			Attempt:         r.Attempt,
			DueAt:           r.DueAt.UTC().Format(time.RFC3339),
			Error:           r.Error,
		})
	}
	sort.Slice(retryingList, func(i, j int) bool {
		if retryingList[i].DueAt != retryingList[j].DueAt {
			return retryingList[i].DueAt < retryingList[j].DueAt
		}
		return retryingList[i].IssueIdentifier < retryingList[j].IssueIdentifier
	})

	totals := o.CodexTotals
	totals.SecondsRunning = math.Round((totals.SecondsRunning+activeRuntimeSec)*10) / 10

	return SymphonyStateResponse{
		GeneratedAt: now.Format(time.RFC3339),
		Counts: SymphonyCounts{
			Running:  len(runningList),
			Retrying: len(retryingList),
		},
		Running:     runningList,
		Retrying:    retryingList,
		CodexTotals: totals,
		RateLimits:  o.RateLimits,
	}
}

// IssueDetailSnapshot returns the detailed debug snapshot for a single issue identifier (Symphony §13.7.2).
func (o *LinearOrchestrator) IssueDetailSnapshot(identifier string) (SymphonyIssueDetailResponse, bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()

	trimmed := strings.TrimSpace(identifier)
	if trimmed == "" {
		return SymphonyIssueDetailResponse{}, false
	}

	var issueID string
	var canonicalIdent string
	for id, entry := range o.Running {
		if entry != nil && strings.EqualFold(entry.Identifier, trimmed) {
			issueID = id
			canonicalIdent = entry.Identifier
			break
		}
	}
	if issueID == "" {
		for id, r := range o.RetryAttempts {
			if r != nil && strings.EqualFold(r.Identifier, trimmed) {
				issueID = id
				canonicalIdent = r.Identifier
				break
			}
		}
	}
	if issueID == "" {
		for id, iss := range o.KnownIssues {
			if strings.EqualFold(iss.Identifier, trimmed) {
				issueID = id
				canonicalIdent = iss.Identifier
				break
			}
		}
	}
	if issueID == "" {
		return SymphonyIssueDetailResponse{}, false
	}

	wsPath := filepath.Join(o.WorkspaceRoot, SanitizeWorkspaceKey(canonicalIdent))
	var runningDetail *SymphonyIssueRunningDetail
	var retryDetail *SymphonyIssueRetryDetail
	var currentAttempt *int
	status := "tracked"

	if entry, ok := o.Running[issueID]; ok && entry != nil {
		status = "running"
		if entry.WorkspacePath != "" {
			wsPath = entry.WorkspacePath
		}
		if entry.RetryAttempt != nil {
			val := *entry.RetryAttempt
			currentAttempt = &val
		}
		var lastEventAtStr *string
		if entry.LastEventAt != nil && !entry.LastEventAt.IsZero() {
			s := entry.LastEventAt.UTC().Format(time.RFC3339)
			lastEventAtStr = &s
		}
		runningDetail = &SymphonyIssueRunningDetail{
			SessionID:   entry.SessionID,
			TurnCount:   entry.TurnCount,
			State:       entry.Issue.State,
			Stage:       entry.Stage,
			TaskID:      entry.TaskID,
			StartedAt:   entry.StartedAt.UTC().Format(time.RFC3339),
			LastEvent:   entry.LastEvent,
			LastMessage: entry.LastMessage,
			LastEventAt: lastEventAtStr,
			Tokens: TokenCounts{
				InputTokens:  entry.InputTokens,
				OutputTokens: entry.OutputTokens,
				TotalTokens:  entry.TotalTokens,
			},
		}
	} else if r, ok := o.RetryAttempts[issueID]; ok && r != nil {
		status = "retrying"
		val := r.Attempt
		currentAttempt = &val
		retryDetail = &SymphonyIssueRetryDetail{
			Attempt: r.Attempt,
			DueAt:   r.DueAt.UTC().Format(time.RFC3339),
			Error:   r.Error,
		}
	} else if o.Completed[issueID] {
		status = "completed"
	}

	rawEvents := o.RecentEvents[issueID]
	events := make([]SymphonyEventItem, 0, len(rawEvents))
	for _, ev := range rawEvents {
		events = append(events, SymphonyEventItem{
			At:      ev.At.UTC().Format(time.RFC3339),
			Event:   ev.Event,
			Message: ev.Message,
		})
	}

	var lastErrPtr *string
	if errStr, ok := o.LastError[issueID]; ok && errStr != "" {
		s := errStr
		lastErrPtr = &s
	}

	tracked := make(map[string]any)
	if iss, ok := o.KnownIssues[issueID]; ok {
		tracked["state"] = iss.State
		tracked["title"] = iss.Title
		tracked["dispatchable"] = iss.Dispatchable
	}
	if taskID, ok := o.IssueToTask[issueID]; ok && taskID != "" {
		tracked["task_id"] = taskID
	}
	if stages, ok := o.StageHistory[issueID]; ok && len(stages) > 0 {
		tracked["last_stage"] = stages[len(stages)-1].Stage
		tracked["stage_transitions"] = len(stages)
	}

	return SymphonyIssueDetailResponse{
		IssueIdentifier: canonicalIdent,
		IssueID:         issueID,
		Status:          status,
		Workspace: SymphonyWorkspaceRef{
			Path: wsPath,
		},
		Attempts: SymphonyAttemptSummary{
			RestartCount:        o.RestartCounts[issueID],
			CurrentRetryAttempt: currentAttempt,
		},
		Running: runningDetail,
		Retry:   retryDetail,
		Logs: SymphonyLogsEnvelope{
			CodexSessionLogs: []any{},
		},
		RecentEvents: events,
		LastError:    lastErrPtr,
		Tracked:      tracked,
	}, true
}

// SymphonyHTTPServer hosts the Symphony observability & control HTTP extension (Symphony §13.7).
type SymphonyHTTPServer struct {
	orchestrator *LinearOrchestrator
	host         string
	port         int
	mu           sync.Mutex
	listener     net.Listener
	server       *http.Server
}

// NewSymphonyHTTPServer creates a new SymphonyHTTPServer bound to host:port (defaults host to 127.0.0.1).
func NewSymphonyHTTPServer(orchestrator *LinearOrchestrator, host string, port int) *SymphonyHTTPServer {
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
	}
	return &SymphonyHTTPServer{
		orchestrator: orchestrator,
		host:         host,
		port:         port,
	}
}

// Start binds the TCP listener and begins serving HTTP requests in the background.
func (s *SymphonyHTTPServer) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return nil
	}
	addr := net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = ln
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		s.port = tcpAddr.Port
	}
	s.server = &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = s.server.Serve(ln)
	}()
	return nil
}

// Addr returns the bound listener address (e.g. "127.0.0.1:41234").
func (s *SymphonyHTTPServer) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return net.JoinHostPort(s.host, fmt.Sprintf("%d", s.port))
}

// Port returns the bound TCP port.
func (s *SymphonyHTTPServer) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// URL returns the base http:// URL for this server.
func (s *SymphonyHTTPServer) URL() string {
	return "http://" + s.Addr()
}

// Close gracefully shuts down the HTTP server.
func (s *SymphonyHTTPServer) Close(ctx context.Context) error {
	s.mu.Lock()
	srv := s.server
	s.listener = nil
	s.server = nil
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	err := srv.Shutdown(ctx)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Handler returns the http.Handler implementing all Symphony HTTP routes.
func (s *SymphonyHTTPServer) Handler() http.Handler {
	return http.HandlerFunc(s.serveHTTP)
}

func (s *SymphonyHTTPServer) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case path == "/" || path == "":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeSymphonyError(w, http.StatusMethodNotAllowed, "method_not_allowed", fmt.Sprintf("method %s not allowed on /", r.Method))
			return
		}
		s.handleDashboard(w, r)
		return

	case path == "/api/v1/state":
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeSymphonyError(w, http.StatusMethodNotAllowed, "method_not_allowed", fmt.Sprintf("method %s not allowed on /api/v1/state", r.Method))
			return
		}
		snap := s.orchestrator.StateSnapshot()
		writeSymphonyJSON(w, http.StatusOK, snap)
		return

	case path == "/api/v1/refresh":
		if r.Method != http.MethodPost {
			writeSymphonyError(w, http.StatusMethodNotAllowed, "method_not_allowed", fmt.Sprintf("method %s not allowed on /api/v1/refresh", r.Method))
			return
		}
		now := s.orchestrator.now().UTC().Format(time.RFC3339)
		queued, coalesced := s.orchestrator.TriggerRefresh(context.Background())
		writeSymphonyJSON(w, http.StatusAccepted, SymphonyRefreshResponse{
			Queued:      queued,
			Coalesced:   coalesced,
			RequestedAt: now,
			Operations:  []string{"poll", "reconcile"},
		})
		return

	case strings.HasPrefix(path, "/api/v1/"):
		ident := strings.TrimPrefix(path, "/api/v1/")
		if ident == "" || strings.Contains(ident, "/") {
			writeSymphonyError(w, http.StatusNotFound, "not_found", "route not found")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeSymphonyError(w, http.StatusMethodNotAllowed, "method_not_allowed", fmt.Sprintf("method %s not allowed on %s", r.Method, path))
			return
		}
		detail, ok := s.orchestrator.IssueDetailSnapshot(ident)
		if !ok {
			writeSymphonyError(w, http.StatusNotFound, "issue_not_found", fmt.Sprintf("issue %s not found", ident))
			return
		}
		writeSymphonyJSON(w, http.StatusOK, detail)
		return

	default:
		writeSymphonyError(w, http.StatusNotFound, "not_found", "route not found")
	}
}

func (s *SymphonyHTTPServer) handleDashboard(w http.ResponseWriter, _ *http.Request) {
	snap := s.orchestrator.StateSnapshot()
	s.orchestrator.mu.RLock()
	lastTickErr := s.orchestrator.LastTickError
	s.orchestrator.mu.RUnlock()

	var buf bytes.Buffer
	buf.WriteString("<!DOCTYPE html>\n<html><head><meta charset=\"utf-8\"><title>Fae Symphony Dashboard</title></head><body>\n")
	buf.WriteString("<h1>Fae Symphony Orchestrator Dashboard</h1>\n")
	fmt.Fprintf(&buf, "<p>Generated at: <code>%s</code> | Running: <strong>%d</strong> | Retrying: <strong>%d</strong></p>\n",
		html.EscapeString(snap.GeneratedAt), snap.Counts.Running, snap.Counts.Retrying)
	fmt.Fprintf(&buf, "<p>Tokens: input=<code>%d</code> output=<code>%d</code> total=<code>%d</code> | Runtime: <code>%.1fs</code></p>\n",
		snap.CodexTotals.InputTokens, snap.CodexTotals.OutputTokens, snap.CodexTotals.TotalTokens, snap.CodexTotals.SecondsRunning)
	if strings.TrimSpace(lastTickErr) != "" {
		fmt.Fprintf(&buf, "<p>Health: <strong>degraded</strong> (<code>%s</code>)</p>\n", html.EscapeString(lastTickErr))
	} else {
		buf.WriteString("<p>Health: <strong>ok</strong></p>\n")
	}
	if snap.RateLimits != nil {
		if rlBytes, err := json.Marshal(snap.RateLimits); err == nil {
			fmt.Fprintf(&buf, "<p>Rate Limits: <code>%s</code></p>\n", html.EscapeString(string(rlBytes)))
		}
	}

	buf.WriteString("<h2>Active Running Issues</h2>\n<table><thead><tr><th>Identifier</th><th>State</th><th>Fae Stage</th><th>Task ID</th><th>Session</th><th>Turns</th><th>Tokens</th><th>Last Message</th></tr></thead><tbody>\n")
	for _, row := range snap.Running {
		fmt.Fprintf(&buf, "<tr><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%s</td><td>%d</td><td>%d</td><td>%s</td></tr>\n",
			html.EscapeString(row.IssueIdentifier),
			html.EscapeString(row.State),
			html.EscapeString(row.Stage),
			html.EscapeString(row.TaskID),
			html.EscapeString(row.SessionID),
			row.TurnCount,
			row.Tokens.TotalTokens,
			html.EscapeString(row.LastMessage),
		)
	}
	buf.WriteString("</tbody></table>\n")

	buf.WriteString("<h2>Retry Queue</h2>\n<table><thead><tr><th>Identifier</th><th>Attempt</th><th>Due At</th><th>Error</th></tr></thead><tbody>\n")
	for _, row := range snap.Retrying {
		fmt.Fprintf(&buf, "<tr><td>%s</td><td>%d</td><td>%s</td><td>%s</td></tr>\n",
			html.EscapeString(row.IssueIdentifier),
			row.Attempt,
			html.EscapeString(row.DueAt),
			html.EscapeString(row.Error),
		)
	}
	buf.WriteString("</tbody></table>\n</body></html>\n")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func writeSymphonyJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}

func writeSymphonyError(w http.ResponseWriter, status int, code string, message string) {
	writeSymphonyJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}
