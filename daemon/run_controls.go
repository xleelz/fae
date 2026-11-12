package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// AdmissionResult is the run/start-work-item response (FAE-CLI-011), with the
// molecule the plane bound at admission.
type AdmissionResult struct {
	JobIdentity
	RunID       string             `json:"run_id"`
	MoleculeID  string             `json:"molecule_id,omitempty"`
	Disposition domain.Disposition `json:"disposition"`
	DryRun      bool               `json:"dry_run"`
}

func admissionResult(j controlplane.JobSnapshot) AdmissionResult {
	var e struct {
		Job struct {
			Disposition domain.Disposition `json:"disposition"`
		} `json:"job"`
		Admission struct {
			RunID string `json:"run_id"`
		} `json:"admission"`
	}
	_ = json.Unmarshal(j.Payload, &e)
	d := e.Job.Disposition
	return AdmissionResult{identity(j), e.Admission.RunID, j.MoleculeID, d, d.Mode == domain.Normal && !d.Live}
}

type ResumeRunParams struct {
	RunID              string `json:"run_id,omitempty"`
	Task               string `json:"task,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}
type ResumedTask struct {
	ID      string `json:"id"`
	Stage   string `json:"stage"`
	Version int64  `json:"version"`
	// Carried, for a raised AttemptCap task restarted at its first stage,
	// states each per-task budget the restart carries with its limit.
	Carried *engine.Budgets `json:"carried,omitempty"`
}
type SkippedTask struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

// resumeRun is the stage-aware resume of one task or of every resumable task
// of a run (FAE-CLI-013). The default run is the latest run.
func (d *Daemon) resumeRun(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p ResumeRunParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	members := []string{}
	envelopes := map[string]envelope{}
	// The RPC read the active backlog and the latest run before it took the
	// writer lock (resumeReads); a read that failed or no longer stands is
	// made here.
	active, read := readsBefore(ctx).activeBacklog()
	if !read {
		var err error
		if active, err = d.store.ActiveBacklog(ctx); err != nil {
			return nil, err
		}
	}
	ids, err := d.orderedTaskIDs(controlplane.BacklogGraph{Items: active.Items})
	if err != nil {
		return nil, err
	}
	// Finished tasks startup did not materialise are read by key, so a restart
	// keeps their terminal refusal (FAE-CP-009).
	stored := map[string]controlplane.JobSnapshot{}
	if p.Task != "" {
		s, ok, err := d.storedJob(ctx, p.Task)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("unknown task %q", p.Task)
		}
		ids, stored[p.Task] = []string{p.Task}, s
	} else if p.RunID == "" {
		// The default is the latest run: that of the most recent admission.
		latest, found, read := readsBefore(ctx).latestRun(d.admissionWrites.writes)
		if !read {
			if latest, found, err = d.store.LatestRunID(ctx); err != nil {
				return nil, err
			}
		}
		if p.RunID = d.config.RunID; found {
			p.RunID = latest
		}
	}
	if p.Task == "" && p.RunID != "" {
		if err = d.storedRunTasks(ctx, p.RunID, stored); err != nil {
			return nil, err
		}
		for id := range stored {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		s, ok := stored[id]
		if !ok {
			s = d.jobs[id]
		}
		var e envelope
		if err := decodeEnvelope(s.Payload, &e); err != nil {
			return nil, err
		}
		if p.RunID != "" && e.Admission.RunID != p.RunID {
			if p.Task != "" {
				return nil, fmt.Errorf("task %s is not in run %s", p.Task, p.RunID)
			}
			continue
		}
		members = append(members, id)
		envelopes[id] = e
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("unknown run %q", p.RunID)
	}
	if p.Task == "" && len(stored) > 0 {
		orderMembers(members, envelopes, active.Items)
	}
	if p.RunID == "" {
		p.RunID = envelopes[p.Task].Admission.RunID
	}
	result := map[string]any{"run_id": p.RunID, "resumed": []ResumedTask{}, "skipped": []SkippedTask{}}
	candidates, live := []string{}, 0
	now := time.Now().UTC()
	for _, id := range members {
		e := envelopes[id]
		if e.Job.State.Terminal() {
			if p.Task != "" {
				return nil, fmt.Errorf("task %s is terminal (%s); nothing to resume", id, e.Job.State)
			}
			result["skipped"] = append(result["skipped"].([]SkippedTask), SkippedTask{id, string(e.Job.State), "terminal"})
			continue
		}
		live++
		if e.Job.Disposition.Mode == "" {
			return nil, fmt.Errorf("task %s has no disposition; resume fails closed", id)
		}
		if _, _, err := engine.ApplyControl(e.Job, engine.Control{Method: "Resume", RouteGeneration: d.routeRefusalGeneration()}, now); err != nil {
			if p.Task != "" {
				return nil, err
			}
			result["skipped"] = append(result["skipped"].([]SkippedTask), SkippedTask{id, string(e.Job.State), err.Error()})
			continue
		}
		candidates = append(candidates, id)
	}
	if live == 0 {
		return nil, fmt.Errorf("run %q has no non-terminal task to resume", p.RunID)
	}
	for _, id := range candidates {
		raw, _ := json.Marshal(TaskControlParams{ID: id, AuthorizingMessage: p.AuthorizingMessage})
		out, err := d.taskControl(ctx, "Resume", raw, audit)
		if err != nil {
			return nil, fmt.Errorf("resumed %v before %s failed: %w", result["resumed"], id, err)
		}
		row := out.(map[string]any)
		carried, _ := row["carried"].(*engine.Budgets)
		result["resumed"] = append(result["resumed"].([]ResumedTask), ResumedTask{id, row["state"].(string), row["version"].(int64), carried})
	}
	return result, nil
}

// storedRunTasks reads by key the run's finished tasks that startup left on
// the plane. The caller holds d.mu; the RPC read the run's members and rows
// before taking it (resumeReads). A run's finished members not materialised
// at startup are fixed, and a task admitted since is in d.jobs.
func (d *Daemon) storedRunTasks(ctx context.Context, run string, stored map[string]controlplane.JobSnapshot) error {
	if len(d.storedStates(func(string) bool { return true })) == 0 {
		return nil
	}
	members, read := readsBefore(ctx).runMembers(run)
	if !read {
		var err error
		if members, err = d.store.RunTasks(ctx, run, 100000); err != nil {
			return err
		}
	}
	for _, id := range members {
		if _, ok := d.jobs[id]; ok {
			continue
		}
		s, ok, err := d.storedJob(ctx, id)
		if err != nil {
			return err
		}
		if ok {
			stored[id] = s
		}
	}
	return nil
}

// resumeReads reads, before ResumeRun takes the writer lock, what it reads
// from the plane (FAE-ROB-020/-021): the active backlog, the default run, and
// the named task's row or the members of the run it resumes and their
// finished rows. The default run is read again under the lock if an
// admission row was written since; members read for another run are not
// used.
func (d *Daemon) resumeReads(ctx context.Context, raw json.RawMessage, r *writerReads) {
	var p ResumeRunParams
	if decode(raw, &p) != nil {
		return
	}
	_, writes, seam := d.uncachedSince(nil)
	if seam("active-backlog") == nil {
		if active, err := d.store.ActiveBacklog(ctx); err == nil {
			r.active = &active
		}
	}
	if p.Task != "" {
		r.readStored(ctx, d, []string{p.Task})
		return
	}
	run := p.RunID
	if run == "" {
		if seam("latest-run") != nil {
			return
		}
		latest, found, err := d.store.LatestRunID(ctx)
		if err != nil {
			return
		}
		r.latest = &latestRunRead{latest, found, writes}
		if run = d.config.RunID; found {
			run = latest
		}
	}
	if len(d.storedStates(func(string) bool { return true })) == 0 {
		return
	}
	if seam("run-tasks/"+run) != nil {
		return
	}
	members, err := d.store.RunTasks(ctx, run, 100000)
	if err != nil {
		return
	}
	r.members[run] = members
	r.readStored(ctx, d, members)
}

// orderMembers restores orderedTaskIDs' order (priority, admission, id) once
// stored members join the drive's.
func orderMembers(ids []string, envelopes map[string]envelope, items []controlplane.BacklogItem) {
	admitted := map[string]time.Time{}
	for _, item := range items {
		admitted[item.ID] = item.Admitted
	}
	for id, e := range envelopes {
		if !e.AdmittedAt.IsZero() {
			admitted[id] = e.AdmittedAt
		}
	}
	sort.SliceStable(ids, func(i, j int) bool {
		a, b := ids[i], ids[j]
		if pa, pb := envelopes[a].Admission.Priority, envelopes[b].Admission.Priority; pa != pb {
			return pa > pb
		}
		if ta, tb := admitted[a], admitted[b]; !ta.Equal(tb) {
			return !ta.IsZero() && (tb.IsZero() || ta.Before(tb))
		}
		return a < b
	})
}

type ShowParams struct {
	Task           string `json:"task,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	IncludeRetired bool   `json:"include_retired,omitempty"`
}
type ShowTask struct {
	JobIdentity
	RunID       string              `json:"run_id"`
	Priority    int                 `json:"priority"`
	Disposition domain.Disposition  `json:"disposition"`
	Stage       string              `json:"stage"`
	Parked      bool                `json:"parked"`
	InFlight    bool                `json:"in_flight"`
	Reason      string              `json:"reason,omitempty"`
	Escalation  string              `json:"escalation,omitempty"`
	Artifacts   []ArtifactReference `json:"latest_artifacts"`
}

