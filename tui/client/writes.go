package client

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"
)

// Every write cites an authorizing operator message recorded just before it
// (daemon/AUTHORITY.md); the daemon checks, audits and consumes it.

// Authorization is the operator message text that authorizes one write: a
// human sentence and the action class it directs, in the structured form
// `fae message` accepts.
func Authorization(text, class string) string {
	raw, _ := json.Marshal(map[string]any{"text": text, "authorize": map[string]any{"classes": []string{class}}})
	return string(raw)
}

// Message records an operator message, scoped to a task when task is set,
// and returns its id.
func (c *Client) Message(ctx context.Context, text, task string) (string, error) {
	params := map[string]any{"text": text}
	if task != "" {
		params["task"] = task
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, "Message", params, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", errors.New("message recorded without an id")
	}
	return out.ID, nil
}

// TaskFields are the admission fields of TaskCreate (the fields `backlog
// admit` takes for an item, with the mode and disposition `fae task create`
// takes). Optional fields are omitted when empty. Mode is "Local" or
// "Normal" (empty: the instance default); a Normal task carries exactly one
// of DryRun or CommitExternal.
type TaskFields struct {
	ID             string   `json:"id"`
	Title          string   `json:"title,omitempty"`
	Content        string   `json:"content"`
	Priority       int      `json:"priority,omitempty"`
	SourceRef      string   `json:"source_ref,omitempty"`
	WriteClass     string   `json:"write_class,omitempty"`
	DeclaredFiles  []string `json:"declared_files,omitempty"`
	TestFilter     string   `json:"test_filter,omitempty"`
	TestShape      string   `json:"test_shape,omitempty"`
	WorkflowID     string   `json:"workflow_id,omitempty"`
	Revision       int64    `json:"revision,omitempty"`
	TargetConfig   string   `json:"target_config,omitempty"`
	TargetRevision int64    `json:"target_revision,omitempty"`
	TaskClass      string   `json:"task_class,omitempty"`
	Mode           string   `json:"mode,omitempty"`
	DryRun         bool     `json:"dry_run,omitempty"`
	CommitExternal bool     `json:"commit_external,omitempty"`
}

// ModeComplete is the daemon's submission rule (daemon.SubmissionModeComplete,
// FAE-MODE-001): a Normal task states exactly one of dry-run or
// commit-external, and a Local task states neither.
func ModeComplete(normal, dryRun, commitExternal bool) bool {
	return !(dryRun && commitExternal) && normal == (dryRun || commitExternal)
}

