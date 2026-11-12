package execution

import (
	"sort"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
)

func notificationsBlock(notifications []engine.TaskNotification) any {
	return provider.BuildNotificationsBriefBlock(notifications)
}

// obligations lists the gates and branch arms that will read stage's output,
// so the kernel contract states what the turn is judged by (FAE-WF-043).
func obligations(d workflow.Document, stage string) []map[string]any {
	output := d.Stages[stage].Output
	reads := func(x workflow.Expr) bool {
		found := false
		var walk func(workflow.Expr)
		walk = func(e workflow.Expr) {
			if (e.Op == "field" || e.Op == "count" || e.Op == "exists") && (e.Stage == stage || e.Stage == "" && output != "" && e.Type == output) {
				found = true
			}
			for _, a := range e.Args {
				walk(a)
			}
		}
		walk(x)
		return found
	}
	ids := make([]string, 0, len(d.Stages))
	for id := range d.Stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := []map[string]any{}
	for _, id := range ids {
		s := d.Stages[id]
		if s.Primitive == workflow.Gate && s.Expr != nil && reads(*s.Expr) {
			out = append(out, map[string]any{"gate": id, "expression": *s.Expr, "on_failure": s.Failure})
		}
		if s.Primitive == workflow.Branch {
			for _, arm := range s.Cases {
				if reads(arm.When) {
					out = append(out, map[string]any{"branch": id, "cases": s.Cases, "default": s.Default})
					break
				}
			}
		}
	}
	return out
}

// frozenRule is the kernel's frozen-artifact rule; it names the governed
// request route only where the workflow declares one (FAE-WF-078).
func frozenRule(d workflow.Document) string {
	rule := "Do not modify a frozen RED test or any frozen artifact."
	if d.Requests != nil && d.Requests.Rework != "" {
		rule += " When the RED itself is wrong, request a RED rework (request kind red-rework with its reason) through the handoff instead."
	}
	if d.Requests != nil {
		rule += " Request a scope widen (kind scope-widen, the additional files and the reason) rather than editing outside the declared and approved scope."
	}
	return rule
}
