package workflow_test

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

// ruleCase fires one validator rule: the mutation of a valid default document
// must produce an issue at path whose message contains message.
type ruleCase struct {
	path, message string
	shipped       bool
	mutate        func(*workflow.Document)
}

func stage(d *workflow.Document, id string, f func(*workflow.Stage)) {
	s := d.Stages[id]
	f(&s)
	d.Stages[id] = s
}
func role(d *workflow.Document, id string, f func(*workflow.Role)) {
	r := d.Roles[id]
	f(&r)
	d.Roles[id] = r
}
func expr(x workflow.Expr) *workflow.Expr { return &x }
func lit(v any) workflow.Expr             { return workflow.Expr{Op: "literal", Value: v} }
func eqField(stage, path string, v any) workflow.Expr {
	return workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: stage, Path: []string{path}}, lit(v)}}
}
func and(a, b workflow.Expr) workflow.Expr {
	return workflow.Expr{Op: "and", Args: []workflow.Expr{a, b}}
}

// validatorRules is keyed by the exact source text of each rule's issue
// arguments in workflow.Validate, selectors and validateRedPaths; the census
// test below fails when a rule is added without a firing case here, and
// scripts/check-ledger.py maps every key to requirements (FAE-VER-001/-004).
var validatorRules = map[string]ruleCase{
	`p+".branch_failure", "only FanOut branch_failure: fail-branch is supported"`: {"stages.implement.branch_failure", "only FanOut branch_failure: fail-branch is supported", false, func(d *workflow.Document) {
		stage(d, "implement", func(s *workflow.Stage) { s.BranchFailure = "continue" })
	}},
	`"roles."+name+".cap_scope", "must be workflow, tree or task"`: {"roles.qa.cap_scope", "must be workflow, tree or task", false, func(d *workflow.Document) { role(d, "qa", func(r *workflow.Role) { r.CapScope = "host" }) }},
	`"stages."+id+".run_stats", "must be tree or task"`:            {"stages.implement.run_stats", "must be tree or task", false, func(d *workflow.Document) { stage(d, "implement", func(s *workflow.Stage) { s.RunStats = "instance" }) }},
	`"schema_version", "must be 1 or 2"`:                           {"schema_version", "must be 1 or 2", false, func(d *workflow.Document) { d.SchemaVersion = 3 }},
	`"id", "required"`:                                             {"id", "required", false, func(d *workflow.Document) { d.ID = "" }},
	`"start", "unknown stage"`:                                     {"start", "unknown stage", false, func(d *workflow.Document) { d.Start = "nowhere" }},
	`"stages", "required"`:                                         {"stages", "required", false, func(d *workflow.Document) { d.Stages = map[string]workflow.Stage{} }},
	`"roles", "required"`:                                          {"roles", "required", false, func(d *workflow.Document) { d.Roles = nil }},
	`"artifacts", "required"`:                                      {"artifacts", "required", false, func(d *workflow.Document) { d.Artifacts = nil }},
	`"limits.attempts", "must be positive"`:                        {"limits.attempts", "must be positive", false, func(d *workflow.Document) { d.Limits.Attempts = 0 }},
	`"limits.test_runs", "must be positive"`:                       {"limits.test_runs", "must be positive", false, func(d *workflow.Document) { d.Limits.TestRuns = 0 }},
	`"limits.scope_widens", "must be positive"`:                    {"limits.scope_widens", "must be positive", false, func(d *workflow.Document) { d.Limits.ScopeWidens = 0 }},
	`p, "empty role name"`:                                         {"roles.", "empty role name", false, func(d *workflow.Document) { d.Roles[""] = d.Roles["qa"] }},
	`p, "instructions and briefs required"`: {"roles.qa", "instructions and briefs required", false, func(d *workflow.Document) {
		role(d, "qa", func(r *workflow.Role) { r.Instructions = "" })
	}},
	`p+".cap", "positive or -1 required"`: {"roles.qa.cap", "positive or -1 required", false, func(d *workflow.Document) { role(d, "qa", func(r *workflow.Role) { r.Cap = 0 }) }},
	`p+".scope", "unknown scope"`:         {"roles.qa.scope", "unknown scope", false, func(d *workflow.Document) { role(d, "qa", func(r *workflow.Role) { r.Scope = "global" }) }},
	`p+".routes", "required"`:             {"roles.qa.routes", "required", false, func(d *workflow.Document) { role(d, "qa", func(r *workflow.Role) { r.Routes = nil }) }},
	`fmt.Sprintf("%s.routes[%d]", p, i), "provider and model required"`: {"roles.qa.routes[0]", "provider and model required", false, func(d *workflow.Document) {
		role(d, "qa", func(r *workflow.Role) { r.Routes = append([]workflow.Binding{{Provider: "claude"}}, r.Routes[1:]...) })
	}},
	`p+".routes", "unknown effort"`: {"roles.qa.routes", "unknown effort", false, func(d *workflow.Document) {
		role(d, "qa", func(r *workflow.Role) {
			r.Routes = append([]workflow.Binding{{Provider: "claude", Model: "m", Effort: "max"}}, r.Routes[1:]...)
		})
	}},
	`fmt.Sprintf("%s.routes[%d]", p, i), "effort "+b.Effort+" is not supported by provider "+b.Provider`: {"roles.qa.routes[0]", "effort minimal is not supported by provider claude", false, func(d *workflow.Document) {
		role(d, "qa", func(r *workflow.Role) {
			r.Routes = append([]workflow.Binding{{Provider: "claude", Model: "m", Effort: "minimal"}}, r.Routes[1:]...)
		})
	}},
	`p+".outputs", "unknown artifact "+a`: {"roles.qa.outputs", "unknown artifact Ghost", false, func(d *workflow.Document) {
		role(d, "qa", func(r *workflow.Role) { r.Outputs = append(r.Outputs, "Ghost") })
	}},
	`p+".profile.allowed", "unknown capability "+c`: {"roles.qa.profile.allowed", "unknown capability network", false, func(d *workflow.Document) {
		role(d, "qa", func(r *workflow.Role) { r.Profile.Allowed = append(r.Profile.Allowed, "network") })
	}},
	`p, "brief key must be default or a declared phase of a stage using this role"`: {"roles.planner.briefs.main", "brief key must be default", false, func(d *workflow.Document) {
		role(d, "planner", func(r *workflow.Role) { r.Briefs = map[string]string{"default": r.Briefs["default"], "main": "x"} })
	}},
	`p, "empty brief template"`: {"roles.planner.briefs.default", "empty brief template", false, func(d *workflow.Document) {
		role(d, "planner", func(r *workflow.Role) { r.Briefs = map[string]string{"default": "  "} })
	}},
	`"artifacts."+name, "name, positive version and JSON schema required"`: {"artifacts.Plan", "positive version", false, func(d *workflow.Document) {
		a := d.Artifacts["Plan"]
		a.Version = 0
		d.Artifacts["Plan"] = a
	}},
	`"artifacts."+name+".schema", err.Error()`: {"artifacts.Plan.schema", "", false, func(d *workflow.Document) {
		a := d.Artifacts["Plan"]
		a.Schema = json.RawMessage(`{"type":5}`)
		d.Artifacts["Plan"] = a
	}},
	`p, "unknown endpoint"`: {"edges[", "unknown endpoint", false, func(d *workflow.Document) {
		d.Edges = append(d.Edges, workflow.Edge{From: "nowhere", To: "Done"})
	}},
	`p, "kernel edges cannot be overridden"`: {"edges[", "kernel edges cannot be overridden", false, func(d *workflow.Document) {
		d.Edges = append(d.Edges, workflow.Edge{From: "Done", To: "intake"})
	}},
	`p, "kernel destination cannot be overridden"`: {"edges[", "kernel destination cannot be overridden", false, func(d *workflow.Document) {
		d.Edges = append(d.Edges, workflow.Edge{From: "plan", To: "Escalated"})
	}},
	`p+".reverify", "must be an outgoing bounded Loop of a Merge or Integrate stage"`: {"stages.qa.reverify", "must be an outgoing bounded Loop", false, func(d *workflow.Document) {
		stage(d, "qa", func(s *workflow.Stage) { s.Reverify = "reverify-loop" })
	}},
	`p+".phase", "unknown phase"`: {"stages.plan.phase", "unknown phase", false, func(d *workflow.Document) { stage(d, "plan", func(s *workflow.Stage) { s.Phase = "draft" }) }},
	`p+".kind", "escalation kind must be an Appendix B kind on an Escalate stage"`: {"stages.stop.kind", "Appendix B kind", false, func(d *workflow.Document) {
		stage(d, "stop", func(s *workflow.Stage) { s.Kind = "Whenever" })
	}},
	`p, "reserved or empty stage name"`: {"stages.Done", "reserved or empty stage name", false, func(d *workflow.Document) {
		d.Stages["Done"] = workflow.Stage{Primitive: workflow.Escalate}
	}},
	`p+".primitive", "unknown primitive"`: {"stages.odd.primitive", "unknown primitive", false, func(d *workflow.Document) {
		d.Stages["odd"] = workflow.Stage{Primitive: "Teleport"}
	}},
	`p+".role", "unknown role"`: {"stages.plan.role", "unknown role", false, func(d *workflow.Document) { stage(d, "plan", func(s *workflow.Stage) { s.Role = "ghost" }) }},
	`p+".output", "required"`:   {"stages.plan.output", "required", false, func(d *workflow.Document) { stage(d, "plan", func(s *workflow.Stage) { s.Output = "" }) }},
	`p+".output", "output not permitted by role"`: {"stages.plan.output", "output not permitted by role", false, func(d *workflow.Document) {
		stage(d, "plan", func(s *workflow.Stage) { s.Output = "Spec" })
	}},
	`p+".role", fmt.Sprintf("role %s has no brief template for phase %s", role, phaseName(s.Phase))`: {"stages.implement.role", "role coder has no brief template for phase implement", false, func(d *workflow.Document) {
		role(d, "coder", func(r *workflow.Role) { r.Briefs = map[string]string{"test-first": r.Briefs["test-first"]} })
	}},
	`p+".test_shape", "declared test shape requires a test-run HostCommand of a supported form ("+strings.Join(TestShapes, ", ")+") matching its command"`: {"stages.run-red.test_shape", "declared test shape requires", false, func(d *workflow.Document) {
		stage(d, "run-red", func(s *workflow.Stage) { s.TestShape = "pytest" })
	}},
	`p+".output", "unknown artifact"`: {"stages.plan.output", "unknown artifact", false, func(d *workflow.Document) { stage(d, "plan", func(s *workflow.Stage) { s.Output = "Ghost" }) }},
	`ip, "unknown artifact, wrong version, or missing name"`: {"stages.plan.inputs[0]", "wrong version", false, func(d *workflow.Document) {
		stage(d, "plan", func(s *workflow.Stage) {
			s.Inputs = append([]workflow.Input(nil), s.Inputs...)
			s.Inputs[0].Version = 9
		})
	}},
	`ip, "unknown producer stage"`: {"stages.plan.inputs[0]", "unknown producer stage", false, func(d *workflow.Document) {
		stage(d, "plan", func(s *workflow.Stage) {
			s.Inputs = append([]workflow.Input(nil), s.Inputs...)
			s.Inputs[0].Stage = "nowhere"
		})
	}},
	`p+".max_iterations", "finite positive cap required"`: {"stages.spec-loop.max_iterations", "finite positive cap", false, func(d *workflow.Document) {
		stage(d, "spec-loop", func(s *workflow.Stage) { s.MaxIterations = 0 })
	}},
	`p+".default", "default arm must be an edge"`: {"stages.verdict.default", "default arm must be an edge", false, func(d *workflow.Document) {
		stage(d, "verdict", func(s *workflow.Stage) { s.Default = "plan" })
	}},
	`fmt.Sprintf("%s.cases[%d]", p, i), "invalid expression or missing edge"`: {"stages.verdict.cases[0]", "invalid expression or missing edge", false, func(d *workflow.Document) {
		stage(d, "verdict", func(s *workflow.Stage) {
			s.Cases = append([]workflow.Case(nil), s.Cases...)
			s.Cases[0].Next = "plan"
		})
	}},
	`p, "command vector, positive weight and timeout required"`: {"stages.format", "command vector, positive weight and timeout required", false, func(d *workflow.Document) {
		stage(d, "format", func(s *workflow.Stage) { s.Weight = 0 })
	}},
	`p+".subject", "unknown stage "+s.Subject`: {"stages.freeze-red.subject", "unknown stage ghost", false, func(d *workflow.Document) {
		stage(d, "freeze-red", func(s *workflow.Stage) { s.Subject = "ghost" })
	}},
	`p+".expression", "required"`: {"stages.intake-gate.expression", "required", false, func(d *workflow.Document) {
		stage(d, "intake-gate", func(s *workflow.Stage) { s.Expr = nil })
	}},
	`p+".expression", err.Error()`: {"stages.intake-gate.expression", "", false, func(d *workflow.Document) {
		stage(d, "intake-gate", func(s *workflow.Stage) { s.Expr = expr(workflow.Expr{Op: "teleport"}) })
	}},
	`p+".expression", "must assert positive evidence on every passing arm"`: {"stages.intake-gate.expression", "must assert positive evidence", false, func(d *workflow.Document) {
		stage(d, "intake-gate", func(s *workflow.Stage) { s.Expr = expr(lit(true)) })
	}},
	`p+".expression", "test gate must assert exit zero and executed_count >= 1"`: {"stages.green-gate.expression", "test gate must assert exit zero", false, func(d *workflow.Document) {
		stage(d, "green-gate", func(s *workflow.Stage) { s.TestStage = "format" })
	}},
	`p+".expression", "test-run gate must assert executed_count >= 1 for "+sel.label()`: {"stages.qa-gate.expression", "test-run gate must assert executed_count >= 1 for stage qa", false, func(d *workflow.Document) {
		stage(d, "qa-gate", func(s *workflow.Stage) {
			s.TestStage, s.Expr = "", expr(and(eqField("qa", "exit_code", 0), eqField("qa", "fresh_build", true)))
		})
	}},
	`p+".failure", "failure route must be an edge"`: {"stages.intake-gate.failure", "failure route must be an edge", false, func(d *workflow.Document) {
		stage(d, "intake-gate", func(s *workflow.Stage) { s.Failure = "plan" })
	}},
	`p+".role", "reviewer must differ from every change producer"`: {"stages.spec-review.role", "reviewer must differ", false, func(d *workflow.Document) {
		stage(d, "spec-review", func(s *workflow.Stage) { s.Role = "coder" })
	}},
	`p+".count", "review count must be between 0 and 16"`: {"stages.spec-review.count", "between 0 and 16", false, func(d *workflow.Document) {
		stage(d, "spec-review", func(s *workflow.Stage) { s.Count = 17 })
	}},
	`p+".reviewers", "unknown or change-producing reviewer"`: {"stages.reviews.reviewers", "unknown or change-producing reviewer", false, func(d *workflow.Document) {
		stage(d, "reviews", func(s *workflow.Stage) { s.Reviewers = append(append([]string(nil), s.Reviewers...), "coder") })
	}},
	`p+".reviewers", "output not permitted by reviewer role"`: {"stages.reviews.reviewers", "output not permitted by reviewer role", false, func(d *workflow.Document) {
		stage(d, "reviews", func(s *workflow.Stage) { s.Reviewers = append(append([]string(nil), s.Reviewers...), "planner") })
	}},
	`p, "external effect requires Normal-only mode, pins and idempotence key"`: {"stages.review-create", "external effect requires Normal-only mode", true, func(d *workflow.Document) {
		stage(d, "review-create", func(s *workflow.Stage) { s.NormalOnly = false })
	}},
	`"molecule_concurrency.max_running", "must be -1 (unbounded) or 1..1000000"`: {"molecule_concurrency.max_running", "must be -1", false, func(d *workflow.Document) {
		d.MoleculeConcurrency = &workflow.MoleculeConcurrency{MaxRunning: 0}
	}},
	`"stages."+id, "unreachable"`: {"stages.island", "unreachable", false, func(d *workflow.Document) {
		d.Stages["island"] = workflow.Stage{Primitive: workflow.Escalate}
	}},
	`"stages."+n, "cycle avoids bounded Loop"`: {"stages.", "cycle avoids bounded Loop", false, func(d *workflow.Document) {
		d.Edges = append(d.Edges, workflow.Edge{From: "format", To: "run-tests"})
	}},
	`"stages."+id, "no path to Done"`:  {"stages.integrate", "no path to Done", false, func(d *workflow.Document) { dropEdge(d, "integration-gate", "Done") }},
	`"stages."+id, "no success route"`: {"stages.integration-gate", "no success route", false, func(d *workflow.Document) { dropEdge(d, "integration-gate", "Done") }},
	`"stages."+id, "ambiguous success route: "+strings.Join(success, ", ")`: {"stages.plan", "ambiguous success route: ", false, func(d *workflow.Document) {
		d.Edges = append(d.Edges, workflow.Edge{From: "plan", To: "qa"})
	}},
	`"stages."+q.node, "protected effect has a path without independent review and passing test gate"`: {"stages.commit", "protected effect has a path without independent review", false, func(d *workflow.Document) {
		d.Edges = append(d.Edges, workflow.Edge{From: "implement", To: "commit"})
	}},
	`path, "unknown stage "+x.Stage`: {"stages.intake-gate.expression", "unknown stage ghost", false, func(d *workflow.Document) {
		stage(d, "intake-gate", func(s *workflow.Stage) { s.Expr = expr(and(*s.Expr, eqField("ghost", "accepted", true))) })
	}},
	`path, "unknown artifact type "+x.Type`: {"stages.intake-gate.expression", "unknown artifact type Ghost", false, func(d *workflow.Document) {
		stage(d, "intake-gate", func(s *workflow.Stage) { s.Expr = expr(and(*s.Expr, workflow.Expr{Op: "exists", Type: "Ghost"})) })
	}},
	`"stages." + id + ".phase", "code-bearing change producer must declare test-first or implement phase"`: {"stages.implement.phase", "code-bearing change producer must declare", false, func(d *workflow.Document) {
		stage(d, "implement", func(s *workflow.Stage) { s.Phase = "" })
	}},
	`"stages." + x.node, "freeze requires a passing declared RED gate"`: {"stages.freeze-red", "freeze requires a passing declared RED gate", false, func(d *workflow.Document) {
		stage(d, "run-red", func(s *workflow.Stage) { s.TestShape = "" })
	}},
	`"stages." + x.node, "implementation path lacks frozen RED"`: {"stages.implement", "implementation path lacks frozen RED", false, func(d *workflow.Document) {
		stage(d, "run-red", func(s *workflow.Stage) { s.TestShape = "" })
	}},
	`"stages." + x.node, "commit path lacks frozen RED comparison"`: {"stages.commit", "commit path lacks frozen RED comparison", false, func(d *workflow.Document) {
		stage(d, "green-gate", func(s *workflow.Stage) { s.Expr = expr(dropConjunct(*s.Expr, "compare-red")) })
	}},
	`"stages." + x.node, "commit path lacks a passing run of the frozen RED test"`: {"stages.commit", "commit path lacks a passing run of the frozen RED test", false, func(d *workflow.Document) {
		stage(d, "green-gate", func(s *workflow.Stage) { s.Expr = expr(dropField(*s.Expr, "red_passed")) })
	}},
}

