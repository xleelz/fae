package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// Keep headroom for the transport envelope and future additive response fields.
const readPageBytes = 900 << 10

type BoardParams struct {
	IncludeRetired bool   `json:"include_retired"`
	After          string `json:"after"`
	Limit          int    `json:"limit"`
	// Filters (FAE-API-001); an empty filter matches every task.
	States      []string `json:"states,omitempty"`
	Target      string   `json:"target,omitempty"`
	Workflow    string   `json:"workflow,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	Molecule    string   `json:"molecule,omitempty"`
	RunID       string   `json:"run_id,omitempty"`
	MinPriority int      `json:"min_priority,omitempty"`
	InFlight    bool     `json:"in_flight,omitempty"`
}

// matches applies the board filters to one row.
func (p BoardParams) matches(t BoardItem) bool {
	return (len(p.States) == 0 || slices.Contains(p.States, t.State)) && (p.Target == "" || t.TargetID == p.Target) && (p.Workflow == "" || t.WorkflowID == p.Workflow) &&
		(p.Mode == "" || strings.EqualFold(t.Mode, p.Mode)) && (p.Molecule == "" || t.MoleculeID == p.Molecule) && (p.RunID == "" || t.RunID == p.RunID) &&
		t.Priority >= p.MinPriority && (!p.InFlight || t.InFlight)
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
	// Records pages the relational transitions, artifacts and decisions.
	Records bool `json:"records,omitempty"`
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
	MoleculeID       string    `json:"molecule_id,omitempty"`
	// Transcript points into the agent CLI's own store (FAE-OBS-007).
	Transcript *engine.Transcript   `json:"transcript,omitempty"`
	ReadRoots  map[string]string    `json:"read_roots,omitempty"`
	Reads      *workflow.ReadPolicy `json:"reads,omitempty"`
}
type HistoryPage struct {
	Task               JobIdentity          `json:"task"`
	Events             []controlplane.Event `json:"events"`
	ArtifactReferences []ArtifactReference  `json:"artifact_references"`
	NextCursor         int64                `json:"next_cursor,omitempty"`
	// Messages are the operator messages attached to the task (first page).
	Messages []controlplane.Message `json:"messages,omitempty"`
}

// FAE-OBS-003: a task's history shows every operator message that touched it,
// on the first page, oldest first, the oldest dropped first to fit.
const historyMessages, historyMessageText = 100, 2048

func (d *Daemon) historyMessages(ctx context.Context, task string, after int64) ([]controlplane.Message, error) {
	if after != 0 {
		return nil, nil
	}
	messages, _, err := d.store.TaskMessages(ctx, task, historyMessages, historyMessageText)
	return messages, err
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
func (d *Daemon) board(ctx context.Context, raw json.RawMessage) (BoardPage, error) {
	var p BoardParams
	if err := decode(raw, &p); err != nil {
		return BoardPage{}, err
	}
	limit, err := readLimit(p.Limit)
	if err != nil {
		return BoardPage{}, err
	}
	var s statusSnapshot
	if value := d.cachedStatus(true); value != nil {
		s = value.(statusSnapshot)
	}
	if s.Updated.IsZero() || time.Since(s.Updated) > snapshotStaleness {
		return BoardPage{}, &api.RPCError{Code: "snapshot-stale", Message: "board snapshot exceeds 10 seconds"}
	}
	if len(p.States) > 64 || p.MinPriority < 0 {
		return BoardPage{}, errors.New("board takes at most 64 states and a nonnegative minimum priority")
	}
	keep := func(t BoardItem) bool { return (p.IncludeRetired || t.State != "Retired") && p.matches(t) }
	visible := []BoardItem{}
	for _, t := range s.Tasks {
		if keep(t) {
			visible = append(visible, t)
		}
	}
	states := d.storedStates(func(state string) bool {
		return (p.IncludeRetired || state != "Retired") && (len(p.States) == 0 || slices.Contains(p.States, state))
	})
	// A finished task is never in flight (finishedBoardRow), so an in-flight
	// board needs no finished row.
	if len(states) == 0 || p.InFlight {
		return boardPage(visible, p.After, limit, s.Updated)
	}
	// Finished tasks from before the start come from their cached summary,
	// or, until it is built, from the plane in the read pool, filtered there.
	if sum := d.finishedSummary(); sum != nil {
		stored, _ := sum.after(s, keep, p.After, limit+1)
		return boardPage(mergeByID(idsAfter(visible, p.After), stored, limit+1), p.After, limit, s.Updated)
	}
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	key, err := domain.CanonicalJSON(raw)
	if err != nil {
		return BoardPage{}, err
	}
	value, err := d.readPool.run(ctx, "Board:"+string(key), func(work context.Context) (any, error) {
		rows, err := d.withStoredRows(work, s, visible, controlplane.TerminalFilter{States: states, Target: p.Target, Workflow: p.Workflow, Molecule: p.Molecule, RunID: p.RunID, Mode: p.Mode, MinPriority: p.MinPriority}, keep, p.After, limit+1)
		if err != nil {
			return nil, err
		}
		return boardPage(rows, p.After, limit, s.Updated)
	})
	if err != nil {
		return BoardPage{}, err
	}
	return value.(BoardPage), nil
}

// boardPage is the page of id-ordered rows after the cursor, shrunk to the
// encoded budget.
func boardPage(rows []BoardItem, after string, limit int, at time.Time) (BoardPage, error) {
	out := BoardPage{SnapshotAgeMillis: max(0, time.Since(at).Milliseconds()), Items: []BoardItem{}, SnapshotAt: at}
	start := sort.Search(len(rows), func(i int) bool { return rows[i].ID > after })
	end := min(start+limit, len(rows))
	// Copy the slice so handlers cannot accidentally mutate the shared snapshot.
	out.Items = append(out.Items, rows[start:end]...)
	for i := range out.Items {
		if p := out.Items[i].Parent; p != nil {
			cp := *p
			out.Items[i].Parent = &cp
		}
		if c := out.Items[i].Child; c != nil {
			cp := *c
			if c.Children != nil {
				cp.Children = append([]engine.ChildRef(nil), c.Children...)
			}
			out.Items[i].Child = &cp
		}
	}
	for {
		if end < len(rows) && len(out.Items) > 0 {
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

// withStoredRows merges into the visible snapshot rows after the cursor the
// plane's terminal rows of tasks the snapshot lacks (startup materialises
// only active jobs, FAE-CP-009), so a restart changes no read. It reads pages
// of the rows the filter admits, in id order, until the first want rows are
// certain; want 0 reads every row and refuses a set beyond the response
// limit. Rows come from the job and task columns (finishedBoardRow).
func (d *Daemon) withStoredRows(ctx context.Context, s statusSnapshot, visible []BoardItem, f controlplane.TerminalFilter, keep func(BoardItem) bool, after string, want int) ([]BoardItem, error) {
	visible = idsAfter(visible, after)
	stored, size, chunk := []BoardItem{}, 0, 500
	if want > 0 {
		chunk = min(want, chunk)
	}
	for cursor := after; ; {
		rows, err := d.store.TerminalRowsAfter(ctx, cursor, chunk, f)
		if err != nil {
			return nil, err
		}
		d.finished.count(rows)
		for _, r := range rows {
			cursor = r.ID
			if snapshotHolds(s, r.ID) {
				continue
			}
			d.hydrateFinishedRow(ctx, &r)
			if row, ok := finishedBoardRow(r); ok && keep(row) {
				stored = append(stored, row)
				if want == 0 {
					raw, _ := json.Marshal(row)
					if size += len(raw); size > api.MaxResponseBytes {
						return nil, tooLarge(storedRowsTooLarge)
					}
				}
			}
		}
		if len(rows) < chunk || want > 0 && (len(stored) >= want || len(visible) >= want && cursor >= visible[want-1].ID) {
			break
		}
	}
	return mergeByID(visible, stored, want), nil
}

const storedRowsTooLarge = "task rows exceed the response limit; page them with board"

// idsAfter is the id-ordered rows after the cursor.
func idsAfter(rows []BoardItem, after string) []BoardItem {
	return rows[sort.Search(len(rows), func(i int) bool { return rows[i].ID > after }):]
}

// statusSummary renders the published snapshot; include is (retired, task rows).
// Total counts the finished tasks startup left on the plane; their rows are
// read by statusRead.
func (d *Daemon) statusSummary(include ...bool) any {
	value := d.cachedStatus(false)
	if value == nil {
		return nil
	}
	return d.statusView(value.(statusSnapshot), len(include) > 0 && include[0], len(include) > 1 && include[1])
}

// statusRead is the Status RPC's snapshot: with task rows, those of finished
// tasks startup left on the plane too, from their summary or, until it is
// built, read in the read pool.
func (d *Daemon) statusRead(ctx context.Context, retired, tasks bool) (any, error) {
	value := d.cachedStatus(false)
	if value == nil {
		return nil, nil
	}
	s := value.(statusSnapshot)
	v := d.statusView(s, retired, tasks)
	states := d.storedStates(func(state string) bool { return retired || state != "Retired" })
	if !tasks || len(states) == 0 {
		return v, nil
	}
	// Their count alone can prove the rows too large, with nothing read.
	if d.storedFloor(states) > api.MaxResponseBytes {
		return nil, tooLarge(storedRowsTooLarge)
	}
	if sum := d.finishedSummary(); sum != nil {
		stored, size := sum.after(s, func(t BoardItem) bool { return retired || t.State != "Retired" }, "", 0)
		if size > api.MaxResponseBytes {
			return nil, tooLarge(storedRowsTooLarge)
		}
		all := mergeByID(*v.Tasks, stored, 0)
		v.Tasks, v.Total = &all, len(all)
		return v, nil
	}
	rows, err := d.readPool.run(ctx, "Status:tasks:"+strconv.FormatBool(retired), func(work context.Context) (any, error) {
		return d.withStoredRows(work, s, *v.Tasks, controlplane.TerminalFilter{States: states}, func(BoardItem) bool { return true }, "", 0)
	})
	if err != nil {
		return nil, err
	}
	all := rows.([]BoardItem)
	v.Tasks, v.Total = &all, len(all)
	return v, nil
}

type statusView struct {
	TicketOutbox  ticketStatus   `json:"ticket_outbox"`
	Configuration map[string]any `json:"configuration"`
	Scheduler     map[string]any `json:"scheduler,omitempty"`
	Drive         map[string]any `json:"drive"`
	Maintenance   map[string]any `json:"maintenance"`
	Drained       bool           `json:"drained"`
	Draining      bool           `json:"draining"`
	Active        int            `json:"active"`
	Counts        map[string]int `json:"counts"`
	Total         int            `json:"total"`
	Tasks         *[]BoardItem   `json:"tasks,omitempty"`
	Updated       time.Time      `json:"updated"`
	AgeMillis     int64          `json:"age_ms"`
	// Quarantined tasks are excluded from the drive until restart.
	Quarantined map[string]string `json:"quarantined,omitempty"`
}

func (d *Daemon) statusView(s statusSnapshot, retired, tasks bool) statusView {
	if !retired {
		s.Counts = copyCountsWithoutRetired(s.Counts)
		visible := []BoardItem{}
		for _, t := range s.Tasks {
			if t.State != "Retired" {
				visible = append(visible, t)
			}
		}
		s.Tasks = visible
	}
	var rows *[]BoardItem
	if tasks {
		rows = &s.Tasks
	}
	stored := d.storedCount(d.storedStates(func(state string) bool { return retired || state != "Retired" }))
	return statusView{s.TicketOutbox, s.Configuration, s.Scheduler, s.Drive, s.Maintenance, s.Drained, s.Draining, s.Active, s.Counts, len(s.Tasks) + stored, rows, s.Updated, time.Since(s.Updated).Milliseconds(), s.Quarantined}
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
	if p.Records {
		return d.historyRecords(ctx, p, limit)
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
	// Read the current checkpoint's row after the event page so every event's
	// committed artifact metadata is represented even while the drive continues
	// writing. The snapshot itself is read only for legacy history (FAE-CP-009).
	snapshot, err := d.headSnapshot(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	if snapshot.State == "Retired" && !p.IncludeRetired {
		return nil, errors.New("retired task hidden; use include_retired")
	}
	// References come from the plane's provenance rows. Only a dispatch that
	// completed before records existed (migrated legacy history) is read from
	// the job snapshot.
	dispatches := completedDispatches(events)
	provenance, err := d.store.DispatchProvenance(ctx, p.ID, dispatches)
	if err != nil {
		return nil, err
	}
	recorded := map[string]bool{}
	for _, row := range provenance {
		recorded[row.DispatchID] = true
	}
	legacy := map[string]bool{}
	for _, dispatch := range dispatches {
		if !recorded[dispatch] {
			legacy[dispatch] = true
		}
	}
	if len(legacy) > 0 {
		if snapshot, err = d.store.GetJob(ctx, p.ID); err != nil {
			return nil, err
		}
		var e envelope
		if err = decodeEnvelope(snapshot.Payload, &e); err != nil {
			return nil, err
		}
		for _, x := range e.Job.Provenance {
			if legacy[x.DispatchID] {
				provenance = append(provenance, controlplane.ProvenanceRecord{ArtifactID: x.ArtifactID, TaskID: x.TaskID, DispatchID: x.DispatchID, Stage: x.Stage, Primitive: string(x.Primitive), Role: x.Role, Provider: x.Provider, Model: x.Model, SessionID: x.Session, Transcript: x.Transcript, Tree: x.Tree, Inputs: x.Inputs, ExecutionKind: x.ExecutionKind, WorkflowDigest: x.WorkflowHash, WorkflowRevision: int64(x.WorkflowRevision), ProducedAt: x.ProducedAt, ReadRoots: x.ReadRoots, Reads: x.Reads})
			}
		}
	}
	d.enrichProvenanceFromMemory(p.ID, provenance)
	page, err := historyPage(snapshot, provenance, events, limit)
	if err != nil {
		return nil, err
	}
	if page.Messages, err = d.historyMessages(ctx, p.ID, p.After); err != nil {
		return nil, err
	}
	for len(page.Messages) > 0 && !fits(page) {
		page.Messages = page.Messages[1:]
	}
	return page, nil
}

// completedDispatches names the dispatches whose completion the events record.
func completedDispatches(events []controlplane.Event) []string {
	out := []string{}
	for _, event := range events {
		var decision engine.Event
		if json.Unmarshal(event.Payload, &decision) == nil && decision.Kind == "transition" && strings.HasPrefix(decision.Cause, "completed ") {
			out = append(out, strings.TrimPrefix(decision.Cause, "completed "))
		}
	}
	return out
}
func historyPage(snapshot controlplane.JobSnapshot, provenance []controlplane.ProvenanceRecord, events []controlplane.Event, limit int) (HistoryPage, error) {
	out := HistoryPage{Task: identity(snapshot), Events: []controlplane.Event{}, ArtifactReferences: []ArtifactReference{}}
	n := min(limit, len(events))
	for {
		out.Events = events[:n]
		out.ArtifactReferences = []ArtifactReference{}
		out.NextCursor = 0
		completed := map[string]bool{}
		for _, dispatch := range completedDispatches(out.Events) {
			completed[dispatch] = true
		}
		for _, p := range provenance {
			if completed[p.DispatchID] {
				out.ArtifactReferences = append(out.ArtifactReferences, artifactReference(p))
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

// headSnapshot is a task's row with only the execution-kind marker identity
// reads from its snapshot: the kind of its latest provenance, or, for a task
// with none yet, its (then small) snapshot.
func (d *Daemon) headSnapshot(ctx context.Context, id string) (controlplane.JobSnapshot, error) {
	h, err := d.store.GetJobHead(ctx, id)
	if err != nil {
		return h.JobSnapshot, err
	}
	if h.ExecutionKind == "" {
		return d.store.GetJob(ctx, id)
	}
	h.Payload, _ = json.Marshal(map[string]any{"job": map[string]string{"execution_kind": h.ExecutionKind}})
	return h.JobSnapshot, nil
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

func artifactReference(p controlplane.ProvenanceRecord) ArtifactReference {
	return ArtifactReference{ExecutionKind: p.ExecutionKind, ArtifactID: p.ArtifactID, DispatchID: p.DispatchID, Stage: p.Stage, Role: p.Role, Session: p.SessionID, Tree: p.Tree, ProducedAt: p.ProducedAt, WorkflowHash: p.WorkflowDigest, WorkflowRevision: int(p.WorkflowRevision), Provider: p.Provider, Model: p.Model, MoleculeID: p.MoleculeID, Transcript: p.Transcript, ReadRoots: p.ReadRoots, Reads: p.Reads}
}

func (d *Daemon) enrichProvenanceFromMemory(taskID string, rows []controlplane.ProvenanceRecord) {
	if len(rows) == 0 {
		return
	}
	d.mu.Lock()
	snap, ok := d.jobs[taskID]
	d.mu.Unlock()
	if (!ok || len(snap.Payload) == 0) && d.store != nil {
		needsEnrich := false
		for i := range rows {
			if len(rows[i].ReadRoots) == 0 || rows[i].Reads == nil {
				needsEnrich = true
				break
			}
		}
		if needsEnrich {
			if stored, err := d.store.GetJob(context.Background(), taskID); err == nil {
				snap = stored
				ok = len(snap.Payload) > 0
			}
		}
	}
	if !ok || len(snap.Payload) == 0 {
		return
	}
	var e envelope
	if decodeEnvelope(snap.Payload, &e) != nil || len(e.Job.Provenance) == 0 {
		return
	}
	byArtifact := map[string]engine.Provenance{}
	byDispatch := map[string]engine.Provenance{}
	for _, p := range e.Job.Provenance {
		if p.ArtifactID != "" {
			byArtifact[p.ArtifactID] = p
		}
		if p.DispatchID != "" {
			byDispatch[p.DispatchID] = p
		}
	}
	for i := range rows {
		p, found := byArtifact[rows[i].ArtifactID]
		if !found {
			p, found = byDispatch[rows[i].DispatchID]
		}
		if found {
			if len(rows[i].ReadRoots) == 0 && len(p.ReadRoots) > 0 {
				rows[i].ReadRoots = p.ReadRoots
			}
			if rows[i].Reads == nil && p.Reads != nil {
				rows[i].Reads = p.Reads
			}
		}
	}
}
