package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOptionalTemplatePathsAndPresenceSectionsValidate(t *testing.T) {
	doc := Document{Signature: &Signature{Inputs: map[string]json.RawMessage{"contract": json.RawMessage(`{"type":["object","null"],"properties":{"component":{"type":"string"}},"required":["component"]}`)}}, Stages: map[string]Stage{"read": {Primitive: AgentTurn, Role: "author", Brief: "{{input.contract.component}}"}}, Roles: map[string]Role{"author": {}}}
	check := func() []Issue {
		issues := []Issue{}
		validateBriefTemplates(doc, func(path, message string) { issues = append(issues, Issue{path, message}) })
		return issues
	}
	if issues := check(); len(issues) == 0 || !strings.Contains(issues[0].Message, "null or absent") {
		t.Fatal("nullable path accepted", issues)
	}
	s := doc.Stages["read"]
	s.Brief = "{{#present input.contract}}Child {{input.contract.component?}}{{/present}}{{#absent input.contract}}Root{{/absent}}"
	doc.Stages["read"] = s
	if issues := check(); len(issues) != 0 {
		t.Fatal(issues)
	}
	for _, broken := range []string{"{{#present input.contract}}open", "{{/present}}", "{{#absent input.contract}}{{/present}}"} {
		if _, err := ParseBriefTemplate(broken); err == nil {
			t.Fatal("malformed section accepted", broken)
		}
	}
}
func TestDeadReadAllowIsWarningAndDenyStillWins(t *testing.T) {
	d := Document{Roles: map[string]Role{"author": {Reads: &ReadPolicy{Deny: []string{"notes/**"}}}}, Stages: map[string]Stage{"read": {Primitive: AgentTurn, Role: "author", Reads: &ReadPolicy{Allow: []string{"notes/{{item.id}}.md"}}}}}
	warnings := ReadPolicyWarnings(d)
	if len(warnings) != 1 || !strings.Contains(warnings[0].Message, "dead allow") {
		t.Fatal(warnings)
	}
}