// showTask summarizes one checkpoint: the current stage, the latest artifact
// per stage and any escalation. It never mutates or starts anything.
func showTask(s controlplane.JobSnapshot) (ShowTask, error) {
	var e envelope
	if err := decodeEnvelope(s.Payload, &e); err != nil {
		return ShowTask{}, err
	}
	j := e.Job
	out := ShowTask{JobIdentity: identity(s), RunID: e.Admission.RunID, Priority: e.Admission.Priority, Disposition: j.Disposition, Stage: string(j.State), Parked: j.Parked, InFlight: j.InFlight != nil, Reason: j.Reason, Artifacts: []ArtifactReference{}}
	if j.State == domain.Blocked {
		out.Stage = j.BlockedFrom
	}
	if j.State == domain.Escalated {
		out.Escalation = j.Reason
		if out.Escalation == "" {
			out.Escalation = "operator"
		}
	}
	latest, order := map[string]int{}, []string{}
	for i, p := range j.Provenance {
		if _, seen := latest[p.Stage]; !seen {
			order = append(order, p.Stage)
		}
		latest[p.Stage] = i
	}
	for _, stage := range order {
		p := j.Provenance[latest[stage]]
		out.Artifacts = append(out.Artifacts, ArtifactReference{ExecutionKind: p.ExecutionKind, ArtifactID: p.ArtifactID, DispatchID: p.DispatchID, Stage: p.Stage, Role: p.Role, Session: p.Session, Tree: p.Tree, ProducedAt: p.ProducedAt, WorkflowHash: p.WorkflowHash, WorkflowRevision: p.WorkflowRevision, Provider: p.Provider, Model: p.Model})
	}
	return out, nil
}

