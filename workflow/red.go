package workflow

// validateRedPaths explores node × safety facts. Loop vertices do not erase
// obligations; a rework path must pass the same RED/frozen checks again. Every
// change-producing stage other than test-first is an implementation: it needs
// the frozen RED and voids any earlier comparison or passing RED run.
func validateRedPaths(d Document, edges map[string][]string) []Issue {
	issues := []Issue{}
	for id, s := range d.Stages {
		if ProducesChanges(d,s) && s.Phase == "" {
			issues = append(issues, Issue{"stages." + id + ".phase", "code-bearing change producer must declare test-first or implement phase"})
		}
	}
	type state struct {
		node                                                string
		authored, redRun, redGate, frozen, compared, passed bool
		run, comparison, green                              string
		// noRed marks a path only a no-red declaration can take; the
		// commit-time diff classifier replaces the RED obligations on it.
		noRed bool
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
			x.passed = false
			x.green = ""
		}
		// Only a run of the declared test shape can establish the RED.
		if s.Primitive == HostCommand && s.TestRun && s.TestShape != "" && x.authored && !x.frozen {
			x.redRun = true
			x.run = x.node
		}
		if s.Primitive == HostCommand && s.TestRun && x.frozen {
			x.green = x.node
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
		if s.Phase == "implement" || ProducesChanges(d,s) && s.Phase != "test-first" {
			if !x.frozen && !x.noRed {
				issues = append(issues, Issue{"stages." + x.node, "implementation path lacks frozen RED"})
			}
			x.compared = false
			x.comparison = ""
			x.passed = false
			x.green = ""
		}
		if s.Primitive == CompareFrozen && x.frozen {
			x.comparison = x.node
		}
		if s.Primitive == Gate && s.Expr != nil && x.comparison != "" && frozenGate(*s.Expr, x.comparison) {
			x.compared = true
		}
		if s.Primitive == Gate && s.Expr != nil && x.green != "" && redPassedGate(*s.Expr, x.green) {
			x.passed = true
		}
		if (s.Primitive == VcsCommit || s.Primitive == Integrate) && (!x.frozen || !x.compared) && !x.noRed {
			issues = append(issues, Issue{"stages." + x.node, "commit path lacks frozen RED comparison"})
		}
		if (s.Primitive == VcsCommit || s.Primitive == Integrate) && !x.passed && !x.noRed {
			issues = append(issues, Issue{"stages." + x.node, "commit path lacks a passing run of the frozen RED test"})
		}
		for _, to := range edges[x.node] {
			n := x
			n.node = to
			if s.Primitive == Gate && to == s.Failure {
				n.redGate = false
				n.compared = false
				n.passed = false
			}
			// A Branch successor reached only by no-red arms waives the RED floor.
			plain, declared := s.Primitive != Branch || to == s.Default, false
			for _, arm := range s.Cases {
				if arm.Next == to {
					declared = declared || noRedArm(d, arm.When)
					plain = plain || !noRedArm(d, arm.When)
				}
			}
			plain = plain || !declared
			if plain {
				q = append(q, n)
			}
			if declared {
				n.noRed = true
				q = append(q, n)
			}
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

// redPassedGate asserts the post-implementation run passed, executed tests and
// reported the declared RED test itself as passed (FAE-WF-073).
func redPassedGate(x Expr, stage string) bool {
	return testGate(x, stage) && conjunct(x, func(e Expr) bool {
		if e.Op != "eq" || len(e.Args) != 2 {
			return false
		}
		a, b := e.Args[0], e.Args[1]
		return a.Op == "field" && a.Stage == stage && !a.All && len(a.Path) == 1 && a.Path[0] == "red_passed" && b.Op == "literal" && b.Value == true
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
