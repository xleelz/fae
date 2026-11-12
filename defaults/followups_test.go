package defaults

import (
	"strings"
	"testing"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-WF-084: an approving verdict that defers valid non-blocking findings
// leaves one durable follow-up record per deferred finding, naming the
// verdict artifact; invalid and blocking findings are not follow-ups.
func TestDeferredFindingsRecordedAsFollowUps(t *testing.T) {
	finding := func(text string, valid bool, severity string) map[string]any {
		return map[string]any{"finding": text, "valid": valid, "severity": severity, "target": "Production", "rationale": "fixture"}
	}
	script := HappyScript()
	script["oracle"] = []engine.ScriptStep{{Output: raw(map[string]any{"approved": true, "findings": []any{finding("rename helper", true, "non-blocking"), finding("log noise", true, "non-blocking"), finding("not a real issue", false, "non-blocking")}, "rationale": "fixture"})}}
	j := drive(t, script)
	if j.State != domain.Done {
		t.Fatal(j.State, j.Reason)
	}
	verdict := ""
	for _, a := range j.Evidence {
		if a.Stage == "oracle" {
			verdict = a.ID
		}
	}
	got := []string{}
	for _, e := range j.History {
		if e.Kind == "follow-up" {
			if !strings.HasPrefix(e.Cause, verdict+": ") || e.Stage != "oracle" || e.DispatchID == "" {
				t.Fatal("follow-up does not cite its verdict", e)
			}
			got = append(got, e.Cause)
		}
	}
	if len(got) != 2 || !strings.Contains(got[0], "rename helper") || !strings.Contains(got[1], "log noise") {
		t.Fatal("follow-ups", got)
	}
	d := Workflow()
	s := d.Stages["oracle"]
	s.FollowUps = &workflow.FollowUps{Path: nil, When: s.FollowUps.When}
	d.Stages["oracle"] = s
	if len(workflow.Validate(d)) == 0 {
		t.Fatal("follow-ups without a path accepted")
	}
}
