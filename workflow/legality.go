package workflow

import "github.com/xleelz/fae/domain"

// LateStages are the change producers and every stage reachable from one;
// every other stage is pre-code.
func LateStages(d Document) map[string]bool {
	out := map[string]bool{}
	edges := map[string][]string{}
	for _, e := range d.Edges {
		edges[e.From] = append(edges[e.From], e.To)
	}
	var visit func(string)
	visit = func(n string) {
		for _, to := range edges[n] {
			if !out[to] {
				out[to] = true
				visit(to)
			}
		}
	}
	for id, s := range d.Stages {
		if ProducesChanges(d,s) {
			out[id] = true
			visit(id)
		}
	}
	return out
}

// CarryEntry reports a stage where a re-admission carrying accepted pre-code
// evidence is released (FAE-WF-086): a late stage entered from a pre-code one.
func CarryEntry(d Document, stage string) bool {
	late := LateStages(d)
	if _, ok := d.Stages[stage]; !ok || !late[stage] || d.Stages[stage].Primitive == Escalate {
		return false
	}
	for _, e := range d.Edges {
		if e.To == stage && !late[e.From] && !domain.State(e.From).Kernel() {
			return true
		}
	}
	return false
}

// remediation reports a stage that is a declared failure route: where an
// operator-Blocked task re-enters (FAE-OPS-010 Block, Appendix C).
func remediation(d Document, stage string) bool {
	if _, ok := d.Stages[stage]; !ok {
		return false
	}
	for _, s := range d.Stages {
		if s.Failure == stage {
			return true
		}
	}
	return false
}
