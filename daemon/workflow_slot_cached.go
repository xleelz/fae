package daemon

import (
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/scheduler"
	"time"
)

// A granted commitment is the authority for its existing members. Host samples,
// ledger availability and another member's pending increase cannot revoke it.
func grantedWorkflowSlotCovers(held, desired scheduler.WorkflowHolder) bool {
	if held.ID != desired.ID || held.Workflow != desired.Workflow || held.Revision != desired.Revision || held.Scope != desired.Scope || held.Sessions < desired.Sessions || held.Panes < desired.Panes {
		return false
	}
	heldBase := held.BaseSessions
	if heldBase == 0 {
		heldBase = held.Sessions
		for _, n := range held.DetachedSessions {
			heldBase -= n
		}
	}
	desiredBase := desired.BaseSessions
	if desiredBase == 0 {
		desiredBase = desired.Sessions
		for _, n := range desired.DetachedSessions {
			desiredBase -= n
		}
	}
	if desiredBase > heldBase {
		return false
	}
	for id, n := range desired.DetachedSessions {
		if held.DetachedSessions[id] < n {
			return false
		}
	}
	return true
}
func applyGrantedWorkflowSlot(j *engine.Job, granted scheduler.WorkflowHolder) {
	j.SlotHolder = granted.ID
	j.SlotBinding = &engine.WorkflowSlotBinding{Holder: granted.ID, Scope: granted.Scope, Workflow: granted.Workflow, Revision: granted.Revision, Sessions: granted.Sessions, BaseSessions: granted.BaseSessions, BaseMemory: granted.BaseMemory, DetachedSessions: cloneDetachedSessions(granted.DetachedSessions), Memory: granted.Memory, Panes: granted.Panes}
	j.SlotWaitReason = ""
	j.SlotWaitSince = time.Time{}
	if j.Reason == "WorkflowSlot" {
		j.Parked = false
		j.Reason = ""
	}
}
