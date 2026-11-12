package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/symphony"
	"github.com/xleelz/fae/tracker"
)

type linearSyncReceipt struct {
	TaskID         string `json:"task_id"`
	RequestedState string `json:"requested_state"`
	ObservedState  string `json:"observed_state"`
	CommentVersion int64  `json:"comment_version"`
}

// Writes are explicit opt-ins, independent of reading and admitting tickets.
// State updates are idempotent; comments are best-effort across a crash between
// Linear accepting a comment and the local receipt being committed.
func (b *DaemonTaskBridge) syncTrackerTask(ctx context.Context, adapter tracker.Adapter, issue symphony.Issue, status LinearTaskStatus, cfg symphony.FaeConfig) error {
	if !cfg.SyncState && !cfg.PostComments {
		return nil
	}
	id, err := canonicalLinearID(issue.ID)
	if err != nil {
		return err
	}
	key := "linear-sync/" + linearBindingHash(id)
	b.daemon.mu.Lock()
	if err := b.daemon.store.RequireScheduleAuthority(ctx, cfg.AuthorizingMessage); err != nil {
		b.daemon.mu.Unlock()
		return fmt.Errorf("Linear synchronization authority: %w", err)
	}
	raw, readErr := b.daemon.store.GetMarker(ctx, key)
	b.daemon.mu.Unlock()
	receipt := linearSyncReceipt{TaskID: status.TaskID}
	if readErr == nil {
		if err = json.Unmarshal(raw, &receipt); err != nil || receipt.TaskID != status.TaskID {
			return fmt.Errorf("invalid Linear sync receipt")
		}
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return readErr
	}
	target := "In Progress"
	if status.Completed {
		target = "Done"
	} else if status.Terminal {
		target = "Canceled"
	}
	if cfg.SyncState && !(receipt.RequestedState == target && symphony.NormalizeState(receipt.ObservedState) == symphony.NormalizeState(issue.State)) {
		if symphony.NormalizeState(issue.State) != symphony.NormalizeState(target) {
			updated, updateErr := adapter.TransitionIssueState(ctx, issue.ID, target)
			if updateErr != nil {
				return updateErr
			}
			if updated.ID != issue.ID || updated.State == "" {
				return fmt.Errorf("Linear state update returned no matching issue")
			}
			issue = updated
		}
		receipt.RequestedState, receipt.ObservedState = target, issue.State
	}
	if cfg.PostComments && receipt.CommentVersion != status.Version {
		body := fmt.Sprintf("Fae task `%s`: `%s` (version %d).", status.TaskID, status.State, status.Version)
		if status.NeedsOperator {
			body += " Operator attention is required in Fae."
		}
		if _, err = adapter.AddIssueComment(ctx, issue.ID, body); err != nil {
			return err
		}
		receipt.CommentVersion = status.Version
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	if string(encoded) == string(raw) {
		return nil
	}
	b.daemon.mu.Lock()
	defer b.daemon.mu.Unlock()
	return b.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "symphony", Action: "record Linear tracker synchronization", MessageID: cfg.AuthorizingMessage, Outcome: "ok"}, key, encoded)
}
