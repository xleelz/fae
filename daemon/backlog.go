package daemon

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

type backlogPreparationKey struct{}

type BacklogItemParams struct {
	AdmitParams
	Source     string `json:"source_ref,omitempty"`
	WriteClass string `json:"write_class,omitempty"`
}
type BacklogParams struct {
	Items              []BacklogItemParams        `json:"items"`
	Edges              []controlplane.BacklogEdge `json:"edges"`
	AuthorizingMessage string                     `json:"authorizing_message"`
}
type BacklogEdgeParams struct {
	controlplane.BacklogEdge
	Remove             bool   `json:"remove,omitempty"`
	AcceptInFlight     bool   `json:"accept_in_flight,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}

func backlogTerminal(state string) bool {
	return state == "Done" || state == "Cancelled" || state == "Retired"
}
func backlogIDs(jobs map[string]controlplane.JobSnapshot) map[string]bool {
	ids := map[string]bool{}
	for id := range jobs {
		ids[id] = true
	}
	return ids
}
func (d *Daemon) backlogAdmit(ctx context.Context, raw json.RawMessage, a controlplane.Audit) (any, error) {
	if d.draining || d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("admission paused until drain/checkpoint recovery")
	}
	if len(raw) > 65536 {
		return nil, errors.New("backlog payload exceeds audited 64 KiB bound")
	}
	var p BacklogParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if len(p.Items) == 0 || len(p.Items) > 1000 || len(p.Edges) > 10000 {
		return nil, errors.New("backlog requires 1..1000 items and at most 10000 edges")
	}
	saved, updated, err := d.admitBacklogBatch(ctx, p, a, backlogBatch{via: "BacklogAdmit"})
	if err != nil {
		return nil, err
	}
	return map[string]any{"admitted": len(saved) - updated, "updated": updated, "refused": 0}, nil
}

// backlogBatch is how admitBacklogBatch treats its items.
type backlogBatch struct {
	// via names the verb on an updated item's task-edited event.
	via string
	// create admits new identities only (TaskCreate): an existing one is
	// refused, never updated.
	create bool
	// acceptInFlight is the operator's acceptance of a new edge to an
	// already dispatched dependent (TaskCreate's accept_in_flight), as
	// `backlog edge add --accept-in-flight` accepts one.
	acceptInFlight bool
}

// admitBacklogBatch validates and commits one batch of items and edges
// (FAE-CLI-014): a new identity is admitted, an existing non-terminal one has
// its mutable admission fields updated (the keyed upsert, editTask), and a
// terminal one is refused. It returns the saved rows and how many of them
// were updates. The caller holds d.mu.
func (d *Daemon) admitBacklogBatch(ctx context.Context, p BacklogParams, a controlplane.Audit, opts backlogBatch) ([]controlplane.JobSnapshot, int, error) {
	if d.draining || d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, 0, errors.New("admission paused until drain/checkpoint recovery")
	}
	graph, err := d.store.Backlog(ctx)
	if err != nil {
		return nil, 0, err
	}
	ids, err := d.knownTasks(ctx, graph.Edges, p.Edges)
	if err != nil {
		return nil, 0, err
	}
	seen := map[string]bool{}
	for n := range p.Items {
		if p.Items[n].ID == "" && p.Items[n].ParentTask != "" {
			for suffix := 1; suffix <= 100000; suffix++ {
				id := fmt.Sprintf("%s.detached.%d", p.Items[n].ParentTask, suffix)
				if seen[id] {
					continue
				}
				_, exists, err := d.storedJob(ctx, id)
				if err != nil {
					return nil, 0, err
				}
				if !exists {
					p.Items[n].ID = id
					break
				}
			}
		}
		seen[p.Items[n].ID] = true
	}
	seen = map[string]bool{}
	for _, i := range p.Items {
		if strings.TrimSpace(i.ID) == "" || len(i.ID) > 2048 || seen[i.ID] || i.Priority < 10 || strings.TrimSpace(i.Content) == "" {
			return nil, 0, errors.New("unique identity, priority >= 10 and acceptance content required")
		}
		seen[i.ID] = true
		ids[i.ID] = true
		if old, ok, err := d.storedJob(ctx, i.ID); err != nil {
			return nil, 0, err
		} else if ok && backlogTerminal(old.State) {
			return nil, 0, fmt.Errorf("terminal identity %s: re-admit under a new identity", i.ID)
		} else if ok && opts.create {
			return nil, 0, fmt.Errorf("task %s already exists: a create never updates; edit it with TaskUpdate (fae task update)", i.ID)
		}
	}
	all := append(append([]controlplane.BacklogEdge{}, graph.Edges...), p.Edges...)
	if err = controlplane.ValidateBacklogEdges(ids, all); err != nil {
		return nil, 0, err
	}
	// A batch cannot silently add a new hold to an already dispatched dependent.
	existingEdges := map[controlplane.BacklogEdge]bool{}
	for _, e := range graph.Edges {
		existingEdges[e] = true
	}
	for _, e := range p.Edges {
		old, ok, err := d.storedJob(ctx, e.To)
		if err != nil {
			return nil, 0, err
		}
		if ok && !existingEdges[e] && backlogDispatched(old) && !opts.acceptInFlight {
			return nil, 0, errors.New("new edge to dispatched dependent requires explicit edge add --accept-in-flight")
		}
	}
	prepared := []BacklogItemParams{}
	preparationComplete := false
	defer func() {
		if !preparationComplete && d.live != nil {
			for _, i := range prepared {
				cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_ = d.live.manager.Remove(cleanup, i.RunID, i.ID)
				cancel()
			}
		}
	}()
	batch := make([]controlplane.BacklogAdmission, 0, len(p.Items))
	updated := 0
	reservations := map[string]int{}
	// Resolve and validate every selector before preparing any worktree.
	for _, i := range p.Items {
		wid := i.WorkflowID
		revision := i.Revision
		old, bound := d.jobs[i.ID]
		if bound {
			if wid == "" {
				wid = old.WorkflowID
			}
			if revision == 0 {
				revision = old.WorkflowRevision
			}
		}
		if wid == "" {
			wid = defaults.Workflow().ID
		}
		_, doc, e := d.storedWorkflow(ctx, wid, revision)
		if e != nil {
			return nil, 0, fmt.Errorf("item %s: %w", i.ID, e)
		}
		if _, e = d.parseWorkflow(doc); e != nil {
			return nil, 0, e
		}
		// Upserts of already bound items continue on their pinned revision.
		if !bound {
			if e = d.store.WorkflowActive(ctx, wid); e != nil {
				return nil, 0, fmt.Errorf("item %s: %w", i.ID, e)
			}
		}
	}
	rows := map[string]controlplane.BacklogItem{}
	for _, item := range graph.Items {
		rows[item.ID] = item
	}
	for _, i := range p.Items {
		var snapshot controlplane.JobSnapshot
		item := controlplane.BacklogItem{ID: i.ID, Source: i.Source, Priority: i.Priority, AcceptanceDigest: controlplane.Digest([]byte(i.Content)), WriteClass: i.WriteClass}
		if old, ok := d.jobs[i.ID]; ok {
			updated++
			if old.TargetID != "" {
				var oldEnv envelope
				if decodeEnvelope(old.Payload, &oldEnv) == nil && len(oldEnv.Job.DenyPaths) == 0 {
					tParams := oldEnv.Admission
					if tParams.TargetConfig == "" {
						tParams.TargetConfig = old.TargetID
						tParams.TargetRevision = old.TargetRevision
					}
					if pinnedTgt, tErr := d.pinnedTarget(ctx, tParams); tErr == nil && pinnedTgt != nil && len(pinnedTgt.DenyPaths) > 0 {
						oldEnv.Job.DenyPaths = slices.Clone(pinnedTgt.DenyPaths)
						if b, mErr := json.Marshal(oldEnv); mErr == nil {
							old.Payload = b
						}
					}
				}
			}
			edit, err := backlogUpsert(old, i, rows[i.ID], a, opts.via)
			if err != nil {
				return nil, 0, err
			}
			snapshot, item = edit.snapshot, edit.item
		} else {
			i.AuthorizingMessage = p.AuthorizingMessage
			b, _ := json.Marshal(i.AdmitParams)
			if d.live != nil {
				if i.RunID == "" {
					i.RunID = d.config.RunID
				}
				w := d.live.manager.Binding(i.RunID, i.ID)
				if _, statErr := os.Lstat(w.Path); os.IsNotExist(statErr) {
					prepared = append(prepared, i)
				}
			}
			snapshot, err = d.prepareAdmission(context.WithValue(context.WithValue(ctx, backlogPreparationKey{}, true), treeAdmissionReservationsKey{}, reservations), b)
			if err != nil {
				return nil, 0, err
			}
		}
		if _, existed := d.jobs[snapshot.ID]; !existed {
			var preparedEnv envelope
			if decodeEnvelope(snapshot.Payload, &preparedEnv) == nil && preparedEnv.Job.Parent != nil {
				reservations[engine.RootTask(preparedEnv.Job)]++
			}
		}
		batch = append(batch, controlplane.BacklogAdmission{Job: snapshot, Item: item})
	}
	preparationComplete = true // Preserve owned worktrees on uncertain SQL commit; never remove possibly committed work.
	saved, err := d.commitBacklogBatch(ctx, a, batch, p.Edges)
	if err != nil {
		return nil, 0, err
	}
	return saved, updated, nil
}

// commitBacklogBatch commits a validated batch in one transaction and
// publishes the saved rows. A lost acknowledgement pauses every mutation until
// the batch's rows are read back (reconcileBacklogCheckpoint). The caller
// holds d.mu.
func (d *Daemon) commitBacklogBatch(ctx context.Context, a controlplane.Audit, batch []controlplane.BacklogAdmission, edges []controlplane.BacklogEdge) ([]controlplane.JobSnapshot, error) {
	unlock, lockErr := d.lockOutputOwnership(ctx)
	if lockErr != nil {
		return nil, lockErr
	}
	defer unlock()
	saved, err := d.store.AdmitBacklog(ctx, a, batch, edges)
	if err == nil {
		err = d.publishOutputOwners(saved)
	}
	batchIDs := make([]string, 0, len(batch))
	for _, x := range batch {
		batchIDs = append(batchIDs, x.Job.ID)
	}
	d.noteAdmissionWrite(err, batchIDs...)
	if err != nil {
		d.backlogUncertain = true
		d.backlogUncertainIDs = append(d.backlogUncertainIDs[:0], batchIDs...)
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		recoveryErr := d.reconcileBacklogCheckpoint(readCtx)
		cancel()
		if recoveryErr != nil {
			return nil, fmt.Errorf("backlog commit unresolved; all mutations paused: %w", err)
		}
		committed := true
		for _, want := range saved {
			got, ok := d.jobs[want.ID]
			if !ok || got.Version != want.Version || !sameBacklogPayload(got.Payload, want.Payload) {
				committed = false
			}
		}
		if !committed {
			return nil, err
		}
	}
	for n := range saved {
		saved[n].Events = nil // committed with this version
		d.jobs[saved[n].ID] = saved[n]
	}
	d.publishStatus()
	d.signal()
	d.notifyEvents()
	return saved, nil
}

// backlogUpsert is a backlog admission's keyed upsert of an existing task
// (FAE-CLI-014): its title, acceptance content, priority, source reference
// and write class take the item's values, and each structured field the
// item names (FAE-CLI-014's declared files, test filter and test shape)
// replaces the pinned one. The pinned workflow, execution target and
// disposition cannot change.
func backlogUpsert(old controlplane.JobSnapshot, i BacklogItemParams, row controlplane.BacklogItem, a controlplane.Audit, via string) (taskEditResult, error) {
	var e envelope
	if err := decodeEnvelope(old.Payload, &e); err != nil {
		return taskEditResult{}, err
	}
	if (i.ParentTask != "" && i.ParentTask != e.Admission.ParentTask) || (i.MaxSessions != 0 && i.MaxSessions != e.Admission.MaxSessions) || (i.Inputs != nil && !samePinnedJSON(i.Inputs, e.Admission.Inputs)) || (i.FromParent != nil && !samePinnedJSON(i.FromParent, e.Admission.FromParent)) {
		return taskEditResult{}, errPinnedChange
	}
	if (i.TargetConfig != "" && i.TargetConfig != e.Admission.TargetConfig) || (i.TargetRevision != 0 && i.TargetRevision != e.Admission.TargetRevision) || (i.TargetHash != "" && i.TargetHash != e.Admission.TargetHash) || (i.WorkflowID != "" && i.WorkflowID != old.WorkflowID) || (i.Revision != 0 && i.Revision != old.WorkflowRevision) || (i.Mode != "" && i.Mode != e.Admission.Mode) || i.CommitExternal != e.Admission.CommitExternal || i.TicketRoom != e.Admission.TicketRoom || i.Destination != e.Admission.Destination || i.PushNamespace != e.Admission.PushNamespace || (i.RunID != "" && i.RunID != e.Admission.RunID) || (i.Tree != "" && i.Tree != e.Admission.Tree) {
		return taskEditResult{}, errPinnedChange
	}
	if row.ID == "" {
		row = controlplane.BacklogItem{ID: i.ID, Priority: e.Admission.Priority, AcceptanceDigest: controlplane.Digest([]byte(e.Admission.Content))}
	}
	edit := taskEdit{Title: &i.Title, Content: &i.Content, Priority: &i.Priority, Source: &i.Source, WriteClass: &i.WriteClass}
	if len(i.DeclaredFiles) > 0 {
		edit.DeclaredFiles = &i.DeclaredFiles
	}
	if len(i.ReadOnlyFiles) > 0 {
		edit.ReadOnlyFiles = &i.ReadOnlyFiles
	}
	if i.TestFilter != "" {
		edit.TestFilter = &i.TestFilter
	}
	if i.TestShape != "" {
		edit.TestShape = &i.TestShape
	}
	return editTask(old, e, row, edit, a, via)
}

// errPinnedChange refuses an edit of what admission pinned.
var errPinnedChange = errors.New("upsert cannot change pinned workflow, execution target or disposition")

// taskEdit is an edit of a task's mutable admission fields (FAE-ROB-040);
// a nil field is left unchanged.
type taskEdit struct {
	Title, Content, Source, WriteClass, TestFilter, TestShape *string
	Priority                                                  *int
	DeclaredFiles                                             *[]string
	ReadOnlyFiles                                             *[]string
}

// taskEditResult is an edited task's row to save, with its task-edited event
// when a field changed, and its backlog row.
type taskEditResult struct {
	snapshot controlplane.JobSnapshot
	item     controlplane.BacklogItem
	changed  []string
	// effect is "from-next-stage" when a turn is in flight (it keeps the
	// text it was dispatched with), else "applied".
	effect string
}

// editTask applies edit to a non-terminal task's admission and its backlog
// row (FAE-ROB-040). An edit reaches the next composed brief and the next
// scheduling decision without a restart; a turn already in flight keeps the
// acceptance text it was dispatched with (supersededAcceptance) and the
// declared files, test filter and test shape it was dispatched under
// (supersededStructuredFields): they apply from the task's next dispatch,
// as the content does. The result's snapshot carries a task-edited event
// naming the changed fields; with none changed it carries no event.
func editTask(old controlplane.JobSnapshot, e envelope, row controlplane.BacklogItem, edit taskEdit, a controlplane.Audit, via string) (taskEditResult, error) {
	out := taskEditResult{snapshot: old, item: row, changed: []string{}, effect: "applied"}
	if backlogTerminal(old.State) {
		return out, fmt.Errorf("task %s is %s: a terminal task cannot be edited; admit the work under a new identity", old.ID, old.State)
	}
	if e.Job.InFlight != nil {
		out.effect = "from-next-stage"
	}
	note := func(field string) { out.changed = append(out.changed, field) }
	if edit.Title != nil && *edit.Title != e.Admission.Title {
		e.Admission.Title = *edit.Title
		note("title")
	}
	if edit.Content != nil && *edit.Content != e.Admission.Content {
		if strings.TrimSpace(*edit.Content) == "" {
			return out, errors.New("acceptance content cannot be blank")
		}
		if e.Job.InFlight != nil && (e.Superseded == nil || e.Superseded.Until <= e.Job.Sequence) {
			// The edit reaches the next brief; the turn in flight keeps its text.
			e.Superseded = &supersededAcceptance{Content: e.Admission.Content, Until: e.Job.Sequence + 1}
		}
		e.Admission.Content = *edit.Content
		note("content")
	}
	if edit.Priority != nil && *edit.Priority != e.Admission.Priority {
		if *edit.Priority < 10 {
			return out, errors.New("priority must be at least 10")
		}
		e.Admission.Priority = *edit.Priority
		note("priority")
	}
	if edit.Source != nil && *edit.Source != row.Source {
		row.Source = *edit.Source
		note("source_ref")
	}
	if edit.WriteClass != nil && *edit.WriteClass != row.WriteClass {
		row.WriteClass = *edit.WriteClass
		note("write_class")
	}
	structured := []string{}
	before := e.Admission
	if edit.DeclaredFiles != nil && !slices.Equal(*edit.DeclaredFiles, e.Admission.DeclaredFiles) && (len(*edit.DeclaredFiles) > 0 || len(e.Admission.DeclaredFiles) > 0) {
		e.Admission.DeclaredFiles = slices.Clone(*edit.DeclaredFiles)
		if len(e.Admission.DeclaredFiles) == 0 {
			e.Admission.DeclaredFiles = nil
		}
		e.Job.DeclaredFiles = slices.Clone(e.Admission.DeclaredFiles)
		structured = append(structured, "declared_files")
	}
	if edit.ReadOnlyFiles != nil && !slices.Equal(*edit.ReadOnlyFiles, e.Admission.ReadOnlyFiles) && (len(*edit.ReadOnlyFiles) > 0 || len(e.Admission.ReadOnlyFiles) > 0) {
		e.Admission.ReadOnlyFiles = slices.Clone(*edit.ReadOnlyFiles)
		if len(e.Admission.ReadOnlyFiles) == 0 {
			e.Admission.ReadOnlyFiles = nil
		}
		e.Job.ReadOnlyFiles = slices.Clone(e.Admission.ReadOnlyFiles)
		structured = append(structured, "read_only_files")
	}
	if edit.TestFilter != nil && *edit.TestFilter != e.Admission.TestFilter {
		e.Admission.TestFilter = *edit.TestFilter
		structured = append(structured, "test_filter")
	}
	if edit.TestShape != nil && *edit.TestShape != e.Admission.TestShape {
		e.Admission.TestShape = *edit.TestShape
		structured = append(structured, "test_shape")
	}
	if len(structured) > 0 {
		if err := validateStructuredFields(e.Admission); err != nil {
			return out, err
		}
		if err := workflow.CheckDocumentWritesDisjoint(e.Job.Document, e.Admission.DeclaredFiles); err != nil {
			return out, err
		}
		for _, f := range e.Admission.DeclaredFiles {
			if idx := workflow.MatchDenyPathIndex(e.Job.DenyPaths, f); idx >= 0 {
				return out, fmt.Errorf("declared file %q refused by deny_paths[%d]", f, idx)
			}
		}
		for _, ro := range e.Admission.ReadOnlyFiles {
			if idx := workflow.MatchDenyPathIndex(e.Job.DenyPaths, ro); idx >= 0 {
				return out, fmt.Errorf("read_only_files path %q refused by deny_paths[%d]", ro, idx)
			}
		}
		if e.Job.InFlight != nil && (e.SupersededFields == nil || e.SupersededFields.Until <= e.Job.Sequence) {
			// The edit reaches the next dispatch; the turn in flight keeps
			// the fields it was dispatched under (dispatchAdmission).
			e.SupersededFields = &supersededStructuredFields{DeclaredFiles: slices.Clone(before.DeclaredFiles), TestFilter: before.TestFilter, TestShape: before.TestShape, Until: e.Job.Sequence + 1}
		}
		out.changed = append(out.changed, structured...)
	}
	row.ID, row.Priority, row.AcceptanceDigest = old.ID, e.Admission.Priority, controlplane.Digest([]byte(e.Admission.Content))
	out.item = row
	var err error
	if out.snapshot.Payload, err = json.Marshal(e); err != nil {
		return out, err
	}
	out.snapshot.Events = nil
	if len(out.changed) > 0 {
		detail := map[string]any{"fields": out.changed, "via": via, "effect": out.effect, "actor": a.Actor, "message_id": a.MessageID}
		if slices.Contains(out.changed, "content") {
			detail["acceptance_sha256"] = row.AcceptanceDigest
		}
		if slices.Contains(out.changed, "priority") {
			detail["priority"] = e.Admission.Priority
		}
		if e.Job.InFlight != nil {
			detail["in_flight"] = e.Job.InFlight.ID
		}
		payload, _ := json.Marshal(detail)
		out.snapshot.Events = []controlplane.Event{{Kind: "task-edited", JobID: old.ID, Payload: payload}}
	}
	return out, nil
}
func (d *Daemon) backlogEdge(ctx context.Context, raw json.RawMessage, a controlplane.Audit) (any, error) {
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("checkpoint recovery required")
	}
	var p BacklogEdgeParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	// The edit reads only its dependent's edges, by key, and for a blocking
	// add the walk from the edge's two ends; never the whole graph
	// (FAE-CP-009). edgeReads read the dependent's edges before the writer
	// lock; they are read again here, under it, since another writer may have
	// changed them since. The walk (refuseBlockingCycle) is read only here,
	// under the lock.
	into, err := d.store.DependentEdges(ctx, p.To)
	if err != nil {
		return nil, err
	}
	g := controlplane.BacklogGraph{Edges: into}
	// The RPC read whether the plane stores the edge's two tasks before it
	// took the writer lock (edgeReads); knownTasks reads any it missed there.
	known, err := d.knownTasks(ctx, []controlplane.BacklogEdge{p.BacklogEdge})
	if err != nil {
		return nil, err
	}
	if err = controlplane.ValidateBacklogEdges(known, []controlplane.BacklogEdge{p.BacklogEdge}); err != nil {
		return nil, err
	}
	if !p.Remove {
		dependent, _, err := d.storedJob(ctx, p.To)
		if err != nil {
			return nil, err
		}
		if backlogDispatched(dependent) && !p.AcceptInFlight {
			return nil, errors.New("dependent already dispatched; --accept-in-flight required")
		}
		if p.AcceptInFlight && a.Actor != "operator" {
			return nil, errors.New("only operator may accept an in-flight dependency change")
		}
		if err = d.refuseBlockingCycle(ctx, p.BacklogEdge); err != nil {
			return nil, err
		}
	}
	// The dependent and its prerequisites, finished ones read by key: startup
	// materialises only active jobs (FAE-CP-009). The RPC read their rows
	// before it took the writer lock (edgeReads); storedJob re-checks them.
	jobs := map[string]controlplane.JobSnapshot{}
	for _, id := range readinessIDs(g.Edges, p.BacklogEdge) {
		if s, ok, err := d.storedJob(ctx, id); err != nil {
			return nil, err
		} else if ok {
			jobs[id] = s
		}
	}
	before := readiness(g, jobs, p.To)
	if p.Remove {
		g.Edges = slices.DeleteFunc(slices.Clone(g.Edges), func(e controlplane.BacklogEdge) bool { return e == p.BacklogEdge })
	} else {
		g.Edges = append(slices.Clone(g.Edges), p.BacklogEdge)
	}
	after := readiness(g, jobs, p.To)
	if err = d.store.EditBacklogEdge(ctx, a, p.BacklogEdge, p.Remove); err != nil {
		return nil, err
	}
	d.signal()
	d.notifyEvents()
	effect := "unchanged"
	switch {
	case backlogDispatched(jobs[p.To]):
		effect = "dispatched: applies to its next release; active work is not recalled"
	case before["ready"] == true && after["ready"] == false:
		effect = "dependent now withheld"
	case before["ready"] == false && after["ready"] == true:
		effect = "dependent now releasable"
	}
	return map[string]any{"edge": p.BacklogEdge, "removed": p.Remove, "readiness_effect": map[string]any{"dependent": p.To, "before": before, "after": after, "effect": effect}}, nil
}

// readinessIDs names the tasks an edge edit's readiness effect reads: the
// dependent and every prerequisite of it, the edited edge's included.
func readinessIDs(edges []controlplane.BacklogEdge, edge controlplane.BacklogEdge) []string {
	ids, looked := []string{}, map[string]bool{}
	for _, e := range append(slices.Clone(edges), edge) {
		for _, id := range []string{e.From, e.To} {
			if e.To != edge.To || looked[id] {
				continue
			}
			looked[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// refuseBlockingCycle refuses a blocking edge that would close a cycle,
// naming it (FAE-OPS-014): adding P -> D closes one exactly when D already
// reaches P. Only the walk between the edge's two ends is read
// (BlockingPath); a relates edge never gates, so it closes none.
func (d *Daemon) refuseBlockingCycle(ctx context.Context, e controlplane.BacklogEdge) error {
	if e.Kind != "blocking" {
		return nil
	}
	walk, err := d.store.BlockingPath(ctx, e.To, e.From)
	if err != nil {
		return err
	}
	if len(walk.Path) > 0 {
		return fmt.Errorf("blocking cycle: %s", strings.Join(append([]string{e.From}, walk.Path...), " -> "))
	}
	return nil
}

// edgeReads reads, before BacklogEdge takes the writer lock, whether the
// plane stores the edge's two tasks, then the dependent's edges by key and
// the rows of the dependent and its prerequisites (FAE-ROB-020/-021,
// FAE-CP-009). The dependent's edges are read again under the lock, which
// reads any prerequisite missed here.
func (d *Daemon) edgeReads(ctx context.Context, raw json.RawMessage, r *writerReads) {
	var p BacklogEdgeParams
	if decode(raw, &p) != nil {
		return
	}
	ends := []string{p.From}
	if p.To != p.From {
		ends = append(ends, p.To)
	}
	r.readExisting(ctx, d, ends)
	into, err := d.store.DependentEdges(ctx, p.To)
	if err != nil {
		return
	}
	r.readStored(ctx, d, readinessIDs(into, p.BacklogEdge))
}

// readiness is one item's release readiness under a dependency graph
// (FAE-CLI-015): its state and the prerequisites withholding it.
func readiness(g controlplane.BacklogGraph, jobs map[string]controlplane.JobSnapshot, id string) map[string]any {
	withheld := blockedBacklog(g, jobs)[id]
	if withheld == nil {
		withheld = []string{}
	}
	return map[string]any{"state": jobs[id].State, "ready": jobs[id].State == "Pending" && len(withheld) == 0, "withheld_by": withheld}
}
func blockedBacklog(g controlplane.BacklogGraph, jobs map[string]controlplane.JobSnapshot) map[string][]string {
	blocked := map[string][]string{}
	for _, e := range g.Edges {
		state := "missing"
		if j, ok := jobs[e.From]; ok {
			state = j.State
		}
		if !CanReleaseDependent(e.Kind == "blocking", prerequisiteSatisfied(jobs[e.From])) {
			blocked[e.To] = append(blocked[e.To], e.From+": "+state)
		}
	}
	return blocked
}
func (d *Daemon) backlogShow(ctx context.Context, raw json.RawMessage) (any, error) {
	var p struct {
		State          string `json:"state"`
		IncludeRetired bool   `json:"include_retired"`
	}
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	g, err := d.store.Backlog(ctx)
	if err != nil {
		return nil, err
	}
	// Task rows carry state and reason: the read loads no job snapshot.
	jobs, err := d.store.TaskHeads(ctx)
	if err != nil {
		return nil, err
	}
	blocked := blockedHeads(g, jobs)
	ready := []string{}
	deferrals := map[string]any{}
	items := []controlplane.BacklogItem{}
	dead := []map[string]any{}
	for _, e := range g.Edges {
		if _, exists := jobs[e.From]; e.Kind == "blocking" && (!exists || ((backlogTerminal(jobs[e.From].State) || jobs[e.From].State == "Escalated") && !headSatisfied(jobs[e.From]))) {
			outcome := jobs[e.From].State
			if !exists {
				outcome = "missing"
			}
			dead = append(dead, map[string]any{"edge": e, "prerequisite_outcome": outcome})
		}
	}
	for _, i := range g.Items {
		if (!p.IncludeRetired && i.State == "Retired") || (p.State != "" && i.State != p.State) {
			continue
		}
		items = append(items, i)
		j := jobs[i.ID]
		if i.Deferred && (i.DeferredUntil.IsZero() || time.Now().Before(i.DeferredUntil)) {
			deferrals[i.ID] = i.DeferredUntil
			continue
		}
		if j.State == "Pending" && len(blocked[i.ID]) == 0 {
			ready = append(ready, i.ID)
		}
	}
	if !p.IncludeRetired {
		visible := []controlplane.BacklogEdge{}
		for _, edge := range g.Edges {
			if jobs[edge.From].State != "Retired" && jobs[edge.To].State != "Retired" {
				visible = append(visible, edge)
			}
		}
		g.Edges = visible

		deadVisible := []map[string]any{}
		for _, entry := range dead {
			edge := entry["edge"].(controlplane.BacklogEdge)
			if jobs[edge.To].State == "Retired" {
				continue
			}
			if jobs[edge.From].State == "Retired" {
				entry = map[string]any{"dependent": edge.To, "prerequisite_outcome": "retired prerequisite hidden; use include_retired"}
			}
			deadVisible = append(deadVisible, entry)
		}
		dead = deadVisible
		for id, reasons := range blocked {
			for n, reason := range reasons {
				prerequisite, _, _ := strings.Cut(reason, ": ")
				if jobs[prerequisite].State == "Retired" {
					reasons[n] = "retired prerequisite hidden; use include_retired"
				}
			}
			blocked[id] = reasons
		}
		for id := range blocked {
			if jobs[id].State == "Retired" {
				delete(blocked, id)
			}
		}
	}
	return map[string]any{"items": items, "edges": g.Edges, "ready": ready, "withheld": blocked, "dead_edges": dead, "deferrals": deferrals}, nil
}

func landingSHA(e envelope) string {
	if e.Job.State != domain.Done {
		return ""
	}
	return ownLandingSHA(e)
}

func ownLandingSHA(e envelope) string {
	// Require an integration artifact tied to this task's own integration primitive.
	for n := len(e.Job.Evidence) - 1; n >= 0; n-- {
		a := e.Job.Evidence[n]
		landed, _ := a.Fields["landed"].(bool)
		sha, _ := a.Fields["sha"].(string)
		if _, err := hex.DecodeString(sha); !landed || (len(sha) != 40 && len(sha) != 64) || err != nil {
			continue
		}
		for _, p := range e.Job.Provenance {
			if p.ArtifactID == a.ID && p.TaskID == e.Job.TaskID && p.Primitive == workflow.Integrate && p.ExecutionKind == "live" && e.Job.ExecutionKind == "live" {
				return sha
			}
		}
	}
	return ""
}
func (d *Daemon) reconcileBacklogDone(ctx context.Context, dry bool, a controlplane.Audit) (any, error) {
	// Only the items whose state differs from their job's are read (FAE-CP-009).
	mismatched, err := d.store.BacklogMismatches(ctx, 100000)
	if err != nil {
		return nil, err
	}
	result := []map[string]string{}
	for _, id := range mismatched {
		i := controlplane.BacklogItem{ID: id}
		j, ok, err := d.storedJob(ctx, id)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if j.State != "Done" {
			result = append(result, map[string]string{"id": i.ID, "reason": "task is not Done"})
			continue
		}
		var e envelope
		if err = decodeEnvelope(j.Payload, &e); err != nil {
			return nil, err
		}
		sha := landingSHA(e)
		if sha == "" {
			result = append(result, map[string]string{"id": i.ID, "reason": "Done task has no identifiable own landing SHA"})
			continue
		}
		entry := map[string]string{"id": i.ID, "sha": sha}
		result = append(result, entry)
		if dry {
			continue
		}
		audit := a
		audit.Action = "reconcile backlog Done " + i.ID + " landed " + sha
		audit.Inputs, _ = json.Marshal(entry)
		if err = d.store.ReconcileBacklogDone(ctx, audit, i.ID, sha); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// The uncertain batch's readback is safe under the daemon writer: a batch is a
// single SQL transaction over its own items, and no local snapshot advances
// while its outcome is uncertain, so only the batch's jobs are read back.
func (d *Daemon) reconcileBacklogCheckpoint(ctx context.Context) error {
	if !d.backlogUncertain {
		return nil
	}
	jobs, absent := map[string]controlplane.JobSnapshot{}, []string{}
	for _, id := range d.backlogUncertainIDs {
		j, err := d.store.GetJob(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			absent = append(absent, id)
			continue
		}
		if err != nil {
			return err
		}
		jobs[id] = j
	}
	for _, id := range absent {
		delete(d.jobs, id)
	}
	for id, j := range jobs {
		if d.quarantineHeld(id) {
			continue
		}
		var e envelope
		err := decodeEnvelope(j.Payload, &e)
		if err == nil {
			err = engine.Validate(e.Job)
		}
		if err != nil {
			d.quarantine(ctx, id, taskErr("backlog-reload", err))
			continue
		}
		d.jobs[id] = j
		delete(d.quarantined, id)
	}
	d.backlogUncertain, d.backlogUncertainIDs = false, nil
	d.publishStatus()
	return nil
}

func sameBacklogPayload(a, b json.RawMessage) bool {
	ca, ea := controlplane.CanonicalJSON(a)
	cb, eb := controlplane.CanonicalJSON(b)
	return ea == nil && eb == nil && string(ca) == string(cb)
}

func backlogDispatched(j controlplane.JobSnapshot) bool {
	var e envelope
	return j.State != "Pending" || decodeEnvelope(j.Payload, &e) != nil || e.Job.Sequence > 0
}

// blockedHeads is blockedBacklog over projected task rows.
func blockedHeads(g controlplane.BacklogGraph, heads map[string]controlplane.TaskHead) map[string][]string {
	blocked := map[string][]string{}
	for _, e := range g.Edges {
		h, ok := heads[e.From]
		state := "missing"
		if ok {
			state = h.State
		}
		if !CanReleaseDependent(e.Kind == "blocking", ok && headSatisfied(h)) {
			blocked[e.To] = append(blocked[e.To], e.From+": "+state)
		}
	}
	return blocked
}

// headSatisfied is prerequisiteSatisfied over a projected task row.
func headSatisfied(h controlplane.TaskHead) bool {
	if h.State == "Done" {
		return true
	}
	if h.State != "Retired" {
		return false
	}
	class, _, err := engine.ParseRetirement(h.Reason)
	return err == nil && class == "contained"
}

func prerequisiteSatisfied(j controlplane.JobSnapshot) bool {
	if j.State == "Done" {
		return true
	}
	if j.State != "Retired" {
		return false
	}
	var e envelope
	if decodeEnvelope(j.Payload, &e) != nil {
		return false
	}
	class, _, err := engine.ParseRetirement(e.Job.Reason)
	return err == nil && class == "contained"
}

// supersededAcceptance keeps the acceptance text of dispatches before Until.
type supersededAcceptance struct {
	Content string `json:"content"`
	Until   int    `json:"until"`
}

// supersededStructuredFields keeps the declared files, test filter and test
// shape of dispatches before Until: an edit of them while a turn is in
// flight applies from the task's next dispatch (daemon/TASKS.md), and the
// turn in flight keeps the scope it was dispatched under.
type supersededStructuredFields struct {
	DeclaredFiles []string `json:"declared_files,omitempty"`
	TestFilter    string   `json:"test_filter,omitempty"`
	TestShape     string   `json:"test_shape,omitempty"`
	Until         int      `json:"until"`
}

// dispatchBefore reports whether a dispatch was made before the sequence
// until: its id ends in its sequence number (engine.Dispatch). An id without
// one counts as before, as acceptanceContext always did.
func dispatchBefore(dispatch engine.Dispatch, until int) bool {
	seq, err := strconv.Atoi(dispatch.ID[strings.LastIndex(dispatch.ID, "/")+1:])
	return err != nil || seq < until
}

// dispatchAdmission is the admission a dispatch runs under: the task's, with
// the declared files, test filter and test shape it was dispatched under
// when an edit of them superseded those since (supersededStructuredFields).
// The live scope and change check of a turn in flight read it, so an edit
// made during the turn reaches the next dispatch only.
func dispatchAdmission(e envelope, dispatch engine.Dispatch) AdmitParams {
	a := e.Admission
	if s := e.SupersededFields; s != nil && dispatchBefore(dispatch, s.Until) {
		a.DeclaredFiles, a.TestFilter, a.TestShape = s.DeclaredFiles, s.TestFilter, s.TestShape
	}
	return a
}

// dispatchedEnvelope is e as a dispatch of the task sees it (dispatchAdmission).
func dispatchedEnvelope(e envelope, dispatch engine.Dispatch) envelope {
	e.Admission = dispatchAdmission(e, dispatch)
	return e
}

// acceptanceContext is the acceptance text a dispatch's brief carries, with its
// digest (FAE-ROB-040): an audited edit reaches the next composed brief without
// a restart, never the turn already in flight.
func acceptanceContext(e envelope, dispatch engine.Dispatch) string {
	content := e.Admission.Content
	if s := e.Superseded; s != nil && dispatchBefore(dispatch, s.Until) {
		content = s.Content
	}
	if content == "" {
		return ""
	}
	return "Acceptance (sha256 " + controlplane.Digest([]byte(content)) + "):\n" + content
}
