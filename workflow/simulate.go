package workflow

import (
	"fmt"

	"github.com/xleelz/fae/domain"
)

// ScriptStep describes a recorded result, never a command to execute. Only
// dispatching stages consume steps; gates, branches and loops are evaluated.
// Next may only select a Merge or Integrate stage's reverify route, modelling
// a changed integration base or a moved target.
type ScriptStep struct {
	Next     string     `json:"next,omitempty"`
	Evidence []Evidence `json:"evidence,omitempty"`
	// Decision is the option an operator takes at a HumanDecision stage;
	// a visit without one leaves the simulation awaiting the decision.
	Decision string         `json:"decision,omitempty"`
	Items    []ItemDecision `json:"items,omitempty"`
}
type Script map[string][]ScriptStep
type Transition struct {
	From domain.State `json:"from"`
	To   domain.State `json:"to"`
}

// SimulationActivity is one virtual session, measured from simulation start.
type SimulationActivity struct {
	Dispatch  string  `json:"dispatch"`
	Task      string  `json:"task"`
	Stage     string  `json:"stage"`
	Branch    string  `json:"branch,omitempty"`
	Role      string  `json:"role"`
	StartMS   float64 `json:"start_ms"`
	FinishMS  float64 `json:"finish_ms"`
	Cancelled bool    `json:"cancelled,omitempty"`
}
type Simulation struct {
	Timeline     []SimulationActivity `json:"timeline,omitempty"`
	DurationMS   float64              `json:"duration_ms,omitempty"`
	PeakSessions int                  `json:"peak_sessions,omitempty"`
	Footprint    *Footprint           `json:"footprint,omitempty"`
	Tree         []SimulationTask     `json:"tree,omitempty"`
	Refusals     []SimulationRefusal  `json:"refusals,omitempty"`
	State        domain.State         `json:"state"`
	Path         []string             `json:"path"`
	Transitions  []Transition         `json:"transitions"`
	Evidence     []Evidence           `json:"evidence"`
	Gates        []GateResult         `json:"gates"`
	Reason       string               `json:"reason,omitempty"`
	// Records are the content-addressed gate, branch-arm, aggregate and shape
	// evaluations, as the engine records them (FAE-WF-063).
	Records []GateRecord `json:"records,omitempty"`
}

// SimulationOptions selects the simulated disposition. Normal admits
// ExternalEffect stages (their results are scripted, never performed).
// engine.Simulate runs the durable engine itself over a Script (FAE-WF-091),
// so a simulated path is the engine's path.
type SimulationOptions struct {
	Normal bool `json:"normal,omitempty"`
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

// ItemDecision identifies exactly one member of a HumanDecision's pinned list.
type ItemDecision struct {
	Index     *int   `json:"index,omitempty"`
	ID        string `json:"id,omitempty"`
	Option    string `json:"option"`
	Rationale string `json:"rationale,omitempty"`
}