func dropEdge(d *workflow.Document, from, to string) {
	edges := []workflow.Edge{}
	for _, e := range d.Edges {
		if e.From != from || e.To != to {
			edges = append(edges, e)
		}
	}
	d.Edges = edges
}

// dropConjunct removes every conjunct reading the named stage.
func dropConjunct(x workflow.Expr, stage string) workflow.Expr {
	return filterConjuncts(x, func(a workflow.Expr) bool { return len(a.Args) > 0 && a.Args[0].Stage == stage })
}
func dropField(x workflow.Expr, field string) workflow.Expr {
	return filterConjuncts(x, func(a workflow.Expr) bool {
		return len(a.Args) > 0 && len(a.Args[0].Path) == 1 && a.Args[0].Path[0] == field
	})
}
func filterConjuncts(x workflow.Expr, drop func(workflow.Expr) bool) workflow.Expr {
	if x.Op != "and" {
		return x
	}
	keep := []workflow.Expr{}
	for _, a := range x.Args {
		if a.Op == "and" {
			keep = append(keep, filterConjuncts(a, drop))
		} else if !drop(a) {
			keep = append(keep, a)
		}
	}
	if len(keep) == 1 {
		return keep[0]
	}
	return workflow.Expr{Op: "and", Args: keep}
}

