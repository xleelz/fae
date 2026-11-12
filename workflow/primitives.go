package workflow

import (
	"fmt"
	"sort"
	"strings"
)

// StayWaiting is the only HumanDecision timeout behaviour: a decision is
// never taken automatically (FAE-WF-030).
const StayWaiting = "stay-waiting"

// configRouted stages leave by a route their configuration selects, not by a
// single success edge.
func configRouted(s Stage) bool {
	return s.Primitive == HumanDecision || s.Primitive == FanOut
}

// FailureRouted stages leave by their failure route when their recorded check
// fails: a Gate, a Review with an aggregation rule and a HostCommand with an
// expected shape. Their success route is the other outgoing edge.
func FailureRouted(s Stage) bool {
	return s.Primitive == Gate || s.Primitive == Review && s.Aggregation != "" || s.Primitive == HostCommand && s.ExpectedShape != nil || s.Primitive == FanIn || s.Failure != "" && (s.Primitive == AgentTurn || s.Primitive == Review || s.Primitive == HostCommand || s.Primitive == SubWorkflow || s.Primitive == ChangeRequest)
}

// Review aggregation rules.
const (
	AllPass          = "all-pass"
	KOfN             = "k-of-n"
	AnyBlockingFails = "any-blocking-fails"
)

// ReviewSessions is the number of isolated review sessions of a Review stage.
func ReviewSessions(s Stage) int {
	return len(stageRoles(s)) * max(s.Count, 1)
}

func countOf(stage string, filter ...Expr) Expr {
	return Expr{Op: "count", Stage: stage, Args: filter}
}
func compare(op string, a Expr, v any) Expr {
	return Expr{Op: op, Args: []Expr{a, {Op: "literal", Value: v}}}
}
func both(a, b Expr) Expr { return Expr{Op: "and", Args: []Expr{a, b}} }

// AggregateExpr is the aggregate verdict of a Review stage's current batch.
// Every rule first asserts that all N isolated reviews are present; a missing
// approved flag, findings list or unrecognised severity is unknown and fails.
func AggregateExpr(id string, s Stage) Expr {
	n := ReviewSessions(s)
	present := compare("eq", countOf(id), n)
	approved := countOf(id, compare("eq", Expr{Op: "field", Stage: id, Path: []string{"approved"}}, true))
	switch s.Aggregation {
	case KOfN:
		return both(present, compare("ge", approved, s.Quorum))
	case AnyBlockingFails:
		blocking := Expr{Op: "count", Stage: id, Path: []string{"findings"}, Args: []Expr{compare("eq", Expr{Op: "item", Path: []string{"severity"}, Enum: []string{"blocking", "non-blocking"}}, "blocking")}}
		return both(present, compare("eq", countOf(id, compare("gt", blocking, 0)), 0))
	}
	return both(present, compare("eq", approved, n))
}

// ShapeExpr is the expected-shape check over a HostCommand's latest run.
func ShapeExpr(id string, shape CommandShape) Expr {
	field := func(name string) Expr { return Expr{Op: "field", Stage: id, Path: []string{name}} }
	checks := []Expr{}
	switch shape.Exit {
	case "zero":
		checks = append(checks, compare("eq", field("exit_code"), 0))
	case "nonzero":
		checks = append(checks, compare("ne", field("exit_code"), 0))
	}
	if shape.Executed > 0 {
		checks = append(checks, compare("eq", field("executed_count"), shape.Executed))
	}
	if shape.MinExecuted > 0 {
		checks = append(checks, compare("ge", field("executed_count"), shape.MinExecuted))
	}
	if shape.Matched > 0 {
		checks = append(checks, compare("eq", field("matched_count"), shape.Matched))
	}
	if len(checks) == 0 {
		return Expr{Op: "literal", Value: false}
	}
	x := checks[0]
	for _, c := range checks[1:] {
		x = both(x, c)
	}
	return x
}

// RenderReason substitutes {task}, {stage} and {attempt} in an Escalate
// reason template; values are inserted as data, never re-expanded.
func RenderReason(template, task, stage string, attempt int) string {
	return strings.NewReplacer("{task}", task, "{stage}", stage, "{attempt}", fmt.Sprint(attempt)).Replace(template)
}

