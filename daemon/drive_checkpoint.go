package daemon

import (
	"bytes"
	"context"
	"errors"

	"github.com/xleelz/fae/controlplane"
)

// A failed acknowledgement is retained independently of a worker completion:
// launch and housekeeping checkpoints can fail before a worker exists.
type jobCheckpoint struct {
	Control          bool
	Before, Proposed controlplane.JobSnapshot
	Completion       string
}

func sameJobCheckpoint(a, b controlplane.JobSnapshot) bool {
	left, e := controlplane.CanonicalJSON(a.Payload)
	if e != nil {
		return false
	}
	right, e := controlplane.CanonicalJSON(b.Payload)
	if e != nil {
		return false
	}
	return a.ID == b.ID && a.Version == b.Version && a.WorkflowID == b.WorkflowID && a.WorkflowRevision == b.WorkflowRevision && a.TargetID == b.TargetID && a.TargetRevision == b.TargetRevision && a.State == b.State && bytes.Equal(left, right)
}

// reconcileJobCheckpoint runs under the serialized writer lock before rearm.
// Only the exact proposed next version can consume a pending completion. An
// unchanged durable row leaves it pending; an unrelated version fails closed.
func (d *Daemon) reconcileJobCheckpoint(ctx context.Context) error {
	p := d.pendingCheckpoint
	if p == nil {
		return nil
	}
	cached, ok := d.jobs[p.Before.ID]
	if !ok || !sameJobCheckpoint(cached, p.Before) {
		return errors.New("pending checkpoint cache changed")
	}
	actual, e := d.store.GetJob(ctx, p.Before.ID)
	if e != nil {
		return e
	}
	if sameJobCheckpoint(actual, p.Before) {
		d.pendingCheckpoint = nil
		return nil
	}
	expected := p.Proposed
	expected.Version = p.Before.Version + 1
	if !sameJobCheckpoint(actual, expected) {
		return errors.New("pending checkpoint durable result diverged")
	}
	if p.Completion != "" {
		if d.pendingCompletion == nil || d.pendingCompletion.id != actual.ID || d.pendingCompletion.dispatch != p.Completion {
			return errors.New("pending checkpoint completion identity changed")
		}
		// The worker already returned; only its checkpoint acknowledgement was lost.
		d.pendingCompletion = nil
		delete(d.active, actual.ID)
	}
	d.jobs[actual.ID] = actual
	d.pendingCheckpoint = nil
	d.publishStatus()
	d.notifyEvents()
	return nil
}
