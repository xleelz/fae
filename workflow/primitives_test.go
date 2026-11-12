package workflow

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
)

// decisionDoc places a HumanDecision between the independent review and the
// commit: ship commits, rework loops back to the change, halt escalates.
func decisionDoc() Document {
	d := protected()
	d.Artifacts["decision"] = ArtifactType{1, json.RawMessage(`{"type":"object","required":["decision_id","option","authorizing_message"]}`)}
	d.Stages["approve"] = Stage{Primitive: HumanDecision, Question: "Ship the reviewed change?", Options: []DecisionOption{{ID: "ship", Next: "commit"}, {ID: "rework", Next: "again"}, {ID: "halt", Next: "stop", Description: "stop here"}}, OnTimeout: StayWaiting, Output: "decision"}
	d.Stages["again"] = Stage{Primitive: Loop, MaxIterations: 2}
	edges := []Edge{}
	for _, e := range d.Edges {
		if e != (Edge{"review", "commit"}) {
			edges = append(edges, e)
		}
	}
	d.Edges = append(edges, Edge{"review", "approve"}, Edge{"approve", "commit"}, Edge{"approve", "again"}, Edge{"approve", "stop"}, Edge{"again", "change"})
	return d
}

func expectIssue(t *testing.T, name string, d Document, path, message string) {
	t.Helper()
	for _, i := range Validate(d) {
		if i.Path == path && strings.Contains(i.Message, message) {
			return
		}
	}
	t.Errorf("%s: no issue %s %q in %v", name, path, message, Validate(d))
}

// FAE-WF-030 HumanDecision conformance: required question, options routed to
// edges, stay-waiting timeout and a decision artifact; floors still hold on
// every option route.
func TestHumanDecisionValidation(t *testing.T) {
	if issues := Validate(decisionDoc()); len(issues) > 0 {
		t.Fatal(issues)
	}
	edit := func(f func(*Stage)) Document {
		d := decisionDoc()
		s := d.Stages["approve"]
		f(&s)
		d.Stages["approve"] = s
		return d
	}
	for _, c := range []struct {
		name, path, message string
		d                   Document
	}{
		{"no question", "stages.approve.question", "non-empty question", edit(func(s *Stage) { s.Question = " " })},
		{"no timeout", "stages.approve.on_timeout", "stay-waiting", edit(func(s *Stage) { s.OnTimeout = "" })},
		{"auto decide", "stages.approve.on_timeout", "never taken automatically", edit(func(s *Stage) { s.OnTimeout = "auto-approve" })},
		{"no output", "stages.approve.output", "decision artifact", edit(func(s *Stage) { s.Output = "" })},
		{"role", "stages.approve", "no role", edit(func(s *Stage) { s.Role = "coder" })},
		{"no options", "stages.approve.options", "between 1 and 32", edit(func(s *Stage) { s.Options = nil })},
		{"duplicate option", "stages.approve.options[1].id", "unique option id", edit(func(s *Stage) { s.Options[1].ID = "ship" })},
		{"blank option", "stages.approve.options[0].id", "unique option id", edit(func(s *Stage) { s.Options[0].ID = "bad id" })},
		{"route not an edge", "stages.approve.options[0].next", "outgoing edge", edit(func(s *Stage) { s.Options[0].Next = "review" })},
		{"edge without option", "stages.approve.options", "edge to stop", edit(func(s *Stage) { s.Options = s.Options[:2] })},
		{"question on gate", "stages.gate", "belong to HumanDecision", func() Document {
			d := decisionDoc()
			g := d.Stages["gate"]
			g.Question = "?"
			d.Stages["gate"] = g
			return d
		}()},
		// An option route that skips the independent review reaches the commit.
		{"floor bypass", "stages.commit", "without independent review", func() Document {
			d := decisionDoc()
			for i, e := range d.Edges {
				if e == (Edge{"gate", "review"}) {
					d.Edges[i] = Edge{"gate", "approve"}
				}
				if e == (Edge{"review", "approve"}) {
					d.Edges[i] = Edge{"review", "commit"}
				}
			}
			s := d.Stages["approve"]
			s.Options = append(s.Options, DecisionOption{ID: "review", Next: "review"})
			d.Edges = append(d.Edges, Edge{"approve", "review"})
			d.Stages["approve"] = s
			return d
		}()},
	} {
		expectIssue(t, c.name, c.d, c.path, c.message)
	}
	raw, _ := json.Marshal(decisionDoc())
	if _, e := Parse(raw); e != nil {
		t.Fatal(e)
	}
	if _, e := Parse([]byte(strings.Replace(string(raw), `"id":"ship"`, `"id":"ship","auto":true`, 1))); e == nil || !strings.Contains(e.Error(), "options[") {
		t.Fatal("unknown option key accepted", e)
	}
	if raw, _ = json.Marshal(protected()); strings.Contains(string(raw), "question") || strings.Contains(string(raw), "on_timeout") {
		t.Fatal("unused primitive fields change existing content hashes")
	}
}

