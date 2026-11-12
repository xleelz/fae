package daemon

import (
	"context"
	"fmt"
	"sort"

	"github.com/xleelz/fae/symphony"
)

type linearTaskStateBridge interface {
	ListLinearTasks(context.Context) (map[string]LinearTaskStatus, error)
	LookupLinearTask(context.Context, string) (LinearTaskStatus, bool, error)
}

// The embedded mode observes Fae's durable engine; it never starts a second
// coding-agent worker or invents a successful completion from a poll.
func (o *LinearOrchestrator) reconcileBridgeTasks(ctx context.Context) error {
	bridge, ok := o.bridge.(linearTaskStateBridge)
	if !ok {
		return nil
	}
	statuses, err := bridge.ListLinearTasks(ctx)
	if err != nil {
		return o.bridgeError(err)
	}
	o.mu.RLock()
	adapter := o.adapter
	o.mu.RUnlock()
	if adapter == nil {
		return o.bridgeError(fmt.Errorf("missing tracker adapter"))
	}
	ids := []string{}
	for id, status := range statuses {
		if !status.Terminal {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	current := map[string]symphony.Issue{}
	// Bound each API filter rather than sending the entire retained backlog.
	for offset := 0; offset < len(ids); offset += 50 {
		end := min(offset+50, len(ids))
		issues, e := adapter.FetchIssuesByIDs(ctx, ids[offset:end])
		if e != nil {
			return o.bridgeError(e)
		}
		for _, issue := range issues {
			current[issue.ID] = issue
		}
	}
	o.mu.Lock()
	o.bridgeTasks = statuses
	for id, status := range statuses {
		o.IssueToTask[id] = status.TaskID
		o.TaskToIssue[status.TaskID] = id
		o.Claimed[id] = true
		o.cancelRetryTimerLocked(id)
		o.Completed[id] = status.Completed
		issue, found := current[id]
		if found {
			o.upsertIssueRecordLocked(issue, status.TaskID, status.State)
		}
		if !status.NeedsOperator {
			delete(o.LastError, id)
		}
		if status.Terminal || status.NeedsOperator {
			if entry := o.Running[id]; entry != nil && entry.Cancel != nil {
				entry.Cancel()
			}
			delete(o.Running, id)
			o.cancelRetryTimerLocked(id)
			if status.NeedsOperator {
				o.LastError[id] = "Fae task requires operator attention: " + status.State
			}
			continue
		}
		if !found || !symphony.StateInList(issue.State, adapter.ActiveStates()) || !symphony.IssueRoutable(issue, o.RequiredLabels) || !symphony.BlockersCleared(issue, adapter.TerminalStates()) {
			continue
		}
		entry := o.Running[id]
		if entry == nil {
			entry = &RunningIssueEntry{IssueID: id, TaskID: status.TaskID, Identifier: issue.Identifier, StartedAt: o.now().UTC()}
			o.Running[id] = entry
		}
		entry.Issue = issue
		entry.WorkspacePath = status.WorkspacePath
		if entry.Stage != status.State {
			now := o.now().UTC()
			entry.Stage = status.State
			entry.LastEventAt = &now
			entry.LastEvent = "fae:" + status.State
		}
	}
	o.mu.Unlock()
	for id, status := range statuses {
		if status.Terminal {
			continue
		}
		issue, found := current[id]
		if status.NeedsOperator && found && !symphony.StateInList(issue.State, adapter.TerminalStates()) {
			continue
		}
		if !found || !symphony.StateInList(issue.State, adapter.ActiveStates()) || !symphony.IssueRoutable(issue, o.RequiredLabels) || !symphony.BlockersCleared(issue, adapter.TerminalStates()) {
			if err := o.bridge.StopLinearTask(ctx, status.TaskID, "Linear issue no longer active, assigned, labeled, or unblocked"); err != nil {
				return o.bridgeError(err)
			}
			o.mu.Lock()
			delete(o.Running, id)
			o.mu.Unlock()
		}
	}
	return nil
}

func (o *LinearOrchestrator) bridgeError(err error) error {
	o.mu.Lock()
	o.LastTickError = "Fae tracker reconciliation: " + err.Error()
	o.mu.Unlock()
	return err
}

func (o *LinearOrchestrator) restoreBridgeCandidates(ctx context.Context, issues []symphony.Issue) error {
	if _, ok := o.bridge.(linearTaskStateBridge); !ok {
		return nil
	}
	o.mu.Lock()
	statuses := make(map[string]LinearTaskStatus, len(issues))
	for _, issue := range issues {
		if status, found := o.bridgeTasks[issue.ID]; found {
			o.upsertIssueRecordLocked(issue, status.TaskID, status.State)
			statuses[issue.ID] = status
		}
	}
	adapter := o.adapter
	cfg := o.serviceConfig.Fae
	o.mu.Unlock()
	bridge, ok := o.bridge.(*DaemonTaskBridge)
	if !ok || (!cfg.SyncState && !cfg.PostComments) {
		return nil
	}
	for _, issue := range issues {
		if status, found := statuses[issue.ID]; found {
			if err := bridge.syncTrackerTask(ctx, adapter, issue, status, cfg); err != nil {
				return o.bridgeError(err)
			}
		}
	}
	return nil
}
