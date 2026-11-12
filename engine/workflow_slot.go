package engine

import "github.com/xleelz/fae/domain"

// WorkflowSlotWait changes readiness only; no dispatch, attempt or recovery
// counter is charged while the host ledger withholds a workflow slot.
func WorkflowSlotWait(input Job, reason string, impossible bool) (Job, []Event, error) {
	j := clone(input)
	events := []Event{}
	if impossible {
		return RefuseHostFootprint(input, reason)
	} else if j.State != domain.Pending {
		j.Parked = true
		j.Reason = "WorkflowSlot"
	}
	j.SlotWaitReason = reason
	return j, events, Validate(j)
}

// WorkflowSlotBinding is the last actually granted commitment, separate from
// a newer document footprint that may still be waiting for repricing.
type WorkflowSlotBinding struct {
	BaseMemory       int64          `json:"base_memory_bytes,omitempty"`
	Holder           string         `json:"holder"`
	Scope            string         `json:"scope"`
	Workflow         string         `json:"workflow"`
	Revision         int            `json:"revision"`
	Sessions         int            `json:"sessions"`
	BaseSessions     int            `json:"base_sessions,omitempty"`
	DetachedSessions map[string]int `json:"detached_sessions,omitempty"`
	Memory           int64          `json:"memory_bytes"`
	Panes            int            `json:"panes"`
}