// EffectiveProfile is the role profile tightened by a stage override:
// allowed capabilities intersect, denials unite, permissions conjoin.
func EffectiveProfile(role Profile, s Stage) Profile {
	if s.Profile == nil {
		return role
	}
	out := Profile{ShellAllow: role.ShellAllow, DenyTestRuns: role.DenyTestRuns || s.Profile.DenyTestRuns, Allowed: []string{}, Denied: append([]string{}, role.Denied...), Build: role.Build && s.Profile.Build, Test: role.Test && s.Profile.Test, Format: role.Format && s.Profile.Format, Shims: role.Shims}
	if s.Profile.ShellAllow != nil {
		out.ShellAllow = s.Profile.ShellAllow
	}
	if len(s.Profile.Shims) > 0 {
		out.Shims = []string{}
		for _, n := range s.Profile.Shims {
			if len(role.Shims) == 0 || contains(role.Shims, n) {
				out.Shims = append(out.Shims, n)
			}
		}
	}
	for _, n := range s.Profile.Effects {
		if contains(role.Effects, n) {
			out.Effects = append(out.Effects, n)
		}
	}
	for _, c := range role.Allowed {
		if contains(s.Profile.Allowed, c) {
			out.Allowed = append(out.Allowed, c)
		}
	}
	if s.Profile.Allowed == nil && (s.Profile.ShellAllow != nil || s.Profile.DenyTestRuns) {
		out.Allowed = append([]string{}, role.Allowed...)
	}
	for _, c := range s.Profile.Denied {
		if !contains(out.Denied, c) {
			out.Denied = append(out.Denied, c)
		}
	}
	return out
}

