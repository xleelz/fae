package symphony

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Normalized coding-agent app-server error categories (Symphony §10.4).
const (
	ErrCodexNotFound       = "codex_not_found"
	ErrInvalidWorkspaceCwd = "invalid_workspace_cwd"
	ErrResponseTimeout     = "response_timeout"
	ErrTurnTimeout         = "turn_timeout"
	ErrPortExit            = "port_exit"
	ErrResponseError       = "response_error"
	ErrTurnFailed          = "turn_failed"
	ErrTurnCancelled       = "turn_cancelled"
	ErrTurnInputRequired   = "turn_input_required"
)

// MaxAppServerLineBytes is the 10 MB line scanner buffer limit for app-server stdout (Symphony §10.1).
const MaxAppServerLineBytes = 10 * 1024 * 1024

// RunnerError represents a categorized coding-agent app-server failure.
type RunnerError struct {
	Category string `json:"category"`
	Message  string `json:"message"`
	Cause    error  `json:"-"`
}

func (e *RunnerError) Error() string {
	if e == nil {
		return ""
	}
	if e.Message != "" {
		return fmt.Sprintf("%s: %s", e.Category, e.Message)
	}
	return e.Category
}

func (e *RunnerError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// RunnerErrorCategory extracts the normalized category from err if it is a *RunnerError.
func RunnerErrorCategory(err error) string {
	var re *RunnerError
	if errors.As(err, &re) && re != nil {
		return re.Category
	}
	return ""
}

// RunnerToolSpec describes a host-side dynamic tool advertised to the coding agent (Symphony §10.5).
type RunnerToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

// RunnerToolResult holds the structured output of a host-executed dynamic tool call.
type RunnerToolResult struct {
	Success bool           `json:"success"`
	Output  map[string]any `json:"output,omitempty"`
	Error   string         `json:"error,omitempty"`
}

// RunnerEvent represents a structured coding-agent session event emitted to the orchestrator.
type RunnerEvent struct {
	Event             string         `json:"event"`
	Timestamp         time.Time      `json:"timestamp"`
	Message           string         `json:"message,omitempty"`
	CodexAppServerPID string         `json:"codex_app_server_pid,omitempty"`
	SessionID         string         `json:"session_id,omitempty"`
	ThreadID          string         `json:"thread_id,omitempty"`
	TurnID            string         `json:"turn_id,omitempty"`
	InputTokens       int64          `json:"input_tokens,omitempty"`
	OutputTokens      int64          `json:"output_tokens,omitempty"`
	TotalTokens       int64          `json:"total_tokens,omitempty"`
	HasTokenUsage     bool           `json:"has_token_usage,omitempty"`
	RateLimits        any            `json:"rate_limits,omitempty"`
	Raw               map[string]any `json:"raw,omitempty"`
}

// AppServerRunner manages the JSON-RPC 2.0 coding-agent subprocess lifecycle and multi-turn loop (Symphony §10).
type AppServerRunner struct {
	WorkspaceRoot string
	Codex         CodexConfig
	MaxTurns      int
	SSHHost       string
	Env           []string
	DynamicTools  []RunnerToolSpec
	ExecuteTool   func(ctx context.Context, name string, arguments map[string]any) (RunnerToolResult, bool)
	OnEvent       func(ev RunnerEvent)
	CheckActive   func(ctx context.Context, issue Issue) (Issue, bool, error)
	Logger        func(format string, args ...any)
}

type jsonRPCEnvelope struct {
	JSONRPC string          `json:"jsonrpc,omitempty"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  map[string]any  `json:"params,omitempty"`
	Result  map[string]any  `json:"result,omitempty"`
	Error   *jsonRPCError   `json:"error,omitempty"`
	Raw     map[string]any  `json:"-"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type scannedLine struct {
	line string
	err  error
}

// Run validates the workspace cwd, launches the coding-agent app-server subprocess, performs the
// initialize/thread handshake, and executes up to MaxTurns turns on the same thread (Symphony §§7.1, 10).
func (r *AppServerRunner) Run(ctx context.Context, issue Issue, ws Workspace, initialPrompt string, attempt *int) error {
	if ctx == nil {
		ctx = context.Background()
	}

	wsPath := strings.TrimSpace(ws.Path)
	if err := ValidateAgentCwd(r.WorkspaceRoot, wsPath, wsPath); err != nil {
		r.emit(RunnerEvent{
			Event:     ErrInvalidWorkspaceCwd,
			Timestamp: time.Now().UTC(),
			Message:   err.Error(),
		})
		return &RunnerError{
			Category: ErrInvalidWorkspaceCwd,
			Message:  err.Error(),
			Cause:    err,
		}
	}

	cmdStr := strings.TrimSpace(r.Codex.Command)
	if cmdStr == "" {
		cmdStr = DefaultCodexCommand
	}

	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	var cmd *exec.Cmd
	if strings.TrimSpace(r.SSHHost) != "" {
		remoteCmd := fmt.Sprintf("cd %s && bash -lc %s", shellSingleQuote(wsPath), shellSingleQuote(cmdStr))
		cmd = exec.CommandContext(runCtx, "ssh", strings.TrimSpace(r.SSHHost), remoteCmd)
	} else {
		cmd = exec.CommandContext(runCtx, "bash", "-lc", cmdStr)
	}
	cmd.Dir = wsPath
	if r.Env != nil {
		cmd.Env = append([]string(nil), r.Env...)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 100 * time.Millisecond

	stdinPipe, err := cmd.StdinPipe()
	if err != nil {
		return &RunnerError{Category: ErrCodexNotFound, Message: fmt.Sprintf("stdin pipe: %v", err), Cause: err}
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return &RunnerError{Category: ErrCodexNotFound, Message: fmt.Sprintf("stdout pipe: %v", err), Cause: err}
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return &RunnerError{Category: ErrCodexNotFound, Message: fmt.Sprintf("stderr pipe: %v", err), Cause: err}
	}

	if err := cmd.Start(); err != nil {
		r.emit(RunnerEvent{
			Event:     ErrCodexNotFound,
			Timestamp: time.Now().UTC(),
			Message:   err.Error(),
		})
		return &RunnerError{Category: ErrCodexNotFound, Message: err.Error(), Cause: err}
	}

	pidStr := ""
	if cmd.Process != nil {
		pidStr = strconv.Itoa(cmd.Process.Pid)
	}

	var stderrMu sync.Mutex
	var stderrLines []string
	stderrDone := make(chan struct{})
	go func() {
		defer close(stderrDone)
		sc := bufio.NewScanner(stderrPipe)
		sc.Buffer(make([]byte, 16*1024), 256*1024)
		for sc.Scan() {
			line := sc.Text()
			r.logf("codex stderr [%s]: %s", issue.Identifier, line)
			stderrMu.Lock()
			if len(stderrLines) < 32 {
				stderrLines = append(stderrLines, line)
			}
			stderrMu.Unlock()
		}
	}()

	linesCh := make(chan scannedLine, 64)
	go func() {
		defer close(linesCh)
		sc := bufio.NewScanner(stdoutPipe)
		sc.Buffer(make([]byte, 64*1024), MaxAppServerLineBytes)
		for sc.Scan() {
			linesCh <- scannedLine{line: sc.Text()}
		}
		if scanErr := sc.Err(); scanErr != nil {
			linesCh <- scannedLine{err: scanErr}
		}
	}()

	defer func() {
		_ = stdinPipe.Close()
		cancelRun()
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
		<-stderrDone
	}()

	var writeMu sync.Mutex
	sendJSON := func(payload map[string]any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		b, mErr := json.Marshal(payload)
		if mErr != nil {
			return mErr
		}
		b = append(b, '\n')
		_, wErr := stdinPipe.Write(b)
		return wErr
	}

	classifyExitError := func(stage string) *RunnerError {
		_ = stdinPipe.Close()
		waitErr := cmd.Wait()
		<-stderrDone
		stderrMu.Lock()
		stderrSummary := strings.Join(stderrLines, "; ")
		stderrMu.Unlock()

		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) && exitErr.ExitCode() == 127 {
			msg := fmt.Sprintf("command not found during %s", stage)
			if stderrSummary != "" {
				msg += ": " + stderrSummary
			}
			r.emit(RunnerEvent{Event: ErrCodexNotFound, Timestamp: time.Now().UTC(), CodexAppServerPID: pidStr, Message: msg})
			return &RunnerError{Category: ErrCodexNotFound, Message: msg, Cause: waitErr}
		}
		lowerStderr := strings.ToLower(stderrSummary)
		if stage == "initialize" && (strings.Contains(lowerStderr, "command not found") || strings.Contains(lowerStderr, "no such file or directory")) {
			msg := fmt.Sprintf("codex command not found: %s", stderrSummary)
			r.emit(RunnerEvent{Event: ErrCodexNotFound, Timestamp: time.Now().UTC(), CodexAppServerPID: pidStr, Message: msg})
			return &RunnerError{Category: ErrCodexNotFound, Message: msg, Cause: waitErr}
		}
		msg := fmt.Sprintf("app-server exited during %s", stage)
		if stderrSummary != "" {
			msg += ": " + stderrSummary
		} else if waitErr != nil {
			msg += ": " + waitErr.Error()
		}
		r.emit(RunnerEvent{Event: ErrPortExit, Timestamp: time.Now().UTC(), CodexAppServerPID: pidStr, Message: msg})
		return &RunnerError{Category: ErrPortExit, Message: msg, Cause: waitErr}
	}

	readTimeout := time.Duration(r.Codex.ReadTimeoutMS) * time.Millisecond
	if readTimeout <= 0 {
		readTimeout = time.Duration(DefaultCodexReadTimeoutMS) * time.Millisecond
	}
	turnTimeout := time.Duration(r.Codex.TurnTimeoutMS) * time.Millisecond
	if turnTimeout <= 0 {
		turnTimeout = time.Duration(DefaultCodexTurnTimeoutMS) * time.Millisecond
	}
	maxTurns := r.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}

	var nextReqID int64 = 1
	allocID := func() int64 {
		id := nextReqID
		nextReqID++
		return id
	}

	// waitForResponse waits up to timeout for a JSON-RPC response matching wantID,
	// while still processing interleaved notifications, approvals, or tool calls.
	var handleIncomingMessage func(env jsonRPCEnvelope, threadID, turnID, sessionID string) (turnDone bool, turnErr *RunnerError)

	waitForResponse := func(wantID int64, stage string, timeout time.Duration, threadID, turnID, sessionID string) (jsonRPCEnvelope, *RunnerError) {
		timer := time.NewTimer(timeout)
		defer timer.Stop()

		for {
			select {
			case <-runCtx.Done():
				return jsonRPCEnvelope{}, &RunnerError{Category: ErrTurnCancelled, Message: "context cancelled", Cause: runCtx.Err()}
			case <-timer.C:
				msg := fmt.Sprintf("timed out waiting for %s response after %v", stage, timeout)
				r.emit(RunnerEvent{Event: ErrResponseTimeout, Timestamp: time.Now().UTC(), CodexAppServerPID: pidStr, ThreadID: threadID, TurnID: turnID, SessionID: sessionID, Message: msg})
				return jsonRPCEnvelope{}, &RunnerError{Category: ErrResponseTimeout, Message: msg}
			case item, ok := <-linesCh:
				if !ok {
					return jsonRPCEnvelope{}, classifyExitError(stage)
				}
				if item.err != nil {
					return jsonRPCEnvelope{}, &RunnerError{Category: ErrPortExit, Message: fmt.Sprintf("stdout read error during %s: %v", stage, item.err), Cause: item.err}
				}
				env, ok := parseJSONRPCLine(item.line)
				if !ok {
					r.emit(RunnerEvent{
						Event:             "malformed",
						Timestamp:         time.Now().UTC(),
						CodexAppServerPID: pidStr,
						ThreadID:          threadID,
						TurnID:            turnID,
						SessionID:         sessionID,
						Message:           "ignoring non-JSON stdout line",
					})
					continue
				}
				if idMatches(env.ID, wantID) && env.Method == "" {
					if env.Error != nil {
						msg := fmt.Sprintf("%s error (%d): %s", stage, env.Error.Code, env.Error.Message)
						r.emit(RunnerEvent{Event: ErrResponseError, Timestamp: time.Now().UTC(), CodexAppServerPID: pidStr, ThreadID: threadID, TurnID: turnID, SessionID: sessionID, Message: msg})
						return jsonRPCEnvelope{}, &RunnerError{Category: ErrResponseError, Message: msg}
					}
					return env, nil
				}
				if handleIncomingMessage != nil {
					if _, turnErr := handleIncomingMessage(env, threadID, turnID, sessionID); turnErr != nil {
						return jsonRPCEnvelope{}, turnErr
					}
				}
			}
		}
	}

	handleIncomingMessage = func(env jsonRPCEnvelope, threadID, turnID, sessionID string) (bool, *RunnerError) {
		now := time.Now().UTC()
		inTok, outTok, totTok, hasTok := extractEnvelopeTokens(env.Raw)
		rateLimits := extractEnvelopeRateLimits(env.Raw)
		method := strings.TrimSpace(env.Method)
		lowerMethod := strings.ToLower(method)

		// 1. Check for user input request -> fail immediately with turn_input_required (Symphony §10.3)
		if method == "item/tool/requestUserInput" ||
			lowerMethod == "turn/input_required" ||
			lowerMethod == "turn_input_required" ||
			strings.Contains(lowerMethod, "requestuserinput") ||
			isInputRequiredEvent(env.Params) {
			msg := "coding agent requested interactive user input"
			r.emit(RunnerEvent{
				Event:             ErrTurnInputRequired,
				Timestamp:         now,
				CodexAppServerPID: pidStr,
				SessionID:         sessionID,
				ThreadID:          threadID,
				TurnID:            turnID,
				Message:           msg,
				InputTokens:       inTok,
				OutputTokens:      outTok,
				TotalTokens:       totTok,
				HasTokenUsage:     hasTok,
				RateLimits:        rateLimits,
				Raw:               env.Raw,
			})
			return false, &RunnerError{Category: ErrTurnInputRequired, Message: msg}
		}

		// 2. Check for dynamic tool execution request (Symphony §10.5)
		if method == "item/tool/call" || method == "tool/call" || method == "tools/call" || method == "dynamic_tool_call" {
			toolName, toolArgs := extractToolCallParams(env.Params)
			var supported bool
			var toolRes RunnerToolResult
			if r.ExecuteTool != nil && toolName != "" {
				toolRes, supported = r.ExecuteTool(runCtx, toolName, toolArgs)
			}
			if !supported {
				errText := fmt.Sprintf("unsupported tool call %q", toolName)
				r.emit(RunnerEvent{
					Event:             "unsupported_tool_call",
					Timestamp:         now,
					CodexAppServerPID: pidStr,
					SessionID:         sessionID,
					ThreadID:          threadID,
					TurnID:            turnID,
					Message:           errText,
					Raw:               env.Raw,
				})
				if env.ID != nil {
					errJSON, _ := json.Marshal(map[string]any{"success": false, "error": errText})
					_ = sendJSON(map[string]any{
						"jsonrpc": "2.0",
						"id":      env.ID,
						"result": map[string]any{
							"success": false,
							"error":   errText,
							"contentItems": []map[string]any{
								{"type": "inputText", "text": string(errJSON)},
							},
						},
					})
				}
				return false, nil
			}

			r.emit(RunnerEvent{
				Event:             "tool_call_completed",
				Timestamp:         now,
				CodexAppServerPID: pidStr,
				SessionID:         sessionID,
				ThreadID:          threadID,
				TurnID:            turnID,
				Message:           toolName,
				Raw:               env.Raw,
			})
			if env.ID != nil {
				resJSON, _ := json.Marshal(toolRes)
				_ = sendJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      env.ID,
					"result": map[string]any{
						"success": toolRes.Success,
						"output":  toolRes.Output,
						"error":   toolRes.Error,
						"contentItems": []map[string]any{
							{"type": "inputText", "text": string(resJSON)},
						},
					},
				})
			}
			return false, nil
		}

		// 3. Check for command / file change approval requests -> auto-approve (Symphony §10.3)
		if strings.Contains(lowerMethod, "approval") {
			if env.ID != nil {
				_ = sendJSON(map[string]any{
					"jsonrpc": "2.0",
					"id":      env.ID,
					"result": map[string]any{
						"decision": "accept",
						"approved": true,
					},
				})
			}
			r.emit(RunnerEvent{
				Event:             "approval_auto_approved",
				Timestamp:         now,
				CodexAppServerPID: pidStr,
				SessionID:         sessionID,
				ThreadID:          threadID,
				TurnID:            turnID,
				Message:           method,
				InputTokens:       inTok,
				OutputTokens:      outTok,
				TotalTokens:       totTok,
				HasTokenUsage:     hasTok,
				RateLimits:        rateLimits,
				Raw:               env.Raw,
			})
			return false, nil
		}

		// 4. Check for turn completion / failure / cancellation
		switch lowerMethod {
		case "turn/completed", "turn_completed":
			status := extractTurnStatus(env.Params)
			switch status {
			case "failed", "error":
				errMsg := extractTurnErrorMessage(env.Params, "turn failed")
				r.emit(RunnerEvent{
					Event:             ErrTurnFailed,
					Timestamp:         now,
					CodexAppServerPID: pidStr,
					SessionID:         sessionID,
					ThreadID:          threadID,
					TurnID:            turnID,
					Message:           errMsg,
					InputTokens:       inTok,
					OutputTokens:      outTok,
					TotalTokens:       totTok,
					HasTokenUsage:     hasTok,
					RateLimits:        rateLimits,
					Raw:               env.Raw,
				})
				return false, &RunnerError{Category: ErrTurnFailed, Message: errMsg}
			case "cancelled", "canceled":
				errMsg := extractTurnErrorMessage(env.Params, "turn cancelled")
				r.emit(RunnerEvent{
					Event:             ErrTurnCancelled,
					Timestamp:         now,
					CodexAppServerPID: pidStr,
					SessionID:         sessionID,
					ThreadID:          threadID,
					TurnID:            turnID,
					Message:           errMsg,
					InputTokens:       inTok,
					OutputTokens:      outTok,
					TotalTokens:       totTok,
					HasTokenUsage:     hasTok,
					RateLimits:        rateLimits,
					Raw:               env.Raw,
				})
				return false, &RunnerError{Category: ErrTurnCancelled, Message: errMsg}
			case "input_required", "requires_input":
				errMsg := extractTurnErrorMessage(env.Params, "turn requires user input")
				r.emit(RunnerEvent{
					Event:             ErrTurnInputRequired,
					Timestamp:         now,
					CodexAppServerPID: pidStr,
					SessionID:         sessionID,
					ThreadID:          threadID,
					TurnID:            turnID,
					Message:           errMsg,
					InputTokens:       inTok,
					OutputTokens:      outTok,
					TotalTokens:       totTok,
					HasTokenUsage:     hasTok,
					RateLimits:        rateLimits,
					Raw:               env.Raw,
				})
				return false, &RunnerError{Category: ErrTurnInputRequired, Message: errMsg}
			default:
				r.emit(RunnerEvent{
					Event:             "turn_completed",
					Timestamp:         now,
					CodexAppServerPID: pidStr,
					SessionID:         sessionID,
					ThreadID:          threadID,
					TurnID:            turnID,
					Message:           extractTurnMessage(env.Params, "turn completed"),
					InputTokens:       inTok,
					OutputTokens:      outTok,
					TotalTokens:       totTok,
					HasTokenUsage:     hasTok,
					RateLimits:        rateLimits,
					Raw:               env.Raw,
				})
				return true, nil
			}
		case "turn/failed", "turn_failed":
			errMsg := extractTurnErrorMessage(env.Params, "turn failed")
			r.emit(RunnerEvent{
				Event:             ErrTurnFailed,
				Timestamp:         now,
				CodexAppServerPID: pidStr,
				SessionID:         sessionID,
				ThreadID:          threadID,
				TurnID:            turnID,
				Message:           errMsg,
				InputTokens:       inTok,
				OutputTokens:      outTok,
				TotalTokens:       totTok,
				HasTokenUsage:     hasTok,
				RateLimits:        rateLimits,
				Raw:               env.Raw,
			})
			return false, &RunnerError{Category: ErrTurnFailed, Message: errMsg}
		case "turn/cancelled", "turn/canceled", "turn_cancelled", "turn_canceled":
			errMsg := extractTurnErrorMessage(env.Params, "turn cancelled")
			r.emit(RunnerEvent{
				Event:             ErrTurnCancelled,
				Timestamp:         now,
				CodexAppServerPID: pidStr,
				SessionID:         sessionID,
				ThreadID:          threadID,
				TurnID:            turnID,
				Message:           errMsg,
				InputTokens:       inTok,
				OutputTokens:      outTok,
				TotalTokens:       totTok,
				HasTokenUsage:     hasTok,
				RateLimits:        rateLimits,
				Raw:               env.Raw,
			})
			return false, &RunnerError{Category: ErrTurnCancelled, Message: errMsg}
		}

		// 5. Generic notification / streaming update
		evName := "notification"
		if method != "" {
			evName = method
		}
		r.emit(RunnerEvent{
			Event:             evName,
			Timestamp:         now,
			CodexAppServerPID: pidStr,
			SessionID:         sessionID,
			ThreadID:          threadID,
			TurnID:            turnID,
			Message:           extractTurnMessage(env.Params, ""),
			InputTokens:       inTok,
			OutputTokens:      outTok,
			TotalTokens:       totTok,
			HasTokenUsage:     hasTok,
			RateLimits:        rateLimits,
			Raw:               env.Raw,
		})
		return false, nil
	}

	// --- Handshake Step 1: initialize ---
	initID := allocID()
	if err := sendJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      initID,
		"method":  "initialize",
		"params": map[string]any{
			"clientInfo": map[string]any{
				"name":    "fae-symphony",
				"version": "1.0",
			},
			"capabilities": map[string]any{},
		},
	}); err != nil {
		return classifyExitError("initialize")
	}

	if _, rErr := waitForResponse(initID, "initialize", readTimeout, "", "", ""); rErr != nil {
		return rErr
	}

	// --- Handshake Step 2: initialized notification ---
	if err := sendJSON(map[string]any{
		"jsonrpc": "2.0",
		"method":  "initialized",
		"params":  map[string]any{},
	}); err != nil {
		return classifyExitError("initialized")
	}

	// --- Handshake Step 3: thread/start ---
	threadParams := map[string]any{
		"cwd": wsPath,
	}
	if r.Codex.ApprovalPolicy != nil {
		threadParams["approvalPolicy"] = r.Codex.ApprovalPolicy
	} else {
		threadParams["approvalPolicy"] = "never"
	}
	if r.Codex.ThreadSandbox != nil {
		threadParams["sandbox"] = r.Codex.ThreadSandbox
	}
	if len(r.DynamicTools) > 0 {
		tools := make([]map[string]any, 0, len(r.DynamicTools))
		for _, t := range r.DynamicTools {
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		threadParams["dynamicTools"] = tools
		threadParams["tools"] = tools
	}

	threadReqID := allocID()
	if err := sendJSON(map[string]any{
		"jsonrpc": "2.0",
		"id":      threadReqID,
		"method":  "thread/start",
		"params":  threadParams,
	}); err != nil {
		return classifyExitError("thread/start")
	}

	threadResp, rErr := waitForResponse(threadReqID, "thread/start", readTimeout, "", "", "")
	if rErr != nil {
		return rErr
	}
	threadID := extractThreadID(threadResp.Result)
	if threadID == "" {
		msg := "missing thread_id in thread/start response"
		r.emit(RunnerEvent{Event: ErrResponseError, Timestamp: time.Now().UTC(), CodexAppServerPID: pidStr, Message: msg})
		return &RunnerError{Category: ErrResponseError, Message: msg}
	}

	// --- Multi-Turn Loop (Symphony §7.1 & §10.2) ---
	currentIssue := issue
	for turnNumber := 1; turnNumber <= maxTurns; turnNumber++ {
		var turnPrompt string
		if turnNumber == 1 {
			turnPrompt = initialPrompt
			if strings.TrimSpace(turnPrompt) == "" {
				rendered, err := RenderPrompt("", currentIssue, attempt)
				if err != nil {
					return err
				}
				turnPrompt = rendered
			}
		} else {
			turnPrompt = RenderContinuationPrompt(currentIssue, attempt, turnNumber, maxTurns)
		}

		turnParams := map[string]any{
			"threadId": threadID,
			"cwd":      wsPath,
			"title":    fmt.Sprintf("%s: %s", currentIssue.Identifier, currentIssue.Title),
			"input": []map[string]any{
				{"type": "text", "text": turnPrompt},
			},
			"prompt": turnPrompt,
		}
		if r.Codex.ApprovalPolicy != nil {
			turnParams["approvalPolicy"] = r.Codex.ApprovalPolicy
		} else {
			turnParams["approvalPolicy"] = "never"
		}
		if r.Codex.TurnSandboxPolicy != nil {
			turnParams["sandboxPolicy"] = r.Codex.TurnSandboxPolicy
		}

		turnReqID := allocID()
		if err := sendJSON(map[string]any{
			"jsonrpc": "2.0",
			"id":      turnReqID,
			"method":  "turn/start",
			"params":  turnParams,
		}); err != nil {
			return classifyExitError("turn/start")
		}

		turnResp, rErr := waitForResponse(turnReqID, "turn/start", readTimeout, threadID, "", "")
		if rErr != nil {
			return rErr
		}
		turnID := extractTurnID(turnResp.Result)
		if turnID == "" {
			turnID = fmt.Sprintf("turn-%d", turnNumber)
		}
		sessionID := fmt.Sprintf("%s-%s", threadID, turnID)

		r.emit(RunnerEvent{
			Event:             "session_started",
			Timestamp:         time.Now().UTC(),
			CodexAppServerPID: pidStr,
			SessionID:         sessionID,
			ThreadID:          threadID,
			TurnID:            turnID,
			Message:           fmt.Sprintf("started turn %d/%d", turnNumber, maxTurns),
		})

		if err := r.streamTurn(runCtx, linesCh, turnTimeout, threadID, turnID, sessionID, pidStr, handleIncomingMessage, classifyExitError); err != nil {
			return err
		}

		if r.CheckActive != nil {
			refreshedIssue, stillActive, checkErr := r.CheckActive(runCtx, currentIssue)
			if checkErr != nil {
				return &RunnerError{
					Category: ErrResponseError,
					Message:  fmt.Sprintf("failed to refresh issue state after turn %d: %v", turnNumber, checkErr),
					Cause:    checkErr,
				}
			}
			currentIssue = refreshedIssue
			if !stillActive || turnNumber == maxTurns {
				break
			}
		} else if turnNumber == maxTurns {
			break
		}
	}

	return nil
}