// configDoc declares every remaining FAE-WF-030 configuration field on the
// protected graph: a named exit gate of a backed-off Loop with its exhaustion
// kind, a k-of-n aggregated review with a tightened profile, a shaped host
// command with an environment policy and an Escalate reason template.
func configDoc() Document {
	d := protected()
	d.Stages["again"] = Stage{Primitive: Loop, MaxIterations: 3, ExitGate: "gate", BackoffSeconds: 5, OnExhaustion: domain.TestRunCap}
	g := d.Stages["gate"]
	g.Failure, g.Name = "again", "Unit tests green"
	d.Stages["gate"] = g
	r := d.Stages["review"]
	r.Aggregation, r.Count, r.Quorum, r.Failure = KOfN, 3, 2, "stop"
	r.Profile = &Profile{Allowed: []string{}, Denied: []string{"edit"}}
	d.Stages["review"] = r
	h := d.Stages["test"]
	h.ExpectedShape, h.Failure, h.Environment = &CommandShape{Exit: "zero", MinExecuted: 1}, "stop", map[string]string{"GOFLAGS": "-mod=mod", "CGO_ENABLED": "0"}
	d.Stages["test"] = h
	st := d.Stages["stop"]
	st.Reason = "{task} stopped at {stage} on attempt {attempt}"
	d.Stages["stop"] = st
	for i, e := range d.Edges {
		if e == (Edge{"gate", "stop"}) {
			d.Edges[i] = Edge{"gate", "again"}
		}
	}
	d.Edges = append(d.Edges, Edge{"again", "change"}, Edge{"review", "stop"}, Edge{"test", "stop"})
	return d
}

