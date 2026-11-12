package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// quarantineDispositionsMarker holds operator retirements of quarantined rows.
const quarantineDispositionsMarker = "quarantine-dispositions"

// quarantineBook is the daemon's quarantine state beside d.quarantined: rows
// quarantined by the startup load (recovered later under that run's stop
// marker) and rows an operator retired out of the drive.
type quarantineBook struct {
	start   map[string]stopMarker
	retired map[string]bool
}

func (b *quarantineBook) atStart(id string, marker stopMarker) {
	if b.start == nil {
		b.start = map[string]stopMarker{}
	}
	b.start[id] = marker
}

func (b *quarantineBook) retire(id string) {
	if b.retired == nil {
		b.retired = map[string]bool{}
	}
	b.retired[id] = true
	delete(b.start, id)
}

// QuarantineParams selects a quarantined task for the FAE-ROB-042 verbs.
type QuarantineParams struct {
	ID                 string `json:"id,omitempty"`
	Reason             string `json:"reason,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

// QuarantineReport answers QuarantineInspect.
type QuarantineReport struct {
	Quarantined []QuarantinedTask   `json:"quarantined"`
	Retired     []RetiredQuarantine `json:"retired"`
}

// QuarantinedTask is one quarantined row: why it was isolated, its durable
// identity, whether it re-validates now (Revalidation "ok") and, for a task
// whose task or backlog row diverged from its snapshot, what release repairs.
type QuarantinedTask struct {
	ID           string          `json:"id"`
	Reason       string          `json:"reason"`
	Row          *QuarantinedRow `json:"row,omitempty"`
	Revalidation string          `json:"revalidation"`
	// RowDivergence names the task or backlog row state that differs from
	// the snapshot's (release sets it to the snapshot's), or a missing task
	// row (release refuses).
	RowDivergence string `json:"row_divergence,omitempty"`
}
type QuarantinedRow struct {
	State            string `json:"state"`
	Version          int64  `json:"version"`
	Digest           string `json:"digest"`
	WorkflowID       string `json:"workflow_id"`
	WorkflowRevision int64  `json:"workflow_revision"`
	TargetID         string `json:"target_id"`
	ExecutionKind    string `json:"execution_kind,omitempty"`
	Parked           string `json:"parked,omitempty"`
	InFlight         string `json:"in_flight,omitempty"`
	InFlightStage    string `json:"in_flight_stage,omitempty"`
	InFlightKind     string `json:"in_flight_primitive,omitempty"`
}

// RetiredQuarantine is an operator's durable retirement of one exact row
// (version and canonical payload digest); a changed row is loaded again.
type RetiredQuarantine struct {
	ID      string    `json:"id"`
	Reason  string    `json:"reason"`
	Cause   string    `json:"cause"`
	Version int64     `json:"version"`
	Digest  string    `json:"digest"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

func rowDigest(s controlplane.JobSnapshot) string {
	if canonical, err := controlplane.CanonicalJSON(s.Payload); err == nil {
		return controlplane.Digest(canonical)
	}
	return controlplane.Digest(s.Payload)
}

func (d *Daemon) quarantineDispositions(ctx context.Context) (map[string]RetiredQuarantine, error) {
	out := map[string]RetiredQuarantine{}
	raw, err := d.store.GetMarker(ctx, quarantineDispositionsMarker)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("quarantine dispositions: %w", err)
	}
	return out, nil
}

// loadStoredJobs loads every durable job at startup. One invalid row is
// quarantined with an alert, never a reason to refuse the instance (FAE-ROB-001,
// -042); only the pane-limit floor and infrastructure refuse it. A row an
// operator retired from quarantine stays out of the drive while unchanged.
func (d *Daemon) loadStoredJobs(ctx context.Context, snapshots []controlplane.JobSnapshot, marker stopMarker) error {
	retired, err := d.quarantineDispositions(ctx)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		if r, ok := retired[snapshot.ID]; ok && r.Version == snapshot.Version && r.Digest == rowDigest(snapshot) {
			d.quarantineBook.retire(snapshot.ID)
			continue
		}
		if loadErr := d.loadJob(ctx, snapshot, marker); loadErr != nil {
			var infrastructure *infrastructureError
			if errors.As(loadErr, &infrastructure) {
				return infrastructure.Err
			}
			d.quarantine(ctx, snapshot.ID, loadErr)
			d.quarantineBook.atStart(snapshot.ID, marker)
		}
	}
	return nil
}