func (r *AppServerRunner) streamTurn(
	ctx context.Context,
	linesCh <-chan scannedLine,
	turnTimeout time.Duration,
	threadID, turnID, sessionID, pidStr string,
	handleIncomingMessage func(env jsonRPCEnvelope, threadID, turnID, sessionID string) (bool, *RunnerError),
	classifyExitError func(stage string) *RunnerError,
) error {
	timer := time.NewTimer(turnTimeout)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return &RunnerError{Category: ErrTurnCancelled, Message: "context cancelled", Cause: ctx.Err()}
		case <-timer.C:
			msg := fmt.Sprintf("turn timed out after %v", turnTimeout)
			r.emit(RunnerEvent{
				Event:             ErrTurnTimeout,
				Timestamp:         time.Now().UTC(),
				CodexAppServerPID: pidStr,
				SessionID:         sessionID,
				ThreadID:          threadID,
				TurnID:            turnID,
				Message:           msg,
			})
			return &RunnerError{Category: ErrTurnTimeout, Message: msg}
		case item, ok := <-linesCh:
			if !ok {
				return classifyExitError("turn")
			}
			if item.err != nil {
				if errors.Is(item.err, io.EOF) {
					return classifyExitError("turn")
				}
				return &RunnerError{Category: ErrPortExit, Message: fmt.Sprintf("stdout read error: %v", item.err), Cause: item.err}
			}
			env, valid := parseJSONRPCLine(item.line)
			if !valid {
				r.emit(RunnerEvent{
					Event:             "malformed",
					Timestamp:         time.Now().UTC(),
					CodexAppServerPID: pidStr,
					SessionID:         sessionID,
					ThreadID:          threadID,
					TurnID:            turnID,
					Message:           "ignoring non-JSON stdout line",
				})
				continue
			}
			done, turnErr := handleIncomingMessage(env, threadID, turnID, sessionID)
			if turnErr != nil {
				return turnErr
			}
			if done {
				return nil
			}
		}
	}
}