// FAE-WF-030 configuration conformance: each field validates on its own
// primitive and is refused, with its path, when malformed or misplaced.
func TestPrimitiveConfigurationValidation(t *testing.T) {
	if issues := Validate(configDoc()); len(issues) > 0 {
		t.Fatal(issues)
	}
	edit := func(stage string, f func(*Stage)) Document {
		d := configDoc()
		s := d.Stages[stage]
		f(&s)
		d.Stages[stage] = s
		return d
	}
	for _, c := range []struct {
		name, path, message string
		d                   Document
	}{
		{"reason placeholder", "stages.stop.reason", "only {task}", edit("stop", func(s *Stage) { s.Reason = "{who} stopped" })},
		{"reason on gate", "stages.gate.reason", "Escalate reason", edit("gate", func(s *Stage) { s.Reason = "x" })},
		{"reason too long", "stages.stop.reason", "1024", edit("stop", func(s *Stage) { s.Reason = strings.Repeat("r", 1025) })},
		{"unknown aggregation", "stages.review.aggregation", "all-pass, k-of-n", edit("review", func(s *Stage) { s.Aggregation = "majority" })},
		{"k-of-n without quorum", "stages.review.quorum", "between 1 and the 3", edit("review", func(s *Stage) { s.Quorum = 0 })},
		{"quorum above n", "stages.review.quorum", "between 1 and the 3", edit("review", func(s *Stage) { s.Quorum = 4 })},
		{"quorum on all-pass", "stages.review.quorum", "other rules take none", edit("review", func(s *Stage) { s.Aggregation = AllPass })},
		{"aggregate without failure", "stages.review.failure", "failure route", edit("review", func(s *Stage) { s.Failure = "test" })},
		{"aggregation on agent turn", "stages.change.aggregation", "belong to Review", edit("change", func(s *Stage) { s.Aggregation = AllPass })},
		{"exit gate not a gate", "stages.again.exit_gate", "Gate whose failure", edit("again", func(s *Stage) { s.ExitGate = "review" })},
		{"exit gate elsewhere", "stages.again.exit_gate", "Gate whose failure", func() Document {
			d := configDoc()
			g := d.Stages["gate"]
			g.Failure = "stop"
			d.Stages["gate"] = g
			d.Edges = append(d.Edges, Edge{"gate", "stop"})
			return d
		}()},
		{"negative backoff", "stages.again.backoff_seconds", "between 0", edit("again", func(s *Stage) { s.BackoffSeconds = -1 })},
		{"day-long backoff", "stages.again.backoff_seconds", "between 0", edit("again", func(s *Stage) { s.BackoffSeconds = 86401 })},
		{"exhaustion kind", "stages.again.on_exhaustion", "Appendix B", edit("again", func(s *Stage) { s.OnExhaustion = "GaveUp" })},
		{"loop fields elsewhere", "stages.gate", "belong to Loop", edit("gate", func(s *Stage) { s.BackoffSeconds = 1 })},
		{"empty shape", "stages.test.expected_shape", "declare exit", edit("test", func(s *Stage) { s.ExpectedShape = &CommandShape{} })},
		{"shape exit", "stages.test.expected_shape", "declare exit", edit("test", func(s *Stage) { s.ExpectedShape.Exit = "maybe" })},
		{"exact and minimum", "stages.test.expected_shape", "not both", edit("test", func(s *Stage) { s.ExpectedShape.Executed = 2 })},
		{"shape without failure", "stages.test.failure", "failure route", edit("test", func(s *Stage) { s.Failure = "" })},
		{"shape on agent turn", "stages.change.expected_shape", "belongs to HostCommand", edit("change", func(s *Stage) { s.ExpectedShape = &CommandShape{Matched: 1} })},
		{"reserved variable", "stages.test.environment.PATH", "non-reserved", edit("test", func(s *Stage) { s.Environment["PATH"] = "/tmp" })},
		{"credential variable", "stages.test.environment.ANTHROPIC_API_KEY", "non-credential", edit("test", func(s *Stage) { s.Environment["ANTHROPIC_API_KEY"] = "x" })},
		{"token variable", "stages.test.environment.MY_TOKEN", "non-credential", edit("test", func(s *Stage) { s.Environment["MY_TOKEN"] = "x" })},
		{"lower-case variable", "stages.test.environment.goflags", "upper-case", edit("test", func(s *Stage) { s.Environment["goflags"] = "x" })},
		{"environment elsewhere", "stages.change.environment", "HostCommand", edit("change", func(s *Stage) { s.Environment = map[string]string{"GOFLAGS": "x"} })},
		{"widened capability", "stages.review.profile", "only tighten", edit("review", func(s *Stage) { s.Profile.Allowed = []string{"edit"} })},
		{"dropped denial", "stages.change.profile", "only tighten", func() Document {
			d := configDoc()
			r := d.Roles["coder"]
			r.Profile.Denied = []string{"external-effect"}
			d.Roles["coder"] = r
			s := d.Stages["change"]
			s.Profile = &Profile{}
			d.Stages["change"] = s
			return d
		}()},
		{"widened build", "stages.review.profile", "only tighten", edit("review", func(s *Stage) { s.Profile.Build = true })},
		{"profile on gate", "stages.gate.profile", "AgentTurn and Review", edit("gate", func(s *Stage) { s.Profile = &Profile{} })},
		{"name on review", "stages.review.name", "printable Gate name", edit("review", func(s *Stage) { s.Name = "reviews" })},
		{"control name", "stages.gate.name", "printable Gate name", edit("gate", func(s *Stage) { s.Name = "bad\nname" })},
		// A failed aggregate verdict confers no review on its failure route.
		{"aggregate failure bypass", "stages.commit", "without independent review", edit("review", func(s *Stage) { s.Failure = "commit" })},
	} {
		expectIssue(t, c.name, c.d, c.path, c.message)
	}
	tight := EffectiveProfile(Profile{Allowed: []string{"handoff", "edit"}, Denied: []string{"external-effect"}, Build: true, Test: true}, Stage{Profile: &Profile{Allowed: []string{"handoff", "read"}, Denied: []string{"exec"}, Test: true}})
	if strings.Join(tight.Allowed, ",") != "handoff" || strings.Join(tight.Denied, ",") != "external-effect,exec" || tight.Build || !tight.Test || tight.Format {
		t.Fatal("effective profile", tight)
	}
	if got := strings.Join(EnvironmentList(map[string]string{"B": "2", "A": "1"}), " "); got != "A=1 B=2" {
		t.Fatal(got)
	}
	if got := RenderReason("{task}/{stage}/{attempt}/{task}", "t{stage}", "s", 3); got != "t{stage}/s/3/t{stage}" {
		t.Fatal("reason values must not be re-expanded", got)
	}
}

func TestExplicitRuntimeSelectionCannotGrantCredentialsOrPermits(t *testing.T) {
	for _, name := range []string{"FAE_HOME", "FAE_INSTANCE", "FAE_INSTANCE_ID", "FAE_CONTROL_PLANE"} {
		d := configDoc()
		s := d.Stages["test"]
		s.Environment = map[string]string{name: "isolated"}
		d.Stages["test"] = s
		if issues := Validate(d); len(issues) > 0 {
			t.Errorf("explicit selector %s refused: %v", name, issues)
		}
	}
	for _, name := range []string{"FAE_HEAVY_OP_PERMIT", "FAE_LEASE_HELD", "FAE_TOKEN", "FAE_AUTH_TOKEN", "FAE_CONTROL_PLANE_DSN", "DOLT_PASSWORD", "AWS_ACCESS_KEY_ID"} {
		d := configDoc()
		s := d.Stages["test"]
		s.Environment = map[string]string{name: "injected"}
		d.Stages["test"] = s
		if issues := Validate(d); len(issues) == 0 {
			t.Errorf("credential or permit selector %s accepted", name)
		}
	}
}