// show is the read-only snapshot of a task or of a run (FAE-CLI-030).
func (d *Daemon) show(ctx context.Context, raw json.RawMessage) (any, error) {
	var p ShowParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if (p.Task == "") == (p.RunID == "") {
		return nil, errors.New("show requires exactly one of task or run_id")
	}
	ids := []string{p.Task}
	if p.RunID != "" {
		// The run index names every member, finished or not (FAE-CP-009).
		var err error
		if ids, err = d.store.RunTasks(ctx, p.RunID, 501); err != nil {
			return nil, err
		}
		if len(ids) == 0 {
			return nil, fmt.Errorf("unknown run %q", p.RunID)
		}
		if len(ids) > 500 {
			return nil, tooLarge("run exceeds 500 tasks; use board and show --task")
		}
	}
	tasks := []ShowTask{}
	for _, id := range ids {
		s, err := d.store.GetJob(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("task %s: %w", id, err)
		}
		if s.State == string(domain.Retired) && !p.IncludeRetired {
			if p.Task != "" {
				return nil, errors.New("retired task hidden; use include_retired")
			}
			continue
		}
		row, err := showTask(s)
		if err != nil {
			return nil, err
		}
		tasks = append(tasks, row)
	}
	out := map[string]any{"tasks": tasks}
	if p.RunID != "" {
		out["run_id"] = p.RunID
	}
	if !fits(out) {
		return nil, tooLarge("show response exceeds the read limit; use show --task")
	}
	return out, nil
}
