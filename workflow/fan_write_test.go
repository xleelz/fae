package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNewFanInWriteRequiresListOutputButLegacyReadsRemain(t *testing.T) {
	doc := fanDoc()
	doc.SchemaVersion = CurrentSchema
	doc.TargetRequirements = &TargetRequirements{}
	// Bring the old fixture's required current-schema fields up to date.
	for id, s := range doc.Stages {
		if s.Primitive == HostCommand {
			doc.TargetRequirements.Commands = append(doc.TargetRequirements.Commands, s.Command)
		}
		if s.Primitive == Gate && s.Name == "" {
			s.Name = "fixture"
		}
		if s.Primitive == Escalate {
			s.Kind = "HumanDecisionRequired"
			s.Reason = "fixture"
		}
		doc.Stages[id] = s
	}
	if got := JoinOutputIssues(doc); len(got) != 1 || !strings.Contains(got[0].Message, "declare a list") {
		t.Fatal(got)
	}
	raw, _ := json.Marshal(doc)
	if _, err := Parse(raw); err != nil {
		t.Fatal("legacy no-output revision stopped loading", err)
	}
	if _, err := ParseWrite(raw); err == nil || !strings.Contains(err.Error(), "declare a list") {
		t.Fatal("new no-output revision accepted", err)
	}
	join := doc.Stages["join"]
	join.Output = "Joined"
	doc.Stages["join"] = join
	doc.Artifacts["Joined"] = ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	if got := JoinOutputIssues(doc); len(got) != 1 || !strings.Contains(got[0].Message, "type array") {
		t.Fatal(got)
	}
	doc.Artifacts["Joined"] = ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"array","items":{"type":"object"}}`)}
	if got := JoinOutputIssues(doc); len(got) != 0 {
		t.Fatal(got)
	}
	// A region producing several types must choose the joined output.
	doc.Artifacts["other"] = doc.Artifacts["report"]
	extra := doc.Stages["review"]
	extra.Output = "other"
	extra.Role = "extra"
	role := doc.Roles[doc.Stages["review"].Role]
	role.Outputs = []string{"other"}
	doc.Roles["extra"] = role
	doc.Stages["extra"] = extra
	for i, e := range doc.Edges {
		if e.From == "review" && e.To == "join" {
			doc.Edges[i].To = "extra"
		}
	}
	doc.Edges = append(doc.Edges, Edge{From: "extra", To: "join"})
	found := false
	for _, issue := range Validate(doc) {
		if strings.Contains(issue.Message, "several artifact types") {
			found = true
		}
	}
	if !found {
		t.Fatal("ambiguous branch output accepted")
	}
	fan := doc.Stages["fan"]
	fan.Branches[0].Output = "report"
	doc.Stages["fan"] = fan
	if issues := ValidateWrite(doc); len(issues) != 0 {
		t.Fatal("explicit branch output refused", issues)
	}
}
