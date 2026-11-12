package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"strings"
)

type EventFilter struct {
	Task       string `json:"task,omitempty"`
	TreeRoot   string `json:"tree_root,omitempty"`
	RunID      string `json:"run_id,omitempty"`
	WorkflowID string `json:"workflow_id,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

func (f EventFilter) empty() bool { return f == EventFilter{} }
func (d *Daemon) enrichEvents(ctx context.Context, rows []controlplane.Event) ([]controlplane.Event, error) {
	cache := map[string]envelope{}
	for i := range rows {
		e := &rows[i]
		if e.JobID == "" {
			continue
		}
		if e.TreeRoot!="" {continue} // new events carry immutable write-time metadata
		j, ok := cache[e.JobID]
		if !ok {
			row, err := d.store.GetJob(ctx, e.JobID)
			if errors.Is(err, sql.ErrNoRows) {
				e.TreeRoot = e.JobID
				continue
			}
			if err != nil {
				return nil, err
			}
			if err = decodeEnvelope(row.Payload, &j); err != nil {
				return nil, err
			}
			cache[e.JobID] = j
		}
		e.TreeRoot = engine.RootTask(j.Job)
		if j.Job.Parent != nil {
			e.ParentTask = j.Job.Parent.Task
		}
		e.RunID = j.Admission.RunID
		e.WorkflowID = j.Job.Document.ID
	}
	return rows, nil
}
func (f EventFilter) match(e controlplane.Event) bool {
	return (f.Task == "" || e.JobID == f.Task) && (f.TreeRoot == "" || e.TreeRoot == f.TreeRoot) && (f.RunID == "" || e.RunID == f.RunID) && (f.WorkflowID == "" || e.WorkflowID == f.WorkflowID) && (f.Kind == "" || e.Kind == f.Kind)
}
func (d *Daemon) filteredEventBatch(ctx context.Context, after int64, limit int, include bool, filter EventFilter) ([]controlplane.Event, int64, bool, *EventGap, error) {
	rows, gap, err := d.retainedEvents(ctx, after, limit, include)
	if err != nil || gap != nil {
		return nil, after, false, gap, err
	}
	rows, err = d.enrichEvents(ctx, rows)
	if err != nil {
		return nil, after, false, nil, err
	}
	cursor := after
	out := []controlplane.Event{}
	for _, e := range rows {
		cursor = e.Sequence
		if filter.match(e) {
			out = append(out, e)
		}
	}
	return out, cursor, len(rows) > 0, nil, nil
}
func (d *Daemon) eventReadFiltered(ctx context.Context, after int64, limit int, include bool, filter EventFilter) (any, error) {
	if filter.empty() {
		rows, err := d.eventRead(ctx, after, limit, include)
		if err != nil {
			return nil, err
		}
		return d.enrichEvents(ctx, rows.([]controlplane.Event))
	}
	out := []controlplane.Event{}
	for len(out) < limit {
		rows, cursor, more, gap, err := d.filteredEventBatch(ctx, after, min(1000, limit-len(out)), include, filter)
		if err != nil {
			return nil, err
		}
		if gap != nil {
			raw, _ := json.Marshal(gap)
			return nil, &api.RPCError{Code: "event-gap", Message: string(raw)}
		}
		out = append(out, rows...)
		after = cursor
		if !more {
			break
		}
	}
	return out, nil
}

type eventFilterGap struct{ message string }

func (e *eventFilterGap) Error() string         { return e.message }
func parseEventFilterValue(value string) string { return strings.TrimSpace(value) }