// writeOnlyRules are the corpus rules of workflow.RouteEffortIssues: a write
// refuses them (ValidateWrite and the daemon's install), but Validate, which
// Parse runs on every stored revision, does not, so a stored revision that
// breaks one still loads (FAE-WF-093).
var writeOnlyRules = map[string]bool{
	`fmt.Sprintf("%s.routes[%d]", p, i), "effort "+b.Effort+" is not supported by provider "+b.Provider`: true,
}

// FAE-WF-090, FAE-VER-001/-004: every validator rule has a firing case that
// refuses one mutation of the valid default at the rule's own path. A
// write-only rule fires on write and not in Validate.
func TestValidatorRuleCorpus(t *testing.T) {
	for _, base := range []workflow.Document{defaults.Workflow(), defaults.Shipped()} {
		if issues := workflow.ValidateWrite(base); len(issues) != 0 {
			t.Fatal(issues)
		}
	}
	fires := func(issues []workflow.Issue, c ruleCase) bool {
		for _, i := range issues {
			if strings.HasPrefix(i.Path, c.path) && strings.Contains(i.Message, c.message) {
				return true
			}
		}
		return false
	}
	for key, c := range validatorRules {
		d := defaults.Workflow()
		if c.shipped {
			d = defaults.Shipped()
		}
		c.mutate(&d)
		validate := workflow.Validate
		if writeOnlyRules[key] {
			validate = workflow.ValidateWrite
			if fires(workflow.Validate(d), c) {
				t.Errorf("write-only rule %s fired in Validate, so a stored revision would not load", key)
			}
		}
		if !fires(validate(d), c) {
			t.Errorf("rule %s did not fire at %s: %v", key, c.path, validate(d))
		}
	}
}

var (
	badCall   = regexp.MustCompile(`(?m)^\s*bad\((.*)\)\s*$`)
	issueCall = regexp.MustCompile(`(?m)Issue\{(.*)\}\)\s*$`)
)

// The census: the corpus keys are exactly the validator's rule sources.
func TestValidatorRuleCensus(t *testing.T) {
	found := map[string]bool{}
	for file, re := range map[string]*regexp.Regexp{"workflow.go": badCall, "red.go": issueCall} {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
			found[m[1]] = true
		}
	}
	missing, stale := []string{}, []string{}
	for k := range found {
		if _, ok := validatorRules[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range validatorRules {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing)+len(stale) != 0 || len(found) < 60 {
		t.Fatalf("validator rules without a firing case: %q; cases for no rule: %q (%d rules)", missing, stale, len(found))
	}
}
