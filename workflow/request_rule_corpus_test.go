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

// requestPolicyRule fires one request-policy validator rule: the mutation of
// the default document must produce an issue at path whose message contains
// message.
type requestPolicyRule struct {
	path, message string
	mutate        func(*workflow.Document)
}

func requestPolicyStage(d *workflow.Document, id string, f func(*workflow.Stage)) {
	s := d.Stages[id]
	f(&s)
	d.Stages[id] = s
}

func requestPolicyDropEdge(d *workflow.Document, from, to string) {
	edges := []workflow.Edge{}
	for _, e := range d.Edges {
		if e.From != from || e.To != to {
			edges = append(edges, e)
		}
	}
	d.Edges = edges
}

// requestPolicyRules is keyed by the exact source text of each rule's issue
// arguments in validateRequests (workflow/requests.go): the rules that a role
// request (FAE-WF-078) is judged by a declared, independent role and counted
// against a cap. The census below fails when a rule is added without a firing
// case, and scripts/check-ledger.py maps every key (FAE-VER-001).
var requestPolicyRules = map[string]requestPolicyRule{
	`"requests.from", "required"`: {"requests.from", "required", func(d *workflow.Document) { d.Requests.From = nil }},
	`path, "must name an AgentTurn stage"`: {"requests.from[0]", "must name an AgentTurn stage", func(d *workflow.Document) {
		d.Requests.From = []string{"run-tests"}
	}},
	`path, "output artifact must declare a request field"`: {"requests.from[1]", "output artifact must declare a request field", func(d *workflow.Document) {
		d.Requests.From = append(d.Requests.From, "plan")
	}},
	`path, "requesting stage needs an edge to the judge"`: {"requests.from[0]", "requesting stage needs an edge to the judge", func(d *workflow.Document) {
		requestPolicyDropEdge(d, "implement", "request-judge")
	}},
	`"requests.judge", "must name an AgentTurn stage other than the requesting stages"`: {"requests.judge", "must name an AgentTurn stage other than the requesting stages", func(d *workflow.Document) {
		d.Requests.Judge = "implement"
	}},
	`"requests.judge", "judge role must differ from every requesting and change-producing role"`: {"requests.judge", "judge role must differ from every requesting and change-producing role", func(d *workflow.Document) {
		requestPolicyStage(d, "request-judge", func(s *workflow.Stage) { s.Role = d.Stages["implement"].Role })
	}},
	`"requests.judge", "judge artifact must declare a boolean approved field"`: {"requests.judge", "judge artifact must declare a boolean approved field", func(d *workflow.Document) {
		a := d.Artifacts["RequestDecision"]
		a.Schema = json.RawMessage(`{"type":"object","properties":{"approved":{"type":"string"}}}`)
		d.Artifacts["RequestDecision"] = a
	}},
	`"requests.rework_cap", "a RED rework route needs a positive cap"`: {"requests.rework_cap", "a RED rework route needs a positive cap", func(d *workflow.Document) {
		d.Requests.ReworkCap = 0
	}},
	`"requests.rework_cap", "requires a RED rework route"`: {"requests.rework_cap", "requires a RED rework route", func(d *workflow.Document) {
		d.Requests.Rework = ""
	}},
	`"requests", "judge needs an edge to route "+r`: {"requests", "judge needs an edge to route red-loop", func(d *workflow.Document) {
		requestPolicyDropEdge(d, "request-judge", "red-loop")
	}},
	`"requests.judge", "judge routes only to the widen and rework routes, not "+to`: {"requests.judge", "judge routes only to the widen and rework routes, not qa", func(d *workflow.Document) {
		d.Edges = append(d.Edges, workflow.Edge{From: "request-judge", To: "qa"})
	}},
}

// FAE-WF-078, FAE-VER-001: every request-policy rule has a firing case that
// refuses one mutation of the valid default at the rule's own path, and the
// census checks the cases are exactly validateRequests' rules.
func TestRequestPolicyRuleCorpus(t *testing.T) {
	if issues := workflow.Validate(defaults.Workflow()); len(issues) != 0 || defaults.Workflow().Requests == nil {
		t.Fatal("the default declares no valid request policy", issues)
	}
	for key, c := range requestPolicyRules {
		d := defaults.Workflow()
		c.mutate(&d)
		found := false
		for _, i := range workflow.Validate(d) {
			found = found || strings.HasPrefix(i.Path, c.path) && strings.Contains(i.Message, c.message)
		}
		if !found {
			t.Errorf("rule %s did not fire at %s: %v", key, c.path, workflow.Validate(d))
		}
	}
	raw, err := os.ReadFile("requests.go")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*bad\((.*)\)\s*$`).FindAllStringSubmatch(string(raw), -1) {
		found[m[1]] = true
	}
	missing, stale := []string{}, []string{}
	for k := range found {
		if _, ok := requestPolicyRules[k]; !ok {
			missing = append(missing, k)
		}
	}
	for k := range requestPolicyRules {
		if !found[k] {
			stale = append(stale, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	if len(missing)+len(stale) != 0 || len(found) < 10 {
		t.Fatalf("request-policy rules without a firing case: %q; cases for no rule: %q (%d rules)", missing, stale, len(found))
	}
}
