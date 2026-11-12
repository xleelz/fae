package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// ParkHumanDecision is the park reason of a task waiting at a HumanDecision
// stage. It raises the HumanDecisionRequired kind without leaving the stage:
// the declared options route out of it, which Escalated could not (FAE-WF-021).
const ParkHumanDecision = string(domain.HumanDecisionRequired)

// DecisionRequest is the undisposed operator decision of a parked task.
type DecisionRequest struct {
	Branch      string                    `json:"branch,omitempty"`
	Items       []any                     `json:"items,omitempty"`
	ItemOptions []workflow.DecisionOption `json:"item_options,omitempty"`
	ID          string                    `json:"id"`
	Stage       string                    `json:"stage"`
	Visit       int                       `json:"visit"`
	Question    string                    `json:"question"`
	Options     []workflow.DecisionOption `json:"options"`
}

// Decision is an operator's answer; Message is the authorizing message it cites.
type Decision struct {
	ID, Branch                        string
	Items                             []workflow.ItemDecision
	Option, Message, Actor, Rationale string
	At                                time.Time
}

// DecisionRecord is the durable decision row the caller writes in the same
// transaction as the transition it caused (FAE-WF-022).
type DecisionRecord struct {
	Branch       string           `json:"branch,omitempty"`
	Items        []map[string]any `json:"items,omitempty"`
	ID           string           `json:"id"`
	TaskID       string           `json:"task_id"`
	Stage        string           `json:"stage"`
	Visit        int              `json:"visit"`
	Question     string           `json:"question"`
	Option       string           `json:"option"`
	Next         string           `json:"next"`
	Message      string           `json:"authorizing_message"`
	Actor        string           `json:"decided_by"`
	Rationale    string           `json:"rationale,omitempty"`
	ArtifactID   string           `json:"artifact_id"`
	WorkflowHash string           `json:"workflow_hash"`
	Revision     int              `json:"workflow_revision"`
	At           time.Time        `json:"at"`
}

// decisionID names one visit of a decision stage on the pinned revision.
func decisionID(j Job, stage string, visit int) string {
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%d", j.TaskID, j.WorkflowHash, stage, visit)
	if j.BranchContext != nil {
		identity += "\x00" + j.BranchContext.Path + fanVisitSuffix(j.BranchContext)
	}
	sum := sha256.Sum256([]byte(identity))
	return "decision-" + hex.EncodeToString(sum[:16])
}

// awaitDecision parks a task arriving at a HumanDecision stage. It stays
// waiting until an operator decides; nothing ever decides automatically.
func awaitDecision(j Job, events []Event) (Job, *Dispatch, []Event, error) {
	stage := string(j.State)
	if s := j.Document.Stages[stage]; s.ItemsFrom != nil {
		items, err := decisionItems(j, s)
		if err != nil {
			stop(&j, domain.HumanDecisionRequired, "DecisionItems: "+err.Error(), &events)
			return j, nil, events, nil
		}
		j.DecisionItems = items
	}
	j.Counts[stage]++
	j.Parked = true
	j.Reason = ParkHumanDecision
	event(&j, Event{Kind: "park", Stage: stage, Attempt: j.stagedAttempt(), Cause: ParkHumanDecision, DispatchID: decisionID(j, stage, j.Counts[stage]), EscalationKind: domain.HumanDecisionRequired}, &events)
	return j, nil, events, nil
}

// PendingDecision reports the decision a task is waiting for, if any.
func pendingDecisionLocal(j Job) (DecisionRequest, bool) {
	stage := string(j.State)
	s, ok := j.Document.Stages[stage]
	if !ok || s.Primitive != workflow.HumanDecision || !j.Parked || j.Reason != ParkHumanDecision || j.InFlight != nil || j.OperatorStage != "" || j.Counts[stage] < 1 {
		return DecisionRequest{}, false
	}
	branch := ""
	if j.BranchContext != nil {
		branch = j.BranchContext.Path
	}
	return DecisionRequest{Branch: branch, ID: decisionID(j, stage, j.Counts[stage]), Stage: stage, Visit: j.Counts[stage], Question: s.Question, Options: s.Options, Items: j.DecisionItems, ItemOptions: s.ItemOptions}, true
}

