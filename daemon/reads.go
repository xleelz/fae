package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

// Keep headroom for the transport envelope and future additive response fields.
const readPageBytes = 900 << 10

type BoardParams struct {
	IncludeRetired bool   `json:"include_retired"`
	After          string `json:"after"`
	Limit          int    `json:"limit"`
}
type BoardPage struct {
	Items             []BoardItem `json:"items"`
	NextCursor        string      `json:"next_cursor,omitempty"`
	SnapshotAt        time.Time   `json:"snapshot_at"`
	SnapshotAgeMillis int64       `json:"snapshot_age_ms"`
}
type HistoryParams struct {
	IncludeRetired bool   `json:"include_retired"`
	ID             string `json:"id"`
	After          int64  `json:"after"`
	Limit          int    `json:"limit"`
	Summary        bool   `json:"summary"`
}
type ArtifactReference struct {
	ExecutionKind    string    `json:"execution_kind"`
	ArtifactID       string    `json:"artifact_id"`
	DispatchID       string    `json:"dispatch_id"`
	Stage            string    `json:"stage"`
	Role             string    `json:"role"`
	Session          string    `json:"session"`
	Tree             string    `json:"tree"`
	ProducedAt       time.Time `json:"produced_at"`
	WorkflowHash     string    `json:"workflow_hash"`
	WorkflowRevision int       `json:"workflow_revision"`
	Provider         string    `json:"provider,omitempty"`
	Model            string    `json:"model,omitempty"`
}
type HistoryPage struct {
	Task               JobIdentity          `json:"task"`
	Events             []controlplane.Event `json:"events"`
	ArtifactReferences []ArtifactReference  `json:"artifact_references"`
	NextCursor         int64                `json:"next_cursor,omitempty"`
}

func readLimit(limit int) (int, error) {
	if limit == 0 {
		return 100, nil
	}
	if limit < 1 || limit > 500 {
		return 0, errors.New("limit must be 1..500")
	}
	return limit, nil
}
func tooLarge(message string) error {
	return &api.RPCError{Code: "response-too-large", Message: message}
}
func fits(value any) bool {
	raw, err := json.Marshal(value)
	return err == nil && len(raw) <= readPageBytes
}
func (d *Daemon) board(raw json.RawMessage) (BoardPage, error) {
	var p BoardParams
	if err := decode(raw, &p); err != nil {
		return BoardPage{}, err
	}
	limit, err := readLimit(p.Limit)
	if err != nil {
		return BoardPage{}, err
	}
	var s statusSnapshot
	if value := d.status.Load(); value != nil {
		s = value.(statusSnapshot)
	}
	if s.Updated.IsZero() || time.Since(s.Updated) > 10*time.Second {
		return BoardPage{}, &api.RPCError{Code: "snapshot-stale", Message: "board snapshot exceeds 10 seconds"}
	}
	if !p.IncludeRetired {
		visible := []BoardItem{}
		for _, t := range s.Tasks {
			if t.State != "Retired" {
				visible = append(visible, t)
			}
		}
		s.Tasks = visible
	}
	out := BoardPage{SnapshotAgeMillis: max(0, time.Since(s.Updated).Milliseconds()), Items: []BoardItem{}, SnapshotAt: s.Updated}
	start := sort.Search(len(s.Tasks), func(i int) bool { return s.Tasks[i].ID > p.After })
	end := min(start+limit, len(s.Tasks))
	// Copy the slice so handlers cannot accidentally mutate the shared snapshot.
	out.Items = append(out.Items, s.Tasks[start:end]...)
	for {
		if end < len(s.Tasks) && len(out.Items) > 0 {
			out.NextCursor = out.Items[len(out.Items)-1].ID
		} else {
			out.NextCursor = ""
		}
		if fits(out) {
			return out, nil
		}
		if len(out.Items) <= 1 {
			return BoardPage{}, tooLarge("one board row exceeds the response limit")
		}
		out.Items = out.Items[:len(out.Items)-1]
		end--
	}
}
func (d *Daemon) history(ctx context.Context, raw json.RawMessage) (any, error) {
	var p HistoryParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.ID == "" || p.After < 0 {
		return nil, errors.New("task id and nonnegative cursor required")
	}
	limit, err := readLimit(p.Limit)
	if err != nil {
		return nil, err
	}
	if !p.Summary {
		if p.After != 0 || p.Limit != 0 {
			return nil, errors.New("history pagination requires summary=true")
		}
		snapshot, err := d.store.GetJob(ctx, p.ID)
		if err != nil {
			return nil, err
		}
		if snapshot.State == "Retired" && !p.IncludeRetired {
			return nil, errors.New("retired task hidden; use include_retired")
		}
		return legacyHistory(snapshot)
	}
	events, err := d.store.JobEvents(ctx, p.ID, p.After, limit+1)
	if err != nil {
		return nil, err
	}
	// Read the current checkpoint after the event page so every event's committed
	// artifact metadata is represented even while the drive continues writing.
	snapshot, err := d.store.GetJob(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if snapshot.State == "Retired" && !p.IncludeRetired {
		return nil, errors.New("retired task hidden; use include_retired")
	}
	var e envelope
	if err = decodeEnvelope(snapshot.Payload, &e); err != nil {
		return nil, err
	}
	return historyPage(snapshot, e.Job.Provenance, events, limit)
}
func historyPage(snapshot controlplane.JobSnapshot, provenance []engine.Provenance, events []controlplane.Event, limit int) (HistoryPage, error) {
	out := HistoryPage{Task: identity(snapshot), Events: []controlplane.Event{}, ArtifactReferences: []ArtifactReference{}}
	n := min(limit, len(events))
	for {
		out.Events = events[:n]
		out.ArtifactReferences = []ArtifactReference{}
		out.NextCursor = 0
		completed := map[string]bool{}
		for _, event := range out.Events {
			var decision engine.Event
			if json.Unmarshal(event.Payload, &decision) == nil && decision.Kind == "transition" && strings.HasPrefix(decision.Cause, "completed ") {
				completed[strings.TrimPrefix(decision.Cause, "completed ")] = true
			}
		}
		for _, p := range provenance {
			if completed[p.DispatchID] {
				out.ArtifactReferences = append(out.ArtifactReferences, ArtifactReference{ExecutionKind: p.ExecutionKind, ArtifactID: p.ArtifactID, DispatchID: p.DispatchID, Stage: p.Stage, Role: p.Role, Session: p.Session, Tree: p.Tree, ProducedAt: p.ProducedAt, WorkflowHash: p.WorkflowHash, WorkflowRevision: p.WorkflowRevision, Provider: p.Provider, Model: p.Model})
			}
		}
		if n < len(events) && n > 0 {
			out.NextCursor = events[n-1].Sequence
		}
		if fits(out) {
			return out, nil
		}
		if n <= 1 {
			return HistoryPage{}, tooLarge("one history event exceeds the response limit; inspect its durable reference")
		}
		n--
	}
}

func legacyHistory(snapshot controlplane.JobSnapshot) (any, error) {
	if !fits(snapshot) {
		return nil, tooLarge("use History summary=true with after/limit to read this task")
	}
	return snapshot, nil
}

func copyCountsWithoutRetired(counts map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range counts {
		if k != "Retired" {
			out[k] = v
		}
	}
	return out
}