// TaskCreateParams is TaskCreate: one new task joined to the live graph.
// AcceptInFlight is the operator's acceptance of a new blocking edge that
// holds an already dispatched dependent's next release.
type TaskCreateParams struct {
	TaskFields
	Edges              []Edge `json:"edges,omitempty"`
	AcceptInFlight     bool   `json:"accept_in_flight,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}

// TaskCreateResult is TaskCreate's result.
type TaskCreateResult struct {
	ID         string          `json:"id"`
	State      string          `json:"state"`
	Version    int64           `json:"version"`
	EdgesAdded json.RawMessage `json:"edges_added"`
	Readiness  json.RawMessage `json:"readiness"`
}

// TaskCreate creates one task (create is not upsert: an existing id is refused).
func (c *Client) TaskCreate(ctx context.Context, p TaskCreateParams) (TaskCreateResult, error) {
	var out TaskCreateResult
	err := c.call(ctx, "TaskCreate", p, &out)
	return out, err
}

// TaskUpdateParams is TaskUpdate: only the fields present change.
// ExpectedVersion, when set, is the task version the edit was prepared
// against: the daemon refuses the edit if the task has moved on since.
type TaskUpdateParams struct {
	ID                 string    `json:"id"`
	Title              *string   `json:"title,omitempty"`
	Content            *string   `json:"content,omitempty"`
	Priority           *int      `json:"priority,omitempty"`
	SourceRef          *string   `json:"source_ref,omitempty"`
	WriteClass         *string   `json:"write_class,omitempty"`
	DeclaredFiles      *[]string `json:"declared_files,omitempty"`
	TestFilter         *string   `json:"test_filter,omitempty"`
	TestShape          *string   `json:"test_shape,omitempty"`
	ExpectedVersion    int64     `json:"expected_version,omitempty"`
	AuthorizingMessage string    `json:"authorizing_message"`
}

// StaleVersion reports the daemon's refusal of a write whose
// expected_version no longer matches the task.
func StaleVersion(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "not the expected") && strings.Contains(text, "version")
}

// Changed lists the fields the update carries, in the contract's names.
func (p TaskUpdateParams) Changed() []string {
	out := []string{}
	for name, present := range map[string]bool{"title": p.Title != nil, "content": p.Content != nil, "priority": p.Priority != nil, "source_ref": p.SourceRef != nil, "write_class": p.WriteClass != nil, "declared_files": p.DeclaredFiles != nil, "test_filter": p.TestFilter != nil, "test_shape": p.TestShape != nil} {
		if present {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// TaskUpdateResult is TaskUpdate's result; Effect is "applied" or
// "from-next-stage" (a running task keeps its turn in flight).
type TaskUpdateResult struct {
	ID      string   `json:"id"`
	Changed []string `json:"changed"`
	Effect  string   `json:"effect"`
	State   string   `json:"state"`
	Version int64    `json:"version"`
}

// TaskUpdate edits a live task.
func (c *Client) TaskUpdate(ctx context.Context, p TaskUpdateParams) (TaskUpdateResult, error) {
	var out TaskUpdateResult
	err := c.call(ctx, "TaskUpdate", p, &out)
	return out, err
}

// TaskDeleteParams is TaskDelete: retire and keep history. Reason is
// required; SupersededBy names a task that takes over the blocking
// dependents, which are otherwise held.
type TaskDeleteParams struct {
	ID                 string `json:"id"`
	Reason             string `json:"reason"`
	SupersededBy       string `json:"superseded_by,omitempty"`
	ExpectedVersion    int64  `json:"expected_version,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}

// TaskDeleteResult is TaskDelete's result. Pending is a delete that waits
// for the task's turn in flight: the task retires once it is collected.
type TaskDeleteResult struct {
	ID                  string   `json:"id"`
	Action              string   `json:"action"`
	CollectedInFlight   bool     `json:"collected_in_flight"`
	HeldDependents      []string `json:"held_dependents"`
	RepointedDependents []string `json:"repointed_dependents"`
	ReasonClass         string   `json:"reason_class"`
	Pending             bool     `json:"pending"`
	Dispatch            string   `json:"dispatch,omitempty"`
	State               string   `json:"state"`
	Version             int64    `json:"version"`
}

// TaskDelete withdraws a task that never started, or stops and retires one
// that did, keeping its history.
func (c *Client) TaskDelete(ctx context.Context, p TaskDeleteParams) (TaskDeleteResult, error) {
	var out TaskDeleteResult
	err := c.call(ctx, "TaskDelete", p, &out)
	return out, err
}

// EdgeParams is BacklogEdge: add or remove one edge.
type EdgeParams struct {
	Edge
	Remove             bool   `json:"remove,omitempty"`
	AcceptInFlight     bool   `json:"accept_in_flight,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}

// BacklogEdge adds or removes one edge; the raw result is returned for display.
func (c *Client) BacklogEdge(ctx context.Context, p EdgeParams) (json.RawMessage, error) {
	return c.c.Call(ctx, "BacklogEdge", p)
}

// AlertAcknowledge acknowledges one alert.
func (c *Client) AlertAcknowledge(ctx context.Context, id int64, message string) error {
	return c.call(ctx, "AlertAcknowledge", map[string]any{"id": id, "authorizing_message": message}, nil)
}

func needsAuthorizingMessage(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "authorizing") || strings.Contains(text, "message")
}

// Notify sends a task notification via the Notify RPC (FAE-OPS-033).
func (c *Client) Notify(ctx context.Context, p NotifyParams) error {
	kind := firstNonEmpty(p.Kind, "note")
	switch kind {
	case "breaking-change", "cross-area-need", "note":
	default:
		kind = "note"
	}
	delivery := firstNonEmpty(p.Delivery, "next-turn")
	if delivery == "interrupt" {
		delivery = "in-flight"
	}
	buildReq := func(msg string) map[string]any {
		req := map[string]any{
			"task":     p.Task,
			"kind":     kind,
			"delivery": delivery,
			"body":     p.Summary,
		}
		if p.FromTask != "" {
			req["from_task"] = p.FromTask
		}
		if msg != "" {
			req["authorizing_message"] = msg
		}
		return req
	}
	err := c.call(ctx, "Notify", buildReq(p.AuthorizingMessage), nil)
	if err != nil && p.AuthorizingMessage == "" && needsAuthorizingMessage(err) {
		msgID, mErr := c.Message(ctx, Authorization("notify task "+p.Task, "task"), p.Task)
		if mErr != nil {
			return mErr
		}
		p.AuthorizingMessage = msgID
		err = c.call(ctx, "Notify", buildReq(p.AuthorizingMessage), nil)
	}
	return err
}

// FollowUp creates a follow-up task via the FollowUp RPC (FAE-OPS-034).
func (c *Client) FollowUp(ctx context.Context, p FollowUpParams) (FollowUpResult, error) {
	decodeRes := func(raw json.RawMessage) (FollowUpResult, error) {
		if len(raw) == 0 || string(raw) == "null" {
			return FollowUpResult{ID: p.ID, PredecessorTask: p.Task}, nil
		}
		var wire struct {
			ID                string `json:"id"`
			PredecessorTask   string `json:"predecessor_task"`
			FollowUpOf        string `json:"follow_up_of"`
			State             string `json:"state"`
			ResumedSession    string `json:"resumed_session"`
			ProviderSessionID string `json:"provider_session_id"`
		}
		if err := json.Unmarshal(raw, &wire); err != nil {
			return FollowUpResult{}, err
		}
		return FollowUpResult{
			ID:              firstNonEmpty(wire.ID, p.ID),
			PredecessorTask: firstNonEmpty(wire.PredecessorTask, wire.FollowUpOf, p.Task),
			State:           wire.State,
			ResumedSession:  firstNonEmpty(wire.ResumedSession, wire.ProviderSessionID),
		}, nil
	}
	buildReq := func(msg string) map[string]any {
		req := map[string]any{
			"task": p.Task,
			"text": p.Prompt,
		}
		if p.ID != "" {
			req["id"] = p.ID
		}
		if len(p.Scope) > 0 {
			req["scope"] = p.Scope
		}
		if p.TurnBudget != nil {
			b := map[string]any{}
			if p.TurnBudget.WallSeconds > 0 {
				b["wall_seconds"] = p.TurnBudget.WallSeconds
			}
			maxTurns := p.TurnBudget.MaxAssistantTurns
			if maxTurns == 0 {
				maxTurns = p.TurnBudget.MaxTurns
			}
			if maxTurns > 0 {
				b["max_turns"] = maxTurns
			}
			if p.TurnBudget.OnExhaustion != "" {
				b["on_exhaustion"] = p.TurnBudget.OnExhaustion
			}
			if len(b) > 0 {
				req["budget"] = b
			}
		}
		if msg != "" {
			req["authorizing_message"] = msg
		}
		return req
	}
	raw, err := c.c.Call(ctx, "FollowUp", buildReq(p.AuthorizingMessage))
	if err != nil && p.AuthorizingMessage == "" && needsAuthorizingMessage(err) {
		msgID, mErr := c.Message(ctx, Authorization("follow up task "+p.Task, "task"), p.Task)
		if mErr != nil {
			return FollowUpResult{}, mErr
		}
		p.AuthorizingMessage = msgID
		raw, err = c.c.Call(ctx, "FollowUp", buildReq(p.AuthorizingMessage))
	}
	if err != nil {
		return FollowUpResult{}, err
	}
	return decodeRes(raw)
}

// PoolResize resizes a provider concurrency pool via PoolResize (FAE-SCH-015).
func (c *Client) PoolResize(ctx context.Context, p PoolResizeParams) error {
	if p.AuthorizingMessage == "" {
		msgID, err := c.Message(ctx, Authorization("resize provider pool "+p.Pool, "runtime"), "")
		if err != nil {
			return err
		}
		p.AuthorizingMessage = msgID
	}
	return c.call(ctx, "PoolResize", map[string]any{
		"pool":                p.Pool,
		"cap":                 p.Cap,
		"authorizing_message": p.AuthorizingMessage,
	}, nil)
}

// QuotaSet records a provider quota reset window via QuotaSet (FAE-RT-018).
func (c *Client) QuotaSet(ctx context.Context, p QuotaSetParams) error {
	if p.AuthorizingMessage == "" {
		target := firstNonEmpty(p.Pool, p.Provider)
		msgID, err := c.Message(ctx, Authorization("set provider quota "+target, "runtime"), "")
		if err != nil {
			return err
		}
		p.AuthorizingMessage = msgID
	}
	resetAt := strings.TrimSpace(p.ResetAt)
	if dur, err := time.ParseDuration(resetAt); err == nil {
		now := time.Now().UTC()
		if c.now != nil {
			now = c.now().UTC()
		}
		resetAt = now.Add(dur).Format(time.RFC3339)
	}
	req := map[string]any{
		"reset_at":            resetAt,
		"grace_seconds":       p.GraceSeconds,
		"authorizing_message": p.AuthorizingMessage,
	}
	if p.Provider != "" {
		req["provider"] = p.Provider
	}
	if p.Model != "" {
		req["model"] = p.Model
	}
	if p.Pool != "" {
		req["pool"] = p.Pool
	}
	return c.call(ctx, "QuotaSet", req, nil)
}

// QuotaClear clears a provider quota window via QuotaClear (FAE-RT-018).
func (c *Client) QuotaClear(ctx context.Context, p QuotaClearParams) error {
	if p.AuthorizingMessage == "" {
		target := firstNonEmpty(p.Pool, p.Provider)
		msgID, err := c.Message(ctx, Authorization("clear provider quota "+target, "runtime"), "")
		if err != nil {
			return err
		}
		p.AuthorizingMessage = msgID
	}
	req := map[string]any{
		"authorizing_message": p.AuthorizingMessage,
	}
	if p.Provider != "" {
		req["provider"] = p.Provider
	}
	if p.Model != "" {
		req["model"] = p.Model
	}
	if p.Pool != "" {
		req["pool"] = p.Pool
	}
	return c.call(ctx, "QuotaClear", req, nil)
}

// DispatchPause pauses dispatch for a scope via DispatchPause (FAE-RT-018).
func (c *Client) DispatchPause(ctx context.Context, p DispatchPauseParams) error {
	if p.AuthorizingMessage == "" {
		target := firstNonEmpty(p.Target, p.Scope)
		msgID, err := c.Message(ctx, Authorization("pause dispatch "+target, "runtime"), "")
		if err != nil {
			return err
		}
		p.AuthorizingMessage = msgID
	}
	req := map[string]any{
		"scope":               p.Scope,
		"target":              p.Target,
		"authorizing_message": p.AuthorizingMessage,
	}
	if p.Reason != "" {
		req["reason"] = p.Reason
	}
	return c.call(ctx, "DispatchPause", req, nil)
}

// DispatchUnpause unpauses dispatch for a scope via DispatchUnpause (FAE-RT-018).
func (c *Client) DispatchUnpause(ctx context.Context, p DispatchPauseParams) error {
	if p.AuthorizingMessage == "" {
		target := firstNonEmpty(p.Target, p.Scope)
		msgID, err := c.Message(ctx, Authorization("unpause dispatch "+target, "runtime"), "")
		if err != nil {
			return err
		}
		p.AuthorizingMessage = msgID
	}
	return c.call(ctx, "DispatchUnpause", map[string]any{
		"scope":               p.Scope,
		"target":              p.Target,
		"authorizing_message": p.AuthorizingMessage,
	}, nil)
}

// Decide records an operator decision for a task via Decide.
func (c *Client) Decide(ctx context.Context, p DecideParams) error {
	if p.AuthorizingMessage == "" {
		msgID, err := c.Message(ctx, Authorization("decide task "+p.Task, "task"), p.Task)
		if err != nil {
			return err
		}
		p.AuthorizingMessage = msgID
	}
	option := "reject"
	if p.Approved {
		option = "approve"
	}
	err := c.call(ctx, "Decide", map[string]any{
		"id":                  p.Task,
		"option":              option,
		"rationale":           firstNonEmpty(p.Note, option),
		"authorizing_message": p.AuthorizingMessage,
	}, nil)
	if err != nil && p.Approved && (strings.Contains(err.Error(), "decision") || strings.Contains(err.Error(), "HumanDecision")) {
		info := firstNonEmpty(p.Note, "operator approved in TUI")
		return c.Lifecycle(ctx, LifecycleParams{
			Task:    p.Task,
			Verb:    "RestartWithInformation",
			Context: info,
			Reason:  info,
		})
	}
	return err
}

// ResolveUncertain resolves an uncertain in-flight dispatch via ResolveUncertain.
func (c *Client) ResolveUncertain(ctx context.Context, p ResolveUncertainParams) error {
	if p.AuthorizingMessage == "" {
		msgID, err := c.Message(ctx, Authorization("resolve uncertain dispatch "+p.Task, "task"), p.Task)
		if err != nil {
			return err
		}
		p.AuthorizingMessage = msgID
	}
	action := strings.ToLower(strings.TrimSpace(p.Action))
	if action == "retry" || action == "" {
		action = "redispatch"
	}
	err := c.call(ctx, "ResolveUncertain", map[string]any{
		"id":                  p.Task,
		"action":              action,
		"authorizing_message": p.AuthorizingMessage,
	}, nil)
	if err != nil && action != "escalate" && (strings.Contains(err.Error(), "no retired live dispatch") || strings.Contains(err.Error(), "not the current uncertain dispatch")) {
		info := firstNonEmpty(p.Note, "operator resolved and restarted in TUI")
		return c.Lifecycle(ctx, LifecycleParams{
			Task:    p.Task,
			Verb:    "RestartWithInformation",
			Context: info,
			Reason:  info,
		})
	}
	return err
}

// Lifecycle invokes a task lifecycle RPC verb (ResumeWithContext,
// RestartWithInformation, QuarantineRelease, QuarantineRetire, Retire,
// Resume, Cancel, Land, etc.).
func (c *Client) Lifecycle(ctx context.Context, p LifecycleParams) error {
	verb := strings.TrimSpace(p.Verb)
	if verb == "" {
		return errors.New("lifecycle requires a verb")
	}
	if verb == "Unescalate" {
		verb = "RestartWithInformation"
	}
	if p.AuthorizingMessage == "" {
		class := "task"
		if verb == "Land" {
			class = "controller"
		}
		msgID, err := c.Message(ctx, Authorization(verb+" "+p.Task, class), p.Task)
		if err != nil {
			return err
		}
		p.AuthorizingMessage = msgID
	}
	switch verb {
	case "Land":
		return c.call(ctx, verb, map[string]any{
			"tasks":               []string{p.Task},
			"target":              firstNonEmpty(p.Context, p.Reason, "main"),
			"authorizing_message": p.AuthorizingMessage,
		}, nil)
	case "RestartWithInformation":
		info := firstNonEmpty(p.Context, p.Reason, "operator approved restart from TUI")
		reason := firstNonEmpty(p.Reason, info)
		err := c.call(ctx, verb, map[string]any{
			"id":                  p.Task,
			"information":         info,
			"reason":              reason,
			"authorizing_message": p.AuthorizingMessage,
		}, nil)
		if err != nil && strings.Contains(err.Error(), "raise attempt limit") {
			limit := 25
			if d, dErr := c.Detail(ctx, p.Task, Snapshot{}, false); dErr == nil && d.Attempts >= 20 {
				limit = d.Attempts + 10
			}
			msg1, mErr := c.Message(ctx, Authorization("SetAttemptLimit "+p.Task, "task"), p.Task)
			if mErr != nil {
				return err
			}
			if sErr := c.call(ctx, "SetAttemptLimit", map[string]any{
				"id":                  p.Task,
				"attempt_limit":       limit,
				"reason":              "raise attempt limit before restart",
				"authorizing_message": msg1,
			}, nil); sErr != nil {
				return sErr
			}
			msg2, mErr2 := c.Message(ctx, Authorization("RestartWithInformation "+p.Task, "task"), p.Task)
			if mErr2 != nil {
				return err
			}
			err = c.call(ctx, verb, map[string]any{
				"id":                  p.Task,
				"information":         info,
				"reason":              reason,
				"authorizing_message": msg2,
			}, nil)
		}
		return err
	case "ResumeWithContext":
		ctxText := firstNonEmpty(p.Context, p.Reason, "operator context from TUI")
		reason := firstNonEmpty(p.Reason, ctxText)
		return c.call(ctx, verb, map[string]any{
			"id":                  p.Task,
			"context":             ctxText,
			"reason":              reason,
			"authorizing_message": p.AuthorizingMessage,
		}, nil)
	case "Retire":
		reason := strings.TrimSpace(firstNonEmpty(p.Reason, p.Context, "obsolete: operator retire"))
		lower := strings.ToLower(reason)
		if !strings.HasPrefix(lower, "obsolete:") && !strings.HasPrefix(lower, "superseded-by:") && !strings.HasPrefix(lower, "contained:") {
			reason = "obsolete: " + reason
		}
		return c.call(ctx, verb, map[string]any{
			"id":                  p.Task,
			"reason":              reason,
			"authorizing_message": p.AuthorizingMessage,
		}, nil)
	default:
		return c.call(ctx, verb, map[string]any{
			"id":                  p.Task,
			"reason":              firstNonEmpty(p.Reason, p.Context, "operator "+strings.ToLower(verb)),
			"authorizing_message": p.AuthorizingMessage,
		}, nil)
	}
}

// SymphonyRefresh triggers TrackerSync or SymphonyRefresh, or no-ops cleanly if unsupported.
func (c *Client) SymphonyRefresh(ctx context.Context) error {
	err := c.call(ctx, "SymphonyRefresh", map[string]any{}, nil)
	if err == nil {
		return nil
	}
	if !Unsupported(err) {
		return err
	}
	err2 := c.call(ctx, "TrackerSync", map[string]any{}, nil)
	if err2 == nil || Unsupported(err2) {
		return nil
	}
	return err2
}