// Decide applies an operator decision to the pending HumanDecision: it records
// the decision artifact citing the authorizing message, with operator
// provenance, and moves the task along the chosen option's route. A task that
// is not waiting (already decided, escalated, blocked, cancelled or terminal)
// is refused, so a decision is never applied twice.
func decideLocal(input Job, d Decision) (Job, []Event, DecisionRecord, error) {
	j := clone(input)
	events := []Event{}
	fail := func(format string, args ...any) (Job, []Event, DecisionRecord, error) {
		return input, nil, DecisionRecord{}, fmt.Errorf("decision refused: "+format, args...)
	}
	if err := Validate(j); err != nil {
		return input, nil, DecisionRecord{}, err
	}
	req, ok := pendingDecisionLocal(j)
	if !ok {
		return fail("task %s is not waiting at a HumanDecision stage", j.TaskID)
	}
	s := j.Document.Stages[req.Stage]
	o, ok := s.Option(d.Option)
	if len(s.Options) == 0 && s.ItemsFrom != nil && d.Option == "" {
		next, err := route(j, s)
		if err == nil {
			o = workflow.DecisionOption{ID: "items", Next: next}
			ok = true
		}
	}
	var itemRecords []map[string]any
	if s.ItemsFrom != nil {
		var err error
		itemRecords, err = bindItemDecisions(j.DecisionItems, s.ItemOptions, d.Items)
		if err != nil {
			return fail("%s", err)
		}
	} else if len(d.Items) > 0 {
		return fail("stage declares no item decisions")
	}

	if !ok {
		declared := []string{}
		for _, o := range s.Options {
			declared = append(declared, o.ID)
		}
		return fail("undeclared option %q; declared options are %s", d.Option, strings.Join(declared, ", "))
	}
	if strings.TrimSpace(d.Message) == "" {
		return fail("a decision must cite the authorizing operator message")
	}
	if len(d.Rationale) > 4096 {
		return fail("rationale exceeds 4096 bytes")
	}
	if d.Actor == "" {
		d.Actor = "operator"
	}
	if d.At.IsZero() {
		d.At = time.Now().UTC()
	}
	fields := workflow.DecisionFields(req.ID, s.Question, o.ID, d.Message, d.Actor, d.Rationale)
	if s.ItemsFrom != nil {
		fields["items"] = itemRecords
	}
	raw, _ := json.Marshal(fields)
	typ := j.Document.Artifacts[s.Output]
	if err := workflow.ValidateArtifact(typ.Schema, raw); err != nil {
		return fail("decision artifact does not satisfy %s: %v", s.Output, err)
	}
	id, err := domain.ContentHash(raw)
	if err != nil {
		return input, nil, DecisionRecord{}, err
	}
	p := Provenance{ExecutionKind: j.ExecutionKind, ArtifactID: id, DispatchID: req.ID, TaskID: j.TaskID, Stage: req.Stage, Primitive: workflow.HumanDecision, Role: "operator", Session: req.ID, Tree: j.Tree, ProducedAt: d.At, WorkflowHash: j.WorkflowHash, WorkflowRevision: j.Revision}
	if j.BranchContext != nil {
		p.BranchID = j.BranchContext.Path
	}
	for _, in := range boundInputs(j, s) {
		p.Inputs = append(p.Inputs, in.ID)
	}
	j.Evidence = append(j.Evidence, workflow.Evidence{ID: id, TaskID: j.TaskID, Type: s.Output, Version: typ.Version, Stage: req.Stage, Tree: j.Tree, Role: "operator", Session: req.ID, Provenance: true, Fields: fields})
	j.Provenance = append(j.Provenance, p)
	j.Parked = false
	j.Reason = ""
	event(&j, Event{Kind: "decision", Stage: req.Stage, DispatchID: req.ID, Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("option %s by %s citing message %s", o.ID, d.Actor, d.Message)}, &events)
	// "completed <id>" lets history pages reference the decision artifact.
	if err := move(&j, o.Next, "completed "+req.ID, &events); err != nil {
		return input, nil, DecisionRecord{}, err
	}
	if err := Validate(j); err != nil {
		return input, nil, DecisionRecord{}, errors.Join(errors.New("decision refused"), err)
	}
	return j, events, DecisionRecord{Branch: req.Branch, Items: itemRecords, ID: req.ID, TaskID: j.TaskID, Stage: req.Stage, Visit: req.Visit, Question: s.Question, Option: o.ID, Next: o.Next, Message: d.Message, Actor: d.Actor, Rationale: d.Rationale, ArtifactID: id, WorkflowHash: j.WorkflowHash, Revision: j.Revision, At: d.At}, nil
}
