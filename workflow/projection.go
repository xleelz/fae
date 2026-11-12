package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Projection selects typed data without evaluating code or interpolating a
// command. Object projections and where predicates have the gate evaluator's
// depth and work bounds when evaluated by the engine.
type ProjectionLookup struct {
	Table map[string]json.RawMessage `json:"table"`
	Key   *Projection                `json:"key"`
}
type Projection struct {
	Index   *int                  `json:"index,omitempty"`
	Then    string                `json:"then,omitempty"`
	Default json.RawMessage       `json:"default,omitempty"`
	Literal json.RawMessage       `json:"literal,omitempty"`
	Lookup  *ProjectionLookup     `json:"lookup,omitempty"`
	Stage   string                `json:"stage,omitempty"`
	Type    string                `json:"type,omitempty"`
	Input   string                `json:"input,omitempty"`
	Source  string                `json:"source,omitempty"`
	Field   string                `json:"field,omitempty"`
	Object  map[string]Projection `json:"object,omitempty"`
	Where   *Expr                 `json:"where,omitempty"`
}

func (p *Projection) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var path string
		if err := json.Unmarshal(b, &path); err != nil {
			return err
		}
		parts := strings.Split(path, ".")
		if len(parts) < 1 || parts[0] == "" {
			return fmt.Errorf("empty projection")
		}
		switch parts[0] {
		case "input":
			if len(parts) < 2 {
				return fmt.Errorf("input projection requires name")
			}
			p.Input = parts[1]
			p.Field = strings.Join(parts[2:], ".")
		case "item", "tree", "failure", "change_requests":
			p.Source = parts[0]
			p.Field = strings.Join(parts[1:], ".")
		case "artifact":
			if len(parts) < 2 {
				return fmt.Errorf("artifact projection requires type")
			}
			p.Type = parts[1]
			p.Field = strings.Join(parts[2:], ".")
		default:
			p.Stage = parts[0]
			p.Field = strings.Join(parts[1:], ".")
		}
		return nil
	}
	type plain Projection
	var v plain
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return err
	}
	*p = Projection(v)
	return nil
}

// FailureHandlerOnly proves that consumer is entered only through this
// failure edge. Removing that edge must disconnect it from normal entry.
func FailureTarget(d Document, stage string) string {
	for _, target := range FailureTargets(d, stage) {
		return target
	}
	return ""
}

func FailureTargets(d Document, stage string) []string {
	s := d.Stages[stage]
	targets := []string{}
	if s.Failure != "" {
		targets = append(targets, s.Failure)
	}
	if target := TurnBudgetExhaustionTarget(d, stage); target != "" && target != s.Failure {
		targets = append(targets, target)
	}
	if target := BranchFailureJoin(d, stage); target != "" {
		found := false
		for _, existing := range targets {
			if existing == target {
				found = true
				break
			}
		}
		if !found {
			targets = append(targets, target)
		}
	}
	return targets
}

func FailureHandlerOnly(d Document, failed, consumer string) bool {
	for _, target := range FailureTargets(d, failed) {
		if failureHandlerOnlyTarget(d, failed, consumer, target) {
			return true
		}
	}
	return false
}

func failureHandlerOnlyTarget(d Document, failed, consumer, target string) bool {
	_, ok := d.Stages[failed]
	if !ok || target == "" {
		return false
	}
	reachable := func(start string, omit bool) bool {
		seen := map[string]bool{}
		q := []string{start}
		for len(q) > 0 {
			n := q[0]
			q = q[1:]
			if n == consumer {
				return true
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			for _, e := range d.Edges {
				if e.From == n && !(omit && e.From == failed && e.To == target) {
					q = append(q, e.To)
				}
			}
		}
		return false
	}
	return reachable(target, false) && !reachable(d.Start, true)
}