// quarantineInspect is the read verb: it never takes the writer lock.
func (d *Daemon) quarantineInspect(ctx context.Context, raw json.RawMessage) (any, error) {
	var p QuarantineParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	report := QuarantineReport{Quarantined: []QuarantinedTask{}, Retired: []RetiredQuarantine{}}
	snapshot, _ := d.status.Load().(statusSnapshot)
	for id, reason := range snapshot.Quarantined {
		if p.ID != "" && id != p.ID {
			continue
		}
		q := QuarantinedTask{ID: id, Reason: reason, Revalidation: "ok"}
		row, err := d.store.GetJob(ctx, id)
		if err != nil {
			q.Revalidation = err.Error()
			report.Quarantined = append(report.Quarantined, q)
			continue
		}
		q.Row = &QuarantinedRow{State: row.State, Version: row.Version, Digest: rowDigest(row), WorkflowID: row.WorkflowID, WorkflowRevision: row.WorkflowRevision, TargetID: row.TargetID}
		var e envelope
		if decodeEnvelope(row.Payload, &e) == nil {
			q.Row.ExecutionKind = e.Job.ExecutionKind
			if e.Job.Parked {
				q.Row.Parked = e.Job.Reason
			}
			if f := e.Job.InFlight; f != nil {
				q.Row.InFlight, q.Row.InFlightStage, q.Row.InFlightKind = f.ID, f.Stage, string(f.Config.Primitive)
			}
		}
		if ref, unrunnable := d.unrunnableRevisionTasks[id]; unrunnable {
			q.Revalidation = unrunnableRevisionQuarantine(ref)
		} else if _, err = d.validateStoredJob(ctx, row); err != nil {
			q.Revalidation = err.Error()
		}
		if divergence, err := d.store.TaskRowDivergence(ctx, id); err != nil {
			q.RowDivergence = err.Error() + "; release refuses: retire the task or restore the plane from a backup"
		} else if divergence != "" {
			q.RowDivergence = divergence + "; release sets them to the snapshot's state"
		}
		report.Quarantined = append(report.Quarantined, q)
	}
	retired, err := d.quarantineDispositions(ctx)
	if err != nil {
		return nil, err
	}
	for id, r := range retired {
		if p.ID == "" || id == p.ID {
			r.ID = id
			report.Retired = append(report.Retired, r)
		}
	}
	sort.Slice(report.Quarantined, func(i, j int) bool { return report.Quarantined[i].ID < report.Quarantined[j].ID })
	sort.Slice(report.Retired, func(i, j int) bool { return report.Retired[i].ID < report.Retired[j].ID })
	return report, nil
}

// quarantinedTarget checks, under d.mu, that p names a quarantined task with
// no dispatch of it still running, and reads its durable row.
func (d *Daemon) quarantinedTarget(ctx context.Context, raw json.RawMessage) (QuarantineParams, controlplane.JobSnapshot, error) {
	var p QuarantineParams
	if err := decode(raw, &p); err != nil {
		return p, controlplane.JobSnapshot{}, err
	}
	if _, ok := d.quarantined[p.ID]; !ok || p.ID == "" {
		return p, controlplane.JobSnapshot{}, fmt.Errorf("task %q is not quarantined", p.ID)
	}
	if d.active[p.ID] {
		return p, controlplane.JobSnapshot{}, fmt.Errorf("a dispatch of quarantined task %s is still running; retry after it completes", p.ID)
	}
	row, err := d.store.GetJob(ctx, p.ID)
	return p, row, err
}

// quarantineRelease returns a repaired row to the drive. The durable row must
// pass the full startup validation now. A task or backlog row whose state
// diverged from the snapshot's (what the start and a plane restart quarantine
// for, FAE-CP-006) is set to the snapshot's state first, in an audited
// transaction, so the next start or plane restart does not quarantine it
// again; a missing task row refuses the release. An in-flight dispatch is
// recovered as the startup load would have; one whose outcome isolation
// dropped at run time is never charged: scripted replays it, live parks it
// UncertainDispatch. A task quarantined because its pinned revision is one
// this binary cannot run (FAE-WF-093) is refused while that stands: it
// leaves quarantine by `fae migrate` or `fae quarantine retire`.
func (d *Daemon) quarantineRelease(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil {
		return nil, errors.New("unresolved checkpoint must reconcile before quarantine release")
	}
	p, row, err := d.quarantinedTarget(ctx, raw)
	if err != nil {
		return nil, err
	}
	if ref, unrunnable := d.unrunnableRevisionTasks[p.ID]; unrunnable {
		return nil, fmt.Errorf("quarantined task %s cannot be released: %s", p.ID, unrunnableRevisionQuarantine(ref))
	}
	if _, err = d.validateStoredJob(ctx, row); err != nil {
		return nil, fmt.Errorf("quarantined task %s still fails validation: %w", p.ID, err)
	}
	repaired, err := d.store.TaskRowDivergence(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("quarantined task %s cannot be released: %w; retire it (fae quarantine retire) or restore the plane from a backup", p.ID, err)
	}
	if repaired != "" {
		repair := audit
		repair.Action, repair.Outcome = "quarantine-release-repair", "repaired"
		repair.Inputs, _ = json.Marshal(map[string]any{"task": p.ID, "version": row.Version, "divergence": repaired})
		if repaired, err = d.store.RepairTaskRows(ctx, repair, p.ID, row.Version); err != nil {
			return nil, fmt.Errorf("quarantined task %s: repairing its rows from the snapshot: %w", p.ID, err)
		}
	}
	body, _ := json.Marshal(map[string]any{"task": p.ID, "version": row.Version, "digest": rowDigest(row), "cause": d.quarantined[p.ID], "repaired": repaired})
	if err = d.store.ControllerAction(ctx, audit, body, false); err != nil {
		return nil, err
	}
	marker, atStart := d.quarantineBook.start[p.ID]
	if !atStart {
		marker = stopMarker{Graceful: true, isolated: true}
	}
	if err = d.loadJob(ctx, row, marker); err != nil {
		d.quarantine(ctx, p.ID, err)
		return nil, fmt.Errorf("quarantined task %s release failed: %w", p.ID, err)
	}
	delete(d.quarantined, p.ID)
	delete(d.quarantineBook.start, p.ID)
	d.notifyEvents()
	d.signal()
	result := map[string]any{"id": p.ID, "released": true, "version": d.jobs[p.ID].Version}
	if repaired != "" {
		result["repaired"] = repaired
	}
	return result, nil
}

