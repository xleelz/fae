package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type eventMetadata struct {
	Task     string `json:"task"`
	Parent   string `json:"parent_task"`
	Root     string `json:"tree_root"`
	Run      string `json:"run_id,omitempty"`
	Workflow string `json:"workflow_id,omitempty"`
}
type storedTaskEvent struct {
	Version  int             `json:"fae_task_event_version"`
	Metadata eventMetadata   `json:"metadata"`
	Payload  json.RawMessage `json:"payload"`
}

func taskEventMetadata(ctx context.Context, tx *sql.Tx, id string) (eventMetadata, error) {
	m := eventMetadata{Task: id, Root: id}
	err := tx.QueryRowContext(ctx, `SELECT x.parent_task,x.tree_root,t.run_id,j.workflow_id FROM task_tree x JOIN tasks t ON t.task_id=x.task_id JOIN jobs j ON j.job_id=x.task_id WHERE x.task_id=?`, id).Scan(&m.Parent, &m.Root, &m.Run, &m.Workflow)
	if errors.Is(err, sql.ErrNoRows) {
		return m, nil
	}
	return m, err
}

// restoreEventMetadata removes only the trusted outer storage envelope. The
// original payload remains the public payload, including for spilled events.
func (s *Store) restoreEventMetadata(ctx context.Context, events []Event) ([]Event, error) {
	for i := range events {
		ev := &events[i]
		if ev.JobID == "" {
			continue
		}
		raw := ev.Payload
		if ref, ok := blobRef(string(raw)); ok {
			// New wrappers contain at most one inline-sized body and bounded IDs.
			// Larger references are legacy bodies: retain their public reference
			// without materializing them merely to look for the new envelope.
			if ref.Size > 128<<10 {
				continue
			}
			var err error
			raw, err = s.GetBlob(ctx, ref)
			if err != nil {
				return nil, err
			}
		}
		var stored storedTaskEvent
		if json.Unmarshal(raw, &stored) != nil || stored.Version != 1 {
			continue
		}
		if stored.Metadata.Task != ev.JobID || stored.Metadata.Root == "" || !json.Valid(stored.Payload) {
			return nil, errors.New("invalid stored task event metadata")
		}
		ev.ParentTask, ev.TreeRoot, ev.RunID, ev.WorkflowID = stored.Metadata.Parent, stored.Metadata.Root, stored.Metadata.Run, stored.Metadata.Workflow
		ev.Payload = stored.Payload
	}
	return events, nil
}
