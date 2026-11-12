package workflow

import (
	"fmt"
	"slices"
)

// TurnBudgetSpec bounds one agentic turn (FAE-RT-015) by wall-clock duration
// and assistant turn count, and names its exhaustion policy ("resume-once" or
// a target stage ID in the workflow).
type TurnBudgetSpec struct {
	WallSeconds       int    `json:"wall_seconds,omitempty"`
	MaxAssistantTurns int    `json:"max_assistant_turns,omitempty"`
	OnExhaustion      string `json:"on_exhaustion,omitempty"`
}

// ValidToolCapabilities is the FAE-RT-015 capability vocabulary for role and
// stage tool profiles.
var ValidToolCapabilities = []string{
	"handoff",
	"read",
	"list",
	"search_glob",
	"search_regex",
	"edit",
	"write",
	"shell",
}

// EffectiveTurnBudget retains the role's bounds unless the stage tightens
// them. The stage's exhaustion route takes precedence over the role's.
func EffectiveTurnBudget(r Role, s Stage) *TurnBudgetSpec {
	if r.TurnBudget == nil && s.TurnBudget == nil {
		return nil
	}
	b := TurnBudgetSpec{}
	if r.TurnBudget != nil {
		b = *r.TurnBudget
	}
	if s.TurnBudget != nil {
		x := s.TurnBudget
		if x.WallSeconds > 0 && (b.WallSeconds == 0 || x.WallSeconds < b.WallSeconds) {
			b.WallSeconds = x.WallSeconds
		}
		if x.MaxAssistantTurns > 0 && (b.MaxAssistantTurns == 0 || x.MaxAssistantTurns < b.MaxAssistantTurns) {
			b.MaxAssistantTurns = x.MaxAssistantTurns
		}
		if x.OnExhaustion != "" {
			b.OnExhaustion = x.OnExhaustion
		}
	}
	return &b
}

// ValidToolCapability reports whether c is a recognized role tool capability.
func ValidToolCapability(c string) bool {
	return slices.Contains(ValidToolCapabilities, c)
}

// TurnBudgetExhaustionTarget returns the workflow stage ID that stage from
// routes to when its role's turn_budget is exhausted, or "" when no stage ID
// is configured.
func TurnBudgetExhaustionTarget(d Document, from string) string {
	s, ok := d.Stages[from]
	if !ok || (s.Primitive != AgentTurn && s.Primitive != Review) {
		return ""
	}
	r, ok := d.Roles[s.Role]
	if !ok {
		return ""
	}
	b := EffectiveTurnBudget(r, s)
	if b == nil {
		return ""
	}
	target := b.OnExhaustion
	if target == "" || target == "resume-once" || target == "fail" {
		return ""
	}
	if _, exists := d.Stages[target]; !exists {
		return ""
	}
	return target
}

// TurnBudgetExhaustionEdge reports whether from -> to is the stage's
// turn-budget exhaustion route (FAE-RT-015) and not its sole outgoing edge.
func TurnBudgetExhaustionEdge(d Document, from, to string) bool {
	target := TurnBudgetExhaustionTarget(d, from)
	if target == "" || target != to {
		return false
	}
	// If from has another outgoing edge, target is an exhaustion-only route;
	// if target is the only explicit edge, let it only count as exhaustion
	// when another outgoing edge exists so normal success routing still works
	// if the same stage is also the success target.
	other := false
	for _, e := range d.Edges {
		if e.From == from && e.To != to {
			other = true
			break
		}
	}
	return other
}

func validateAgentic(d Document, outgoing map[string][]string, bad func(string, string)) {
	for name, r := range d.Roles {
		p := "roles." + name
		if r.Session != "" && r.Session != "agentic" {
			bad(p+".session", fmt.Sprintf("session must be empty (tool-less) or \"agentic\", not %q", r.Session))
		}
		if r.TurnBudget != nil {
			if r.TurnBudget.WallSeconds < 0 {
				bad(p+".turn_budget.wall_seconds", "must be non-negative")
			}
			if r.TurnBudget.MaxAssistantTurns < 0 {
				bad(p+".turn_budget.max_assistant_turns", "must be non-negative")
			}
			if ex := r.TurnBudget.OnExhaustion; ex != "" && ex != "resume-once" && ex != "fail" {
				if _, ok := d.Stages[ex]; !ok {
					bad(p+".turn_budget.on_exhaustion", fmt.Sprintf("must be \"resume-once\" or a known stage, not %q", ex))
				}
			}
		}
	}
	for id, s := range d.Stages {
		p := "stages." + id
		if b := s.TurnBudget; b != nil {
			if s.Primitive != AgentTurn {
				bad(p+".turn_budget", "belongs to AgentTurn")
			}
			r := d.Roles[s.Role]
			if b.WallSeconds < 0 || r.TurnBudget != nil && r.TurnBudget.WallSeconds > 0 && b.WallSeconds > r.TurnBudget.WallSeconds {
				bad(p+".turn_budget.wall_seconds", "must only tighten the role bound")
			}
			if b.MaxAssistantTurns < 0 || r.TurnBudget != nil && r.TurnBudget.MaxAssistantTurns > 0 && b.MaxAssistantTurns > r.TurnBudget.MaxAssistantTurns {
				bad(p+".turn_budget.max_assistant_turns", "must only tighten the role bound")
			}
			if ex := b.OnExhaustion; ex != "" && ex != "fail" && ex != "resume-once" {
				if _, ok := d.Stages[ex]; !ok {
					bad(p+".turn_budget.on_exhaustion", "must be fail, resume-once or a known stage")
				}
			}
		}
		if len(s.ReadOnlyFiles) > 0 {
			if s.Primitive != AgentTurn && s.Primitive != Review {
				bad(p+".read_only_files", "read_only_files belongs to AgentTurn and Review stages")
			}
			seen := map[string]bool{}
			for i, path := range s.ReadOnlyFiles {
				if !DeclaredPathSafe(path) || seen[path] {
					bad(fmt.Sprintf("%s.read_only_files[%d]", p, i), fmt.Sprintf("unique repo-relative path required, not %q", path))
				}
				seen[path] = true
			}
		}
		if (s.Primitive == AgentTurn || s.Primitive == Review) && !s.ChangeProducer && len(s.Writes) == 0 {
			for _, roleName := range stageRoles(s) {
				r, ok := d.Roles[roleName]
				if !ok || len(r.Writes) > 0 || len(r.Scratch) > 0 || len(s.Scratch) > 0 || r.Staging == "write" || s.Staging == "write" {
					continue
				}
				eff := EffectiveProfile(r.Profile, s)
				for _, writeCap := range []string{"edit", "write"} {
					if slices.Contains(eff.Allowed, writeCap) && !slices.Contains(eff.Denied, writeCap) {
						bad(p+".profile.allowed", fmt.Sprintf("capability %s is permitted only on ChangeProducer stages (role %s)", writeCap, roleName))
					}
				}
			}
		}
		if target := TurnBudgetExhaustionTarget(d, id); target != "" && !slices.Contains(outgoing[id], target) {
			outgoing[id] = append(outgoing[id], target)
		}
	}
}