// quarantineRetire durably holds one exact quarantined row out of the drive.
// The row is untouched; its backlog dependents stay held.
func (d *Daemon) quarantineRetire(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	p, row, err := d.quarantinedTarget(ctx, raw)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(p.Reason)
	if reason == "" || len(reason) > 1024 {
		return nil, errors.New("quarantine retire requires a non-blank reason of at most 1024 bytes")
	}
	retired, err := d.quarantineDispositions(ctx)
	if err != nil {
		return nil, err
	}
	if len(retired) >= 10000 {
		return nil, errors.New("quarantine disposition bound reached")
	}
	retired[p.ID] = RetiredQuarantine{Reason: reason, Cause: d.quarantined[p.ID], Version: row.Version, Digest: rowDigest(row), Message: audit.MessageID, At: time.Now().UTC()}
	marker, _ := json.Marshal(retired)
	if err = d.store.PutMarker(ctx, audit, quarantineDispositionsMarker, marker); err != nil {
		return nil, err
	}
	delete(d.quarantined, p.ID)
	delete(d.unrunnableRevisionTasks, p.ID)
	d.quarantineBook.retire(p.ID)
	d.notifyEvents()
	return map[string]any{"id": p.ID, "retired": true, "version": row.Version}, nil
}

// unrunnableQuarantinedRow reads, for Migrate, the durable row of a task
// quarantined because its pinned revision is one this binary cannot run
// (FAE-WF-093): migrating it to a revision this binary validates is one of
// its two ways out of quarantine. Any other quarantined task, and any task
// the drive does not hold, is reported as it was.
func (d *Daemon) unrunnableQuarantinedRow(ctx context.Context, id string) (controlplane.JobSnapshot, bool, error) {
	if _, unrunnable := d.unrunnableRevisionTasks[id]; !unrunnable || d.quarantined[id] == "" {
		return controlplane.JobSnapshot{}, false, nil
	}
	row, err := d.store.GetJob(ctx, id)
	return row, err == nil, err
}

// adoptMigrated puts a migrated task's saved row in the drive. A task
// migrated out of an unrunnable-revision quarantine is loaded as a release
// would load it (the full startup validation, on the revision it now pins);
// one that still fails stays quarantined with the new error. The migration
// itself is audited by the plane write.
func (d *Daemon) adoptMigrated(ctx context.Context, saved controlplane.JobSnapshot) error {
	id := saved.ID
	if _, unrunnable := d.unrunnableRevisionTasks[id]; !unrunnable || d.quarantined[id] == "" {
		d.jobs[id] = saved
		return nil
	}
	delete(d.unrunnableRevisionTasks, id)
	delete(d.restoredUnrunnableQuarantine, id)
	marker, atStart := d.quarantineBook.start[id]
	if !atStart {
		marker = stopMarker{Graceful: true, isolated: true}
	}
	if err := d.loadJob(ctx, saved, marker); err != nil {
		delete(d.quarantined, id)
		d.quarantine(ctx, id, err)
		return fmt.Errorf("task %s migrated to revision %d but stays quarantined: %w", id, saved.WorkflowRevision, err)
	}
	delete(d.quarantined, id)
	delete(d.quarantineBook.start, id)
	d.taskLog("task %s migrated to revision %d and released from quarantine (FAE-WF-093)\n", id, saved.WorkflowRevision)
	return nil
}

// quarantineHeld reports a row the drive must not adopt from a plane reload.
func (d *Daemon) quarantineHeld(id string) bool {
	_, quarantined := d.quarantined[id]
	return quarantined || d.quarantineBook.retired[id]
}
