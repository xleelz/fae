package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/symphony"
)

type LinearTaskStatus struct {
	TaskID        string `json:"task_id"`
	WorkspacePath string `json:"workspace_path,omitempty"`
	Identifier    string `json:"identifier,omitempty"`
	State         string `json:"state"`
	Version       int64  `json:"version"`
	Terminal      bool   `json:"terminal"`
	Completed     bool   `json:"completed"`
	NeedsOperator bool   `json:"needs_operator"`
}
type linearTaskBinding struct {
	Version    int    `json:"version"`
	IssueID    string `json:"issue_id"`
	Identifier string `json:"identifier,omitempty"`
	TaskID     string `json:"task_id"`
	Source     string `json:"source_ref"`
}

var linearOpaqueID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,511}$`)
var linearUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func canonicalLinearID(id string) (string, error) {
	if !linearOpaqueID.MatchString(id) {
		return "", errors.New("Linear issue requires a stable nonempty issue ID")
	}
	if linearUUID.MatchString(id) {
		id = strings.ToLower(id)
	}
	return id, nil
}
func linearBindingHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}
func linearBindingMarker(id string) string { return "linear-binding/" + linearBindingHash(id) }
func linearStableTaskID(id string) string  { return "linear-" + linearBindingHash(id) }
func linearSource(id string) string        { return "linear/" + id }

// linearJobLocked includes durable terminal and quarantined checkpoints; a
// quarantine must not turn an already admitted issue into a second task.
func (d *Daemon) linearJobLocked(ctx context.Context, id string) (controlplane.JobSnapshot, bool, error) {
	if j, ok := d.jobs[id]; ok {
		return j, true, nil
	}
	j, e := d.store.GetJob(ctx, id)
	if errors.Is(e, sql.ErrNoRows) {
		return j, false, nil
	}
	return j, e == nil, e
}
func linearStatus(j controlplane.JobSnapshot, identifier string) LinearTaskStatus {
	s := LinearTaskStatus{TaskID: j.ID, Identifier: identifier, State: j.State, Version: j.Version, Terminal: backlogTerminal(j.State), Completed: j.State == "Done", NeedsOperator: j.State == "Escalated" || j.State == "Blocked"}
	var env envelope
	if decodeEnvelope(j.Payload, &env) == nil && env.Job.Parked && env.Job.Reason == engine.ParkHumanDecision {
		s.NeedsOperator = true
	}
	return s
}

func (d *Daemon) linearStatusLocked(j controlplane.JobSnapshot, identifier string) LinearTaskStatus {
	status := linearStatus(j, identifier)
	if d.quarantined[j.ID] != "" {
		status.NeedsOperator = true
	}
	if d.live != nil && d.live.manager != nil {
		var env envelope
		if decodeEnvelope(j.Payload, &env) == nil && env.Admission.RunID != "" {
			status.WorkspacePath = d.live.manager.Binding(env.Admission.RunID, j.ID).Path
		}
	}
	return status
}

// lookupLinearBindingLocked checks the atomic backlog source before accepting
// a marker. Markers are recoverable indexes, never sole admission authority.
func (b *DaemonTaskBridge) lookupLinearBindingLocked(ctx context.Context, id string) (linearTaskBinding, controlplane.JobSnapshot, bool, bool, error) {
	graph, e := b.daemon.store.Backlog(ctx)
	if e != nil {
		return linearTaskBinding{}, controlplane.JobSnapshot{}, false, false, e
	}
	return b.lookupLinearBindingIndexedLocked(ctx, id, indexLinearSources(graph))
}
func indexLinearSources(graph controlplane.BacklogGraph) map[string][]controlplane.BacklogItem {
	out := map[string][]controlplane.BacklogItem{}
	for _, item := range graph.Items {
		if strings.HasPrefix(item.Source, "linear/") {
			out[item.Source] = append(out[item.Source], item)
		}
	}
	return out
}
func (b *DaemonTaskBridge) lookupLinearBindingIndexedLocked(ctx context.Context, id string, index map[string][]controlplane.BacklogItem) (linearTaskBinding, controlplane.JobSnapshot, bool, bool, error) {
	d := b.daemon
	record := linearTaskBinding{}
	hasMarker := false
	raw, e := d.store.GetMarker(ctx, linearBindingMarker(id))
	if e == nil {
		if e = decode(raw, &record); e != nil {
			return record, controlplane.JobSnapshot{}, false, false, fmt.Errorf("invalid Linear binding marker: %w", e)
		}
		if record.Version != 1 || record.IssueID != id || record.TaskID == "" || record.Source != linearSource(id) {
			return record, controlplane.JobSnapshot{}, false, false, errors.New("Linear binding marker identity mismatch")
		}
		hasMarker = true
	} else if !errors.Is(e, sql.ErrNoRows) {
		return record, controlplane.JobSnapshot{}, false, false, e
	}
	matches := index[linearSource(id)]
	if len(matches) > 1 {
		return record, controlplane.JobSnapshot{}, false, false, errors.New("multiple FAE tasks bind the same Linear issue ID")
	}
	if len(matches) == 0 {
		if hasMarker {
			return record, controlplane.JobSnapshot{}, false, false, errors.New("Linear binding marker has no matching atomic backlog source")
		}
		return record, controlplane.JobSnapshot{}, false, false, nil
	}
	if hasMarker && record.TaskID != matches[0].ID {
		return record, controlplane.JobSnapshot{}, false, false, errors.New("Linear binding marker conflicts with backlog source")
	}
	if !hasMarker {
		record = linearTaskBinding{Version: 1, IssueID: id, TaskID: matches[0].ID, Source: linearSource(id)}
	}
	j, ok, e := d.linearJobLocked(ctx, record.TaskID)
	if e != nil {
		return record, j, false, false, e
	}
	if !ok {
		return record, j, false, false, errors.New("Linear backlog binding has no durable task")
	}
	return record, j, true, !hasMarker, nil
}
func (b *DaemonTaskBridge) writeLinearBindingLocked(ctx context.Context, record linearTaskBinding, a controlplane.Audit) error {
	raw, e := json.Marshal(record)
	if e != nil {
		return e
	}
	return b.daemon.store.PutMarker(ctx, a, linearBindingMarker(record.IssueID), raw)
}

func (b *DaemonTaskBridge) admitLinearIssueDurable(ctx context.Context, issue symphony.Issue, workspacePath, title, prompt string, priority int) (string, error) {
	if b == nil || b.daemon == nil {
		return "", errors.New("daemon task bridge is nil")
	}
	id, e := canonicalLinearID(issue.ID)
	if e != nil {
		return "", e
	}
	if e = symphony.ValidateFaeConfig(b.config); e != nil {
		return "", e
	}
	d := b.daemon
	d.mu.Lock()
	defer d.mu.Unlock()
	binding, _, found, repair, e := b.lookupLinearBindingLocked(ctx, id)
	if e != nil {
		return "", e
	}
	audit := controlplane.Audit{Actor: "symphony", Action: "LinearAdmit", MessageID: b.config.AuthorizingMessage, Outcome: "ok"}
	if found && !repair && binding.Identifier == issue.Identifier {
		return binding.TaskID, nil
	}
	if e = d.store.RequireScheduleAuthority(ctx, b.config.AuthorizingMessage); e != nil {
		return "", fmt.Errorf("Linear admission authority: %w", e)
	}
	if found {
		binding.Identifier = issue.Identifier
		if e = b.writeLinearBindingLocked(ctx, binding, audit); e != nil {
			return binding.TaskID, e
		}
		return binding.TaskID, nil
	}
	graph, e := d.store.Backlog(ctx)
	if e != nil {
		return "", e
	}
	for _, item := range graph.Items {
		if issue.Identifier != "" && item.Source == linearSource(issue.Identifier) && item.Source != linearSource(id) {
			return "", fmt.Errorf("legacy Linear identifier binding %s needs explicit UUID migration", item.ID)
		}
	}
	taskID := linearStableTaskID(id)
	if _, exists, e := d.linearJobLocked(ctx, taskID); e != nil {
		return "", e
	} else if exists {
		return "", errors.New("Linear stable task identity exists without matching issue source")
	}
	if priority < 10 {
		priority = 10
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = fmt.Sprintf("Resolve Linear issue %s: %s", issue.Identifier, issue.Title)
	}
	if strings.TrimSpace(title) == "" {
		title = issue.Title
	}
	tree := b.tree
	if tree == "" && d.live == nil {
		tree = "linear-workspace"
	}
	item := BacklogItemParams{AdmitParams: AdmitParams{ID: taskID, WorkflowID: b.config.WorkflowID, Revision: b.config.WorkflowRevision, TargetConfig: b.config.TargetConfig, TargetRevision: b.config.TargetRevision, AuthorizingMessage: b.config.AuthorizingMessage, Mode: "Local", Tree: tree, Title: title, Content: prompt, Priority: priority}, Source: linearSource(id)}
	if _, _, e = d.admitBacklogBatch(ctx, BacklogParams{Items: []BacklogItemParams{item}, AuthorizingMessage: b.config.AuthorizingMessage}, audit, backlogBatch{via: "LinearSync", create: true}); e != nil {
		return "", e
	}
	binding = linearTaskBinding{Version: 1, IssueID: id, Identifier: issue.Identifier, TaskID: taskID, Source: linearSource(id)}
	if e = b.writeLinearBindingLocked(ctx, binding, audit); e != nil {
		return taskID, fmt.Errorf("Linear task admitted; binding index pending recovery: %w", e)
	}
	return taskID, nil
}

func (b *DaemonTaskBridge) LookupLinearTask(ctx context.Context, issueID string) (LinearTaskStatus, bool, error) {
	if b == nil || b.daemon == nil {
		return LinearTaskStatus{}, false, errors.New("daemon task bridge is nil")
	}
	id, e := canonicalLinearID(issueID)
	if e != nil {
		return LinearTaskStatus{}, false, e
	}
	d := b.daemon
	d.mu.Lock()
	defer d.mu.Unlock()
	record, j, found, _, e := b.lookupLinearBindingLocked(ctx, id)
	if e != nil || !found {
		return LinearTaskStatus{}, found, e
	}
	status := d.linearStatusLocked(j, record.Identifier)
	return status, true, nil
}
func (b *DaemonTaskBridge) ListLinearTasks(ctx context.Context) (map[string]LinearTaskStatus, error) {
	if b == nil || b.daemon == nil {
		return nil, errors.New("daemon task bridge is nil")
	}
	d := b.daemon
	d.mu.Lock()
	defer d.mu.Unlock()
	graph, e := d.store.Backlog(ctx)
	if e != nil {
		return nil, e
	}
	index := indexLinearSources(graph)
	out := map[string]LinearTaskStatus{}
	for _, item := range graph.Items {
		if !strings.HasPrefix(item.Source, "linear/") {
			continue
		}
		id := strings.TrimPrefix(item.Source, "linear/")
		canonical, e := canonicalLinearID(id)
		if e != nil || id != canonical {
			continue
		}
		if !linearUUID.MatchString(id) && item.ID != linearStableTaskID(id) {
			continue
		}
		if _, exists := out[id]; exists {
			return nil, errors.New("duplicate durable Linear issue source")
		}
		record, j, found, _, e := b.lookupLinearBindingIndexedLocked(ctx, id, index)
		if e != nil {
			return nil, e
		}
		if !found {
			return nil, errors.New("Linear source has no task")
		}
		status := d.linearStatusLocked(j, record.Identifier)
		out[id] = status
	}
	return out, nil
}
