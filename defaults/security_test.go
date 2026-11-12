package defaults

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-SEC-007: a change touching credentials, network exposure or file
// serving passes review only when the security review cites the guidance
// consulted for each such domain; the security record must carry both lists.
func TestSecurityReviewCitesGuidance(t *testing.T) {
	reviews := func(security map[string]any) engine.ScriptStep {
		step := engine.ScriptStep{}
		for _, dimension := range Dimensions {
			r := map[string]any{"approved": true, "dimension": dimension, "findings": []any{}}
			if dimension == "security" {
				for k, v := range security {
					r[k] = v
				}
			}
			step.Reviews = append(step.Reviews, raw(r))
		}
		return step
	}
	cite := map[string]any{"domain": "credentials", "source": "org security handbook, secrets chapter", "outcome": "tokens stay in the sealed environment"}
	uncited := reviews(map[string]any{"sensitive_domains": []string{"credentials"}, "guidance": []any{}})
	script := HappyScript()
	script["reviews"] = []engine.ScriptStep{uncited, reviews(map[string]any{"sensitive_domains": []string{"credentials"}, "guidance": []any{cite}})}
	j := drive(t, script)
	if j.State != domain.Done || !routed(j, "reviews-gate", "implement-loop") {
		t.Fatal("uncited sensitive change passed review", j.State, j.Reason)
	}
	cited := false
	for _, a := range j.Evidence {
		if a.Stage == "reviews" && a.Fields["dimension"] == "security" && strings.Contains(string(raw(a.Fields["guidance"])), "secrets chapter") {
			cited = true
		}
	}
	if !cited {
		t.Fatal("the passing review record does not cite the guidance")
	}
	schema := Workflow().Artifacts["Review"].Schema
	for body, ok := range map[string]bool{
		`{"approved":true,"dimension":"security","findings":[]}`:                                                              false,
		`{"approved":true,"dimension":"security","findings":[],"sensitive_domains":["dns"],"guidance":[]}`:                    false,
		`{"approved":true,"dimension":"security","findings":[],"sensitive_domains":["file-serving"],"guidance":[]}`:           true,
		`{"approved":true,"dimension":"quality","findings":[]}`:                                                               true,
		`{"approved":true,"dimension":"security","findings":[],"sensitive_domains":[],"guidance":[{"domain":"credentials"}]}`: false,
	} {
		if got := workflow.ValidateArtifact(schema, json.RawMessage(body)) == nil; got != ok {
			t.Error(body, got)
		}
	}
}
