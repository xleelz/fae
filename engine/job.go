package engine

import (
	"fmt"
	"time"

	"github.com/xleelz/fae/domain"
)

// StatusDone is an alias for domain.Done used by multi-child sub-workflow callers.
const StatusDone = domain.Done

// ClassTurnBudgetExhausted is the typed outcome class recorded when an agentic
// turn exhausts its declared turn_budget (FAE-RT-015).
const ClassTurnBudgetExhausted = "turn-budget-exhausted"

// ClassCheckpointInterrupted is the typed outcome class recorded when an
// agentic turn that already has a durable checkpoint is interrupted by a
// mid-turn process crash or kill (FAE-RT-019).
const ClassCheckpointInterrupted = "checkpoint-interrupted"

// TurnCheckpoint is a durable snapshot of an in-flight or interrupted agentic
// turn's provider session and in-scope worktree git tree hash (FAE-RT-019).
type TurnCheckpoint struct {
	TaskID          string    `json:"task_id,omitempty"`
	StageID         string    `json:"stage_id,omitempty"`
	Visit           int       `json:"visit,omitempty"`
	Attempt         int       `json:"attempt,omitempty"`
	ProviderSession string    `json:"provider_session"`
	WorktreeTree    string    `json:"worktree_tree"`
	AssistantTurns  int       `json:"assistant_turns,omitempty"`
	UpdatedAt       time.Time `json:"updated_at,omitempty"`
}

// SaveCheckpoint records a durable turn checkpoint on the job for stage.
func (j *Job) SaveCheckpoint(stage string, cp TurnCheckpoint) {
	if j == nil || stage == "" || cp.ProviderSession == "" {
		return
	}
	if j.TurnCheckpoints == nil {
		j.TurnCheckpoints = map[string]TurnCheckpoint{}
	}
	cp.TaskID = j.TaskID
	cp.StageID = stage
	if cp.Visit == 0 {
		cp.Visit = j.Counts[stage]
	}
	if cp.Attempt == 0 {
		cp.Attempt = j.stagedAttempt()
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = time.Now().UTC()
	}
	j.TurnCheckpoints[stage] = cp
}

// CheckpointForStage returns the stored turn checkpoint for stage, if any.
func (j Job) CheckpointForStage(stage string) (TurnCheckpoint, bool) {
	if j.Fan != nil && j.Fan.Baseline != nil && j.InFlight != nil && j.InFlight.Stage == stage {
		if owner, ok := BranchJob(j, j.InFlight.BranchID); ok && owner.Fan == nil {
			return owner.CheckpointForStage(stage)
		}
	}
	if j.TurnCheckpoints == nil {
		return TurnCheckpoint{}, false
	}
	cp, ok := j.TurnCheckpoints[stage]
	return cp, ok && cp.ProviderSession != ""
}

// ID returns the job's durable task identifier.
func (j Job) ID() string {
	return j.TaskID
}

// Stage returns the job's current stage name.
func (j Job) Stage() string {
	return string(j.State)
}

// Children returns the active multi-child SubWorkflow references when a parent
// SubWorkflow stage fanned out to multiple child tasks.
func (j Job) Children() []ChildRef {
	if j.Child == nil || len(j.Child.Children) == 0 {
		return nil
	}
	out := make([]ChildRef, len(j.Child.Children))
	copy(out, j.Child.Children)
	return out
}

// SetChildren records multi-child SubWorkflow references on the job and pins
// the first child as j.Child for primary 1-to-1 compatibility.
func (j *Job) SetChildren(children []ChildRef) {
	if j == nil {
		return
	}
	if len(children) == 0 {
		if j.Child != nil {
			j.Child.Children = nil
		}
		return
	}
	cleaned := make([]ChildRef, len(children))
	for i, c := range children {
		c.Children = nil
		cleaned[i] = c
	}
	primary := cleaned[0]
	if len(cleaned) > 1 {
		primary.Children = cleaned
	}
	j.Child = &primary
}

// BindChildren binds all completed child tasks of a multi-child SubWorkflow
// visit into the waiting parent job and advances the parent to its next stage.
func (j *Job) BindChildren(children []*Job) error {
	return BindChildren(j, children)
}

// FailChild transitions a waiting parent SubWorkflow visit along its failure
// route (or escalates) when a child task fails, is cancelled, or cannot be bound.
func (j *Job) FailChild(childID, reason string) ([]Event, error) {
	detail := reason
	if childID != "" && detail == "" {
		detail = fmt.Sprintf("child %s failed", childID)
	}
	next, events, err := ChildFailed(*j, detail)
	if err != nil {
		return nil, err
	}
	*j = next
	return events, nil
}
