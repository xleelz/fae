package workflow

import (
	"fmt"
	"github.com/xleelz/fae/domain"
)

// ScriptStep describes a recorded result, never a command to execute.
// Next selects one declared graph edge; gate routing is interpreted by the
// simulator and cannot be overridden by a script.
type ScriptStep struct {
	Next     string     `json:"next,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
}
type Script map[string][]ScriptStep
type Transition struct {
	From domain.State `json:"from"`
	To   domain.State `json:"to"`
}
type Simulation struct {
	State       domain.State `json:"state"`
	Path        []string     `json:"path"`
	Transitions []Transition `json:"transitions"`
	Evidence    []Evidence   `json:"evidence"`
	Gates       []GateResult `json:"gates"`
	Reason      string       `json:"reason,omitempty"`
}

// Simulate is intentionally an offline replay interpreter: it performs no I/O,
// trusts only supplied provenance-validated records, and refuses unsupported
// runtime primitives. Attempts and loop caps guarantee finite execution.
func Simulate(d Document, task string, script Script) (Simulation, error) {
	result := Simulation{State: domain.Pending}
	if issues := Validate(d); len(issues) > 0 {
		return result, fmt.Errorf("invalid workflow: %v", issues)
	}
	counts := map[string]int{}
	attempts := 0
	move := func(to domain.State) error {
		if !CanTransition(d, result.State, to) {
			return fmt.Errorf("illegal transition %s -> %s", result.State, to)
		}
		result.Transitions = append(result.Transitions, Transition{result.State, to})
		result.State = to
		result.Path = append(result.Path, string(to))
		return nil
	}
	if err := move(domain.State(d.Start)); err != nil {
		return result, err
	}
	for !result.State.Terminal() && result.State != domain.Escalated {
		id := string(result.State)
		stage, ok := d.Stages[id]
		if !ok {
			return result, fmt.Errorf("no executor for %s", id)
		}
		if missing := CheckInputs(stage, task, result.Evidence); len(missing) > 0 {
			result.Reason = fmt.Sprintf("MissingInput: %v", missing)
			return result, nil
		}
		if attempts >= d.Limits.Attempts {
			result.Reason = "AttemptCap"
			_ = move(domain.Escalated)
			break
		}
		attempts++
		if stage.Primitive == Loop && counts[id] >= stage.MaxIterations {
			result.Reason = "LoopCap"
			_ = move(domain.Escalated)
			break
		}
		records := script[id]
		index := counts[id]
		counts[id]++
		next := ""
		if stage.Primitive == Escalate {
			result.Reason = "HumanDecisionRequired"
			_ = move(domain.Escalated)
			break
		}
		if stage.Primitive == ExternalEffect || stage.Primitive == VcsCommit || stage.Primitive == Integrate {
			return result, fmt.Errorf("commit, integration and external effects require runtime evidence enforcement and cannot execute in offline simulator")
		}
		if stage.Primitive == Gate {
			inputs := []Evidence{}
			for _, a := range result.Evidence {
				if a.TaskID == task && a.Provenance {
					inputs = append(inputs, a)
				}
			}
			gate := Eval(*stage.Expr, inputs)
			result.Gates = append(result.Gates, gate)
			if !gate.Passed {
				next = stage.Failure
			} else {
				for _, edge := range d.Edges {
					if edge.From == id && edge.To != stage.Failure {
						if next != "" {
							return result, fmt.Errorf("ambiguous gate success route")
						}
						next = edge.To
					}
				}
			}
		} else {
			if index >= len(records) {
				return result, fmt.Errorf("missing script result for %s visit %d", id, index+1)
			}
			record := records[index]
			next = record.Next
			for _, a := range record.Evidence {
				if a.ID == "" || a.TaskID != task || a.Stage != id || !a.Provenance {
					return result, fmt.Errorf("invalid scripted evidence provenance for %s", id)
				}
				result.Evidence = append(result.Evidence, a)
			}
			if next == "" {
				for _, edge := range d.Edges {
					if edge.From == id {
						if next != "" {
							return result, fmt.Errorf("script must select route for %s", id)
						}
						next = edge.To
					}
				}
			}
		}
		if err := move(domain.State(next)); err != nil {
			return result, err
		}
	}
	return result, nil
}

// CheckProtectedEvidence binds the runtime safety checks to the exact tree,
// producer identities and independent review. Graph validation alone cannot
// prove facts about a future runtime session or worktree.
func CheckProtectedEvidence(tree string, runs, reviews, changes []Evidence) error {
	if tree == "" || len(changes) == 0 {
		return fmt.Errorf("missing tree or changes")
	}
	task := changes[0].TaskID
	if task == "" {
		return fmt.Errorf("missing task identity")
	}
	for _, c := range changes {
		if c.TaskID != task {
			return fmt.Errorf("cross-task changes")
		}
	}
	tested := false
	for _, r := range runs {
		exit, eok := number(r.Fields["exit_code"])
		count, cok := number(r.Fields["executed_count"])
		if r.Provenance && r.TaskID == task && r.Tree == tree && eok && cok && exit == 0 && count >= 1 {
			tested = true
		}
	}
	if !tested {
		return fmt.Errorf("no passing positive test run for tree")
	}
	for _, r := range reviews {
		if !r.Provenance || r.TaskID != task || r.Tree != tree || r.Role == "" || r.Session == "" || r.Fields["approved"] != true {
			continue
		}
		independent := true
		for _, c := range changes {
			if !c.Provenance || c.Role == "" || c.Session == "" || c.Role == r.Role || c.Session == r.Session {
				independent = false
			}
		}
		if independent {
			return nil
		}
	}
	return fmt.Errorf("no independent approved review for tree")
}
