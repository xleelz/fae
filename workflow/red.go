package workflow

// validateRedPaths explores node × safety facts. Loop vertices do not erase
// obligations; a rework path must pass the same RED/frozen checks again.
func validateRedPaths(d Document, edges map[string][]string) []Issue {
	issues := []Issue{}
	type state struct {
		node                                        string
		authored, redRun, redGate, frozen, compared bool
		run, comparison                             string
	}
	seen := map[state]bool{}
	q := []state{{node: d.Start}}
	for len(q) > 0 {
		x := q[0]
		q = q[1:]
		if seen[x] {
			continue
		}
		seen[x] = true
		s := d.Stages[x.node]
		if s.Phase == "test-first" {
			x.authored = true
			x.redRun = false
			x.redGate = false
			x.frozen = false
			x.compared = false
			x.comparison = ""
		}
		if s.Primitive == HostCommand && s.TestRun && x.authored && !x.frozen {
			x.redRun = true
			x.run = x.node
		}
		if s.Primitive == Gate && x.redRun && s.Expr != nil && redGate(*s.Expr, x.run) {
			x.redGate = true
		}
		if s.Primitive == FreezeArtifact {
			if !x.redGate {
				issues = append(issues, Issue{"stages." + x.node, "freeze requires a passing declared RED gate"})
			}
			x.frozen = x.redGate
		}
		if s.Phase == "implement" {
			if !x.frozen {
				issues = append(issues, Issue{"stages." + x.node, "implementation path lacks frozen RED"})
			}
			x.compared = false
			x.comparison = ""
		}
		if s.Primitive == CompareFrozen && x.frozen {
			x.comparison = x.node
		}
		if s.Primitive == Gate && s.Expr != nil && x.comparison != "" && frozenGate(*s.Expr, x.comparison) {
			x.compared = true
		}
		if (s.Primitive == VcsCommit || s.Primitive == Integrate) && (!x.frozen || !x.compared) {
			issues = append(issues, Issue{"stages." + x.node, "commit path lacks frozen RED comparison"})
		}
		for _, to := range edges[x.node] {
			n := x
			n.node = to
			if s.Primitive == Gate && to == s.Failure {
				n.redGate = false
				n.compared = false
			}
			q = append(q, n)
		}
	}
	return issues
}
func frozenGate(x Expr, stage string) bool {
	return conjunct(x, func(e Expr) bool {
		if e.Op != "eq" || len(e.Args) != 2 {
			return false
		}
		a, b := e.Args[0], e.Args[1]
		return a.Op == "field" && a.Stage == stage && !a.All && len(a.Path) == 1 && a.Path[0] == "unchanged" && b.Op == "literal" && b.Value == true
	})
}
func redGate(x Expr, stage string) bool {
	check := func(op, path string, v any) bool {
		return conjunct(x, func(e Expr) bool {
			if e.Op != op || len(e.Args) != 2 {
				return false
			}
			a, b := e.Args[0], e.Args[1]
			if a.Op != "field" || a.Stage != stage || a.All || len(a.Path) != 1 || a.Path[0] != path || b.Op != "literal" {
				return false
			}
			same, ok := equal(b.Value, v)
			return ok && same
		})
	}
	return check("ne", "exit_code", 0) && check("ne", "exit_code", 127) && check("eq", "matched_count", 1) && check("eq", "executed_count", 1) && check("eq", "expected_failure", true) && check("eq", "test_in_source", true)
}
