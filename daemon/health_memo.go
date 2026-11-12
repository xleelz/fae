package daemon

import (
	"crypto/sha256"
	"strings"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// healthRow is one cached job's status and controller-health facts, memoized
// by its exact payload bytes. publishStatus runs on every checkpoint, so
// without the memo each checkpoint decoded and re-validated every cached job
// (quadratic in the job count).
type healthRow struct {
	digest                                        [32]byte
	decoded, valid, allowed, unsafe, qa, inFlight bool
	parked, pendingDelete                         bool
	kind, runID, mode, reason                     string
	priority                                      int
	escalation                                    domain.EscalationKind
	parent                                        *engine.ParentRef
	child                                         *engine.ChildRef
}

// healthRowOf runs under d.mu.
func (d *Daemon) healthRowOf(id string, snapshot controlplane.JobSnapshot) healthRow {
	digest := sha256.Sum256(snapshot.Payload)
	if r, ok := d.healthRows[id]; ok && r.digest == digest {
		return r
	}
	if d.healthRows == nil || len(d.healthRows) > 2*len(d.jobs)+16 {
		pruned := map[string]healthRow{}
		for k, v := range d.healthRows {
			if _, live := d.jobs[k]; live {
				pruned[k] = v
			}
		}
		d.healthRows = pruned
	}
	r := decodeHealthRow(snapshot.Payload, true)
	r.digest = digest
	d.healthRows[id] = r
	return r
}

// decodeHealthRow reads a checkpoint's row facts; the controller-health facts
// only when health asks for them.
func decodeHealthRow(payload []byte, health bool) healthRow {
	var r healthRow
	var e envelope
	if decodeEnvelope(payload, &e) != nil {
		return r
	}
	r.decoded, r.inFlight, r.parked, r.pendingDelete = true, e.Job.InFlight != nil, e.Job.Parked, e.Deletion != nil
	r.kind, r.runID, r.mode, r.reason = e.Job.ExecutionKind, e.Admission.RunID, e.Admission.Mode, e.Job.Reason
	r.priority, r.escalation = e.Admission.Priority, e.Job.EscalationKind
	r.parent, r.child = e.Job.Parent, e.Job.Child
	if health && engine.Validate(e.Job) == nil {
		r.valid = true
		stage, allowed := e.Job.Document.Stages[string(e.Job.State)]
		r.allowed = allowed
		r.unsafe = allowed && maintenanceEffect(stage.Primitive) || e.Job.InFlight != nil && maintenanceEffect(e.Job.InFlight.Config.Primitive)
		r.qa = allowed && (strings.EqualFold(stage.Role, "qa") || strings.Contains(strings.ToLower(string(e.Job.State)), "qa"))
	}
	return r
}

// boardItem is the job's Board and status row.
func (r healthRow) boardItem(j controlplane.JobSnapshot) BoardItem {
	return BoardItem{JobIdentity{r.kind, j.ID, j.Version, j.WorkflowID, j.WorkflowRevision, j.TargetID, j.TargetRevision, j.State}, r.priority, r.runID, r.mode, r.reason, r.inFlight, r.escalation, j.MoleculeID, r.pendingDelete, r.parent, r.child}
}
