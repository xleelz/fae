package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xleelz/fae/workflow"
)

// ScriptStep is a rehearsal fixture, never evidence that a real command or VCS
// operation happened. Dispatch envelopes are minted by the executor, not supplied
// by a role's JSON. Visit-indexed fixtures make explicit safe replay deterministic.
type ScriptStep struct {
	Output  json.RawMessage   `json:"output"`
	Reviews []json.RawMessage `json:"reviews,omitempty"`
	Delay   time.Duration     `json:"delay,omitempty"`
	Class   string            `json:"class,omitempty"`
}
type Scripted struct{ script map[string][]ScriptStep }

func NewScripted(script map[string][]ScriptStep) *Scripted {
	b, _ := json.Marshal(script)
	s := &Scripted{}
	_ = json.Unmarshal(b, &s.script)
	return s
}
func (s *Scripted) Execute(ctx context.Context, d Dispatch) (Result, error) {
	if len(d.Sessions) == 0 || d.ID == "" || d.TaskID == "" {
		return Result{}, fmt.Errorf("incomplete dispatch")
	}
	r := Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID}
	var steps []ScriptStep
	var ok bool
	if d.BranchID != "" {
		steps, ok = s.script[d.TaskID+"/"+d.BranchID+"/"+d.Stage]
		if !ok {
			steps, ok = s.script[d.BranchID+"/"+d.Stage]
		}
	}
	if !ok {
		steps, ok = s.script[d.TaskID+"/"+d.Stage]
	}
	if !ok {
		steps = s.script[d.Stage]
	}
	if len(steps) == 0 {
		return r, fmt.Errorf("missing script for %s", d.Stage)
	}
	i := d.Visit - 1
	if i < 0 || i >= len(steps) {
		return r, fmt.Errorf("script exhausted for %s visit %d", d.Stage, d.Visit)
	}
	step := steps[i]
	timer := time.NewTimer(step.Delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		r.Class = "interrupted"
		r.ProducedAt = time.Now().UTC()
		return r, ctx.Err()
	case <-timer.C:
	}
	r.Output = append(json.RawMessage(nil), step.Output...)
	r.Class = step.Class
	r.ProducedAt = time.Now().UTC()
	if d.Config.Primitive == workflow.Review {
		for i, session := range d.Sessions {
			if i >= len(step.Reviews) {
				return r, fmt.Errorf("script lacks reviewer %d", i)
			}
			r.Reviews = append(r.Reviews, ReviewResult{Session: session.ID, Role: session.Role, Output: append(json.RawMessage(nil), step.Reviews[i]...)})
		}
	}
	// Freeze/compare compute normalized identities, even in rehearsal. Source is
	// explicit fixture artifact data; production executors must acquire tree bytes.
	if d.Config.Primitive == workflow.FreezeArtifact || d.Config.Primitive == workflow.CompareFrozen {
		var e error
		if r.Output, e = rehearsedIdentity(d); e != nil {
			return r, e
		}
	}
	return r, nil
}

// rehearsedIdentity freezes or compares the normalized identity of the bound
// subject artifact's declared test (its source and test_id fields).
func rehearsedIdentity(d Dispatch) (json.RawMessage, error) {
	var subject *workflow.Evidence
	for i := range d.Inputs {
		if d.Inputs[i].Stage == d.Config.Subject {
			subject = &d.Inputs[i]
		}
	}
	if subject == nil {
		return nil, fmt.Errorf("missing identity subject %s", d.Config.Subject)
	}
	source, _ := subject.Fields["source"].(string)
	test, _ := subject.Fields["test_id"].(string)
	identity, e := workflow.GoTestIdentity([]byte(source), []string{test})
	if e != nil {
		return nil, e
	}
	fields := map[string]any{"identity": identity}
	if d.Config.Primitive == workflow.CompareFrozen {
		frozen := ""
		for _, a := range d.Inputs {
			if a.Type == "FrozenIdentity" {
				frozen, _ = a.Fields["identity"].(string)
			}
		}
		if frozen == "" {
			return nil, fmt.Errorf("missing frozen identity")
		}
		fields["unchanged"] = frozen == identity
	}
	return json.Marshal(fields)
}
