package daemon

import (
	"context"
	"errors"

	"github.com/xleelz/fae/engine"
)

// carryForward applies an operator's carry-forward re-admission: the earlier
// task must hold the same item content on the same workflow revision, and
// its accepted pre-code evidence seeds the new task (FAE-WF-086).
func (d *Daemon) carryForward(ctx context.Context, p AdmitParams, job engine.Job) (engine.Job, error) {
	if p.CarryFrom == p.ID {
		return job, errors.New("a task cannot carry forward from itself")
	}
	snapshot, err := d.store.GetJob(ctx, p.CarryFrom)
	if err != nil {
		return job, err
	}
	var old envelope
	if err = decodeEnvelope(snapshot.Payload, &old); err != nil {
		return job, err
	}
	if old.Admission.Content != p.Content || old.Admission.WorkflowID != p.WorkflowID || old.Admission.Revision != p.Revision {
		return job, errors.New("carry-forward requires unchanged acceptance text on the same workflow revision")
	}
	return engine.CarryForward(job, old.Job)
}