func (r *AppServerRunner) emit(ev RunnerEvent) {
	if r.OnEvent != nil {
		r.OnEvent(ev)
	}
}

func (r *AppServerRunner) logf(format string, args ...any) {
	if r.Logger != nil {
		r.Logger(format, args...)
	}
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func parseJSONRPCLine(line string) (jsonRPCEnvelope, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || !strings.HasPrefix(trimmed, "{") {
		return jsonRPCEnvelope{}, false
	}
	var raw map[string]any
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&raw); err != nil {
		return jsonRPCEnvelope{}, false
	}
	var env jsonRPCEnvelope
	if err := json.Unmarshal([]byte(trimmed), &env); err != nil {
		return jsonRPCEnvelope{}, false
	}
	env.Raw = raw
	return env, true
}

func idMatches(rawID any, want int64) bool {
	if rawID == nil {
		return false
	}
	switch v := rawID.(type) {
	case float64:
		return int64(v) == want
	case int:
		return int64(v) == want
	case int64:
		return v == want
	case json.Number:
		n, err := v.Int64()
		return err == nil && n == want
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		return err == nil && n == want
	default:
		return false
	}
}

func extractThreadID(res map[string]any) string {
	if res == nil {
		return ""
	}
	for _, k := range []string{"threadId", "thread_id", "id"} {
		if s, ok := res[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if threadObj, ok := res["thread"].(map[string]any); ok {
		for _, k := range []string{"id", "threadId", "thread_id"} {
			if s, ok := threadObj[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func extractTurnID(res map[string]any) string {
	if res == nil {
		return ""
	}
	for _, k := range []string{"turnId", "turn_id", "id"} {
		if s, ok := res[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	if turnObj, ok := res["turn"].(map[string]any); ok {
		for _, k := range []string{"id", "turnId", "turn_id"} {
			if s, ok := turnObj[k].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func extractToolCallParams(params map[string]any) (string, map[string]any) {
	if params == nil {
		return "", nil
	}
	var name string
	for _, k := range []string{"name", "tool", "toolName", "tool_name"} {
		if s, ok := params[k].(string); ok && strings.TrimSpace(s) != "" {
			name = strings.TrimSpace(s)
			break
		}
	}
	var args map[string]any
	for _, k := range []string{"arguments", "input", "args", "params"} {
		switch v := params[k].(type) {
		case map[string]any:
			args = v
		case string:
			var parsed map[string]any
			if json.Unmarshal([]byte(v), &parsed) == nil {
				args = parsed
			}
		}
		if args != nil {
			break
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	return name, args
}

func isInputRequiredEvent(params map[string]any) bool {
	if params == nil {
		return false
	}
	if reqInput, ok := params["requiresInput"].(bool); ok && reqInput {
		return true
	}
	if reqInput, ok := params["input_required"].(bool); ok && reqInput {
		return true
	}
	if t, ok := params["type"].(string); ok {
		lt := strings.ToLower(strings.TrimSpace(t))
		if lt == "turn_input_required" || lt == "input_required" || lt == "request_user_input" {
			return true
		}
	}
	return false
}

func extractTurnStatus(params map[string]any) string {
	if params == nil {
		return "completed"
	}
	if s, ok := params["status"].(string); ok && strings.TrimSpace(s) != "" {
		return strings.ToLower(strings.TrimSpace(s))
	}
	if turnObj, ok := params["turn"].(map[string]any); ok {
		if s, ok := turnObj["status"].(string); ok && strings.TrimSpace(s) != "" {
			return strings.ToLower(strings.TrimSpace(s))
		}
	}
	return "completed"
}

func extractTurnErrorMessage(params map[string]any, fallback string) string {
	if params == nil {
		return fallback
	}
	for _, k := range []string{"message", "error"} {
		switch v := params[k].(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		case map[string]any:
			if msg, ok := v["message"].(string); ok && strings.TrimSpace(msg) != "" {
				return strings.TrimSpace(msg)
			}
		}
	}
	if turnObj, ok := params["turn"].(map[string]any); ok {
		if msg := extractTurnErrorMessage(turnObj, ""); msg != "" {
			return msg
		}
	}
	return fallback
}

func extractTurnMessage(params map[string]any, fallback string) string {
	if params == nil {
		return fallback
	}
	for _, k := range []string{"message", "summary", "text"} {
		if s, ok := params[k].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return fallback
}

func extractEnvelopeTokens(raw map[string]any) (int64, int64, int64, bool) {
	if raw == nil {
		return 0, 0, 0, false
	}
	for _, containerKey := range []string{"params", "result"} {
		container, ok := raw[containerKey].(map[string]any)
		if !ok {
			continue
		}
		if in, out, tot, ok := extractTokensFromMap(container); ok {
			return in, out, tot, true
		}
	}
	return extractTokensFromMap(raw)
}

func extractTokensFromMap(m map[string]any) (int64, int64, int64, bool) {
	for _, k := range []string{"total_token_usage", "totalTokenUsage"} {
		if sub, ok := m[k].(map[string]any); ok {
			return parseTokenSubmap(sub)
		}
	}
	for _, k := range []string{"usage", "tokenUsage", "token_usage"} {
		if sub, ok := m[k].(map[string]any); ok {
			for _, tk := range []string{"total_token_usage", "totalTokenUsage", "total"} {
				if totSub, ok2 := sub[tk].(map[string]any); ok2 {
					return parseTokenSubmap(totSub)
				}
			}
			if _, hasLastOnly := sub["last_token_usage"]; hasLastOnly && sub["input_tokens"] == nil && sub["inputTokens"] == nil {
				return 0, 0, 0, false
			}
			return parseTokenSubmap(sub)
		}
	}
	return 0, 0, 0, false
}

func parseTokenSubmap(m map[string]any) (int64, int64, int64, bool) {
	in := toRunnerInt64(firstNonNil(m["input_tokens"], m["inputTokens"]))
	out := toRunnerInt64(firstNonNil(m["output_tokens"], m["outputTokens"]))
	tot := toRunnerInt64(firstNonNil(m["total_tokens"], m["totalTokens"]))
	if tot == 0 && (in > 0 || out > 0) {
		tot = in + out
	}
	if in == 0 && out == 0 && tot == 0 {
		return 0, 0, 0, false
	}
	return in, out, tot, true
}

func firstNonNil(vals ...any) any {
	for _, v := range vals {
		if v != nil {
			return v
		}
	}
	return nil
}

func toRunnerInt64(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	default:
		return 0
	}
}

func extractEnvelopeRateLimits(raw map[string]any) any {
	if raw == nil {
		return nil
	}
	for _, containerKey := range []string{"params", "result"} {
		if container, ok := raw[containerKey].(map[string]any); ok {
			if rl := firstNonNil(container["rate_limits"], container["rateLimits"]); rl != nil {
				return rl
			}
		}
	}
	return firstNonNil(raw["rate_limits"], raw["rateLimits"])
}
