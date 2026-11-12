package workflow

import (
	"encoding/json"
	"testing"
)

// FAE-WF-093 with FAE-WF-041: Claude Code has no minimal effort, so a route
// or task-class overlay that binds it can never run. A write refuses it, but a
// stored revision that holds it, written before the rule existed, still
// parses: admission, topology reads, diff and simulate keep working on it, and
// at run time the bridge refuses the binding before the provider starts, so the
// turn moves to the role's next route.
func TestStoredClaudeMinimalRevisionParsesAndWritesRefuseIt(t *testing.T) {
	route := noRedDocument()
	r := route.Roles["writer"]
	r.Routes = append([]Binding{{Provider: "claude", Model: "claude-opus-5-5", Effort: "minimal"}}, r.Routes...)
	route.Roles["writer"] = r
	overlay := noRedDocument()
	r = overlay.Roles["writer"]
	r.Routes = append(r.Routes, Binding{Provider: "claude", Model: "claude-opus-5-5", Effort: "medium"})
	overlay.Roles["writer"] = r
	overlay.TaskClasses["docs"].Overlays["writer"] = Overlay{Effort: "minimal"}
	for name, c := range map[string]struct {
		doc  Document
		path string
	}{
		"route":   {route, "roles.writer.routes[0]"},
		"overlay": {overlay, "task_classes.docs.overlays.writer.effort"},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(c.doc)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = Parse(raw); err != nil {
				t.Fatal("a stored revision with a Claude minimal effort no longer parses:", err)
			}
			if issues := Validate(c.doc); len(issues) != 0 {
				t.Fatal("Validate refuses a stored revision:", issues)
			}
			if issues := ValidateWrite(c.doc); !hasIssue(issues, c.path, "effort minimal is not supported by provider claude") {
				t.Fatal("a write accepts a Claude minimal effort:", issues)
			}
		})
	}
	// Another provider's minimal effort is not refused on write either.
	other := noRedDocument()
	other.TaskClasses["docs"].Overlays["writer"] = Overlay{Effort: "minimal"}
	if issues := ValidateWrite(other); hasIssue(issues, "task_classes.docs.overlays.writer.effort", "not supported") {
		t.Fatal(issues)
	}
}