// EnvironmentList renders a HostCommand environment policy as sorted KEY=VALUE.
func EnvironmentList(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// reservedVariable names variables a workflow may not set: the broker's own
// sanitized base, loader and Git controls, and anything credential-like.
func reservedVariable(name string) bool {
	// Explicit instance selection is data, never a grant. Keep every other
	// FAE variable reserved, especially lease, permit and credential markers.
	switch name {
	case "FAE_HOME", "FAE_INSTANCE", "FAE_INSTANCE_ID", "FAE_CONTROL_PLANE":
		return false
	}

	switch name {
	case "PATH", "HOME", "TMPDIR", "TMP", "TEMP", "LANG", "SHELL", "USER", "PWD":
		return true
	}
	for _, prefix := range []string{"LC_", "GIT_", "FAE_", "LD_", "DYLD_", "CLAUDE", "ANTHROPIC_", "OPENAI_", "CODEX_", "AWS_", "GOOGLE_", "AZURE_", "DOLT"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	for _, part := range []string{"TOKEN", "SECRET", "PASSWORD", "CREDENTIAL", "KEY", "AUTH"} {
		if strings.Contains(name, part) {
			return true
		}
	}
	return false
}
func variableName(name string) bool {
	if name == "" || len(name) > 64 || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for _, r := range name {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}

// Option returns the declared HumanDecision option with this id.
func (s Stage) Option(id string) (DecisionOption, bool) {
	for _, o := range s.Options {
		if o.ID == id {
			return o, true
		}
	}
	return DecisionOption{}, false
}

// DecisionFields is the canonical decision artifact the engine records for a
// HumanDecision, citing the operator message that took it.
func DecisionFields(decision, question, option, message, actor, rationale string) map[string]any {
	f := map[string]any{"decision_id": decision, "question": question, "option": option, "authorizing_message": message, "decided_by": actor}
	if rationale != "" {
		f["rationale"] = rationale
	}
	return f
}

func identifier(s string, limit int) bool {
	if s == "" || len(s) > limit {
		return false
	}
	for i, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || i > 0 && (r == '-' || r == '_' || r == '.')
		if !ok {
			return false
		}
	}
	return true
}

// validatePrimitives checks the FAE-WF-030 configuration of the primitives
// whose fields live in this file. A field of one primitive is refused on any
// other, so a misplaced key never silently does nothing (FAE-WF-004).
// requiredConfiguration enforces schema 2's FAE-WF-030 required items that
// schema 1 left optional: a Gate's human-readable name, an Escalate stage's
// Appendix B kind and its reason template.
func requiredConfiguration(s Stage, p string, bad func(string, string)) {
	if s.Primitive == Gate && strings.TrimSpace(s.Name) == "" {
		bad(p+".name", "required in schema 2: a human-readable gate name")
	}
	if s.Primitive == Escalate && s.Kind == "" {
		bad(p+".kind", "required in schema 2: an Appendix B escalation kind")
	}
	if s.Primitive == Escalate && strings.TrimSpace(s.Reason) == "" {
		bad(p+".reason", "required in schema 2: a reason template")
	}
}

func validatePrimitives(d Document, outgoing map[string][]string, bad func(string, string)) {
	ids := make([]string, 0, len(d.Stages))
	for id := range d.Stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s, p := d.Stages[id], "stages."+id
		validateConfiguration(d, id, s, outgoing, bad)
		if d.SchemaVersion >= 2 {
			requiredConfiguration(s, p, bad)
		}
		if s.Primitive != HumanDecision {
			if s.Question != "" || len(s.Options) > 0 || s.OnTimeout != "" {
				bad(p, "question, options and on_timeout belong to HumanDecision stages")
			}
			continue
		}
		if strings.TrimSpace(s.Question) == "" || len(s.Question) > 4096 {
			bad(p+".question", "a non-empty question of at most 4096 bytes is required")
		}
		if s.OnTimeout != StayWaiting {
			bad(p+".on_timeout", `must be "stay-waiting": a decision is never taken automatically`)
		}
		if s.Output == "" {
			bad(p+".output", "required: the decision artifact type")
		}
		if s.Role != "" || s.ChangeProducer {
			bad(p, "a HumanDecision is taken by the operator: no role and no change production")
		}
		if (len(s.Options) == 0 && s.ItemsFrom == nil) || len(s.Options) > 32 {
			bad(p+".options", "between 1 and 32 declared options required")
		}
		named := map[string]bool{}
		if len(s.Options) == 0 && s.ItemsFrom != nil {
			if len(outgoing[id]) != 1 {
				bad(p+".options", "item-only decisions require one outgoing route")
			} else {
				named[outgoing[id][0]] = true
			}
		}
		for _, o := range s.ItemOptions {
			if strings.TrimSpace(o.Description) == "" || len(o.Description) > 1024 {
				bad(p+".item_options", "each item option requires a description of at most 1024 bytes")
			}
		}
		seen := map[string]bool{}
		for i, o := range s.Options {
			op := fmt.Sprintf("%s.options[%d]", p, i)
			if !identifier(o.ID, 64) || seen[o.ID] {
				bad(op+".id", "unique option id of letters, digits, '-', '_' or '.' required")
			}
			seen[o.ID] = true
			if !contains(outgoing[id], o.Next) {
				bad(op+".next", "option route must be an outgoing edge")
			}
			named[o.Next] = true
			if len(o.Description) > 1024 {
				bad(op+".description", "at most 1024 bytes")
			}
		}
		for _, to := range outgoing[id] {
			if !named[to] {
				bad(p+".options", "edge to "+to+" is not the route of a declared option")
			}
		}
	}
}

// validateConfiguration checks the remaining FAE-WF-030 configuration fields:
// Escalate reason templates, Review aggregation, Loop exit gate, backoff and
// exhaustion kind, HostCommand expected shape and environment, stage profile
// overrides (tighten only) and Gate names.
func validateConfiguration(d Document, id string, s Stage, outgoing map[string][]string, bad func(string, string)) {
	p := "stages." + id
	if s.Reason != "" {
		rest := RenderReason(s.Reason, "", "", 0)
		if s.Primitive != Escalate || len(s.Reason) > 1024 || strings.ContainsAny(rest, "{}") || strings.TrimSpace(s.Reason) == "" {
			bad(p+".reason", "an Escalate reason template of at most 1024 bytes using only {task}, {stage} and {attempt}")
		}
	}
	if s.Aggregation != "" || s.Quorum != 0 && s.Primitive != FanIn {
		n := ReviewSessions(s)
		switch {
		case s.Primitive != Review:
			bad(p+".aggregation", "aggregation and quorum belong to Review stages")
		case s.Aggregation != AllPass && s.Aggregation != KOfN && s.Aggregation != AnyBlockingFails:
			bad(p+".aggregation", "aggregation rule must be all-pass, k-of-n or any-blocking-fails")
		case (s.Aggregation == KOfN) != (s.Quorum != 0) || s.Quorum < 0 || s.Quorum > n:
			bad(p+".quorum", fmt.Sprintf("k-of-n requires a quorum between 1 and the %d isolated reviews; other rules take none", n))
		}
		if s.Primitive == Review && !contains(outgoing[id], s.Failure) {
			bad(p+".failure", "an aggregated review needs a failure route edge")
		}
	}
	if s.ExitGate != "" || s.BackoffSeconds != 0 || s.OnExhaustion != "" {
		if s.Primitive != Loop {
			bad(p, "exit_gate, backoff_seconds and on_exhaustion belong to Loop stages")
		}
		if g, ok := d.Stages[s.ExitGate]; s.ExitGate != "" && (!ok || g.Primitive != Gate || g.Failure != id) {
			bad(p+".exit_gate", "exit gate must be a Gate whose failure route re-enters this loop")
		}
		if s.BackoffSeconds < 0 || s.BackoffSeconds > 86400 {
			bad(p+".backoff_seconds", "between 0 and 86400")
		}
		if s.OnExhaustion != "" && !s.OnExhaustion.Valid() {
			bad(p+".on_exhaustion", "exhaustion escalates with an Appendix B kind")
		}
	}
	if s.ExpectedShape != nil {
		sh := *s.ExpectedShape
		if s.Primitive != HostCommand {
			bad(p+".expected_shape", "expected_shape belongs to HostCommand stages")
		}
		if sh.Exit != "" && sh.Exit != "zero" && sh.Exit != "nonzero" || sh.Executed < 0 || sh.MinExecuted < 0 || sh.Matched < 0 || sh.Executed > 0 && sh.MinExecuted > 0 || sh == (CommandShape{}) {
			bad(p+".expected_shape", "declare exit zero|nonzero, executed or min_executed (not both) and/or matched")
		}
		if !contains(outgoing[id], s.Failure) {
			bad(p+".failure", "a shaped host command needs a failure route edge")
		}
	}
	if len(s.Environment) > 0 {
		if s.Primitive != HostCommand || len(s.Environment) > 32 {
			bad(p+".environment", "at most 32 settings on a HostCommand stage")
		}
		for k, v := range s.Environment {
			if !variableName(k) || reservedVariable(k) || len(v) > 4096 || strings.ContainsRune(v, 0) {
				bad(p+".environment."+k, "an upper-case, non-reserved, non-credential name with a bounded value")
			}
		}
	}
	if s.Profile != nil {
		if s.Primitive != AgentTurn && s.Primitive != Review {
			bad(p+".profile", "a profile override belongs to AgentTurn and Review stages")
		}
		for _, c := range s.Profile.Allowed {
			if !ValidToolCapability(c) {
				bad(p+".profile.allowed", "unknown capability "+c)
			}
		}
		for _, role := range stageRoles(s) {
			r, ok := d.Roles[role]
			if !ok {
				continue
			}
			widened := s.Profile.Build && !r.Profile.Build || s.Profile.Test && !r.Profile.Test || s.Profile.Format && !r.Profile.Format
			for _, c := range s.Profile.Allowed {
				widened = widened || !contains(r.Profile.Allowed, c)
			}
			for _, c := range r.Profile.Denied {
				widened = widened || !contains(s.Profile.Denied, c)
			}
			if widened {
				bad(p+".profile", "a stage may only tighten role "+role+"'s profile")
			}
		}
	}
	if s.Name != "" {
		printable := true
		for _, r := range s.Name {
			printable = printable && r >= ' ' && r != 0x7f
		}
		if s.Primitive != Gate || len(s.Name) > 128 || strings.TrimSpace(s.Name) == "" || !printable {
			bad(p+".name", "a printable Gate name of at most 128 bytes")
		}
	}
}
