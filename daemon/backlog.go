package daemon

import (
	"context"
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
	graph, err := d.store.Backlog(ctx)
	if err != nil {
		return nil, err
	}
	ids := backlogIDs(d.jobs)
	seen := map[string]bool{}
	for _, i := range p.Items {
		if strings.TrimSpace(i.ID) == "" || len(i.ID) > 255 || seen[i.ID] || i.Priority < 10 || strings.TrimSpace(i.Content) == "" {
			return nil, errors.New("unique identity, priority >= 10 and acceptance content required")
		}
		seen[i.ID] = true
		ids[i.ID] = true
		if old, ok := d.jobs[i.ID]; ok && backlogTerminal(old.State) {
			return nil, fmt.Errorf("terminal identity %s: re-admit under a new identity", i.ID)
		}
	}
	all := append(append([]controlplane.BacklogEdge{}, graph.Edges...), p.Edges...)
	if err = controlplane.ValidateBacklogEdges(ids, all); err != nil {
		return nil, err
	}
	// A batch cannot silently add a new hold to an already dispatched dependent.
	existingEdges := map[controlplane.BacklogEdge]bool{}
	for _, e := range graph.Edges {
		existingEdges[e] = true
	}
	for _, e := range p.Edges {
		if old, ok := d.jobs[e.To]; ok && !existingEdges[e] && backlogDispatched(old) {
			return nil, errors.New("new edge to dispatched dependent requires explicit edge add --accept-in-flight")
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
	// Resolve and validate every selector before preparing any worktree.
	for _, i := range p.Items {
		wid := i.WorkflowID
		revision := i.Revision
		if old, ok := d.jobs[i.ID]; ok {
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
		_, doc, e := d.store.GetWorkflow(ctx, wid, revision)
		if e != nil {
			return nil, e
		}
		if e = validWorkflow(doc); e != nil {
			return nil, e
		}
	}
	for _, i := range p.Items {
		var snapshot controlplane.JobSnapshot
		if old, ok := d.jobs[i.ID]; ok {
			updated++
			snapshot = old
			var e envelope
			if err = decodeEnvelope(old.Payload, &e); err != nil {
				return nil, err
			}
			if (i.TargetConfig != "" && i.TargetConfig != e.Admission.TargetConfig) || (i.TargetRevision != 0 && i.TargetRevision != e.Admission.TargetRevision) || (i.TargetHash != "" && i.TargetHash != e.Admission.TargetHash) || (i.WorkflowID != "" && i.WorkflowID != old.WorkflowID) || (i.Revision != 0 && i.Revision != old.WorkflowRevision) || (i.Mode != "" && i.Mode != e.Admission.Mode) || i.CommitExternal != e.Admission.CommitExternal || i.TicketRoom != e.Admission.TicketRoom || i.Destination != e.Admission.Destination || i.PushNamespace != e.Admission.PushNamespace || (i.RunID != "" && i.RunID != e.Admission.RunID) || (i.Tree != "" && i.Tree != e.Admission.Tree) {
				return nil, errors.New("upsert cannot change pinned workflow, execution target or disposition")
			}
			if backlogDispatched(old) && (i.Content != e.Admission.Content || i.Title != e.Admission.Title) {
				return nil, errors.New("dispatched acceptance and title are immutable; append context instead")
			}
			e.Admission.Content = i.Content
			e.Admission.Title = i.Title
			e.Admission.Priority = i.Priority
			snapshot.Payload, err = json.Marshal(e)
			if err != nil {
				return nil, err
			}
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
			snapshot, err = d.prepareAdmission(context.WithValue(ctx, backlogPreparationKey{}, true), b)
			if err != nil {
				return nil, err
			}
		}
		batch = append(batch, controlplane.BacklogAdmission{Job: snapshot, Item: controlplane.BacklogItem{ID: i.ID, Source: i.Source, Priority: i.Priority, AcceptanceDigest: controlplane.Digest([]byte(i.Content)), WriteClass: i.WriteClass}})
	}
	preparationComplete = true // Preserve owned worktrees on uncertain SQL commit; never remove possibly committed work.
	saved, err := d.store.AdmitBacklog(ctx, a, batch, p.Edges)
	if err != nil {
		d.backlogUncertain = true
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
	for _, s := range saved {
		d.jobs[s.ID] = s
	}
	d.publishStatus()
	d.signal()
	d.notifyEvents()
	return map[string]any{"admitted": len(saved) - updated, "updated": updated, "refused": 0}, nil
}
func (d *Daemon) backlogEdge(ctx context.Context, raw json.RawMessage, a controlplane.Audit) (any, error) {
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("checkpoint recovery required")
	}
	var p BacklogEdgeParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	g, err := d.store.Backlog(ctx)
	if err != nil {
		return nil, err
	}
	if err = controlplane.ValidateBacklogEdges(backlogIDs(d.jobs), []controlplane.BacklogEdge{p.BacklogEdge}); err != nil {
		return nil, err
	}
	if !p.Remove {
		if backlogDispatched(d.jobs[p.To]) && !p.AcceptInFlight {
			return nil, errors.New("dependent already dispatched; --accept-in-flight required")
		}
		if p.AcceptInFlight && a.Actor != "operator" {
			return nil, errors.New("only operator may accept an in-flight dependency change")
		}
		if err = controlplane.ValidateBacklogEdges(backlogIDs(d.jobs), append(g.Edges, p.BacklogEdge)); err != nil {
			return nil, err
		}
	}
	if err = d.store.EditBacklogEdge(ctx, a, p.BacklogEdge, p.Remove); err != nil {
		return nil, err
	}
	d.signal()
	d.notifyEvents()
	return map[string]any{"edge": p.BacklogEdge, "removed": p.Remove, "applies_to": "Pending release; active work is not recalled"}, nil
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
	rows, err := d.store.ListJobs(ctx, 100000)
	if err != nil {
		return nil, err
	}
	jobs := map[string]controlplane.JobSnapshot{}
	for _, j := range rows {
		jobs[j.ID] = j
	}
	blocked := blockedBacklog(g, jobs)
	ready := []string{}
	deferrals := map[string]any{}
	items := []controlplane.BacklogItem{}
	dead := []map[string]any{}
	for _, e := range g.Edges {
		if _, exists := jobs[e.From]; e.Kind == "blocking" && (!exists || ((backlogTerminal(jobs[e.From].State) || jobs[e.From].State == "Escalated") && !prerequisiteSatisfied(jobs[e.From]))) {
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
		var e envelope
		if err = decodeEnvelope(j.Payload, &e); err != nil {
			return nil, err
		}
		if e.Job.Deferred && (e.Job.DeferredUntil.IsZero() || time.Now().Before(e.Job.DeferredUntil)) {
			deferrals[i.ID] = e.Job.DeferredUntil
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
	g, err := d.store.Backlog(ctx)
	if err != nil {
		return nil, err
	}
	result := []map[string]string{}
	for _, i := range g.Items {
		j, ok := d.jobs[i.ID]
		if !ok || i.State == j.State {
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

// Whole-plane readback is safe under the daemon writer: a batch is a single SQL
// transaction, and no local snapshots advance while its outcome is uncertain.
func (d *Daemon) reconcileBacklogCheckpoint(ctx context.Context) error {
	if !d.backlogUncertain {
		return nil
	}
	rows, err := d.store.ListJobs(ctx, 100000)
	if err != nil {
		return err
	}
	jobs := map[string]controlplane.JobSnapshot{}
	for _, j := range rows {
		var e envelope
		if err = decodeEnvelope(j.Payload, &e); err != nil {
			return err
		}
		if err = engine.Validate(e.Job); err != nil {
			return err
		}
		jobs[j.ID] = j
	}
	d.jobs = jobs
	d.backlogUncertain = false
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
