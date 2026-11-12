package workflow

import "encoding/json"

// TaskClass is a declared class of task. A code-bearing class puts every
// path of the graph under the test-first floor (FAE-WF-073); overlays
// override a role's reasoning effort and brief templates for tasks of the
// class (FAE-WF-044).
type TaskClass struct {
	CodeBearing bool               `json:"code_bearing,omitempty"`
	Overlays    map[string]Overlay `json:"overlays,omitempty"`
}

// Overlay tightens nothing and widens nothing: it only replaces the effort of
// every route binding and the named brief templates of one role.
type Overlay struct {
	Effort string            `json:"effort,omitempty"`
	Briefs map[string]string `json:"briefs,omitempty"`
}

// CodeBearing reports whether tasks of class run under the test-first floor.
// The legacy document flag makes every class code-bearing; a class can only
// tighten it.
func CodeBearing(d Document, class string) bool {
	return d.Produces != "artifacts" && (d.CodeBearing || d.TaskClasses[class].CodeBearing)
}

// anyCodeBearing reports whether the validator must prove the RED floor.
func anyCodeBearing(d Document) bool {
	if d.Produces == "artifacts" {
		return false
	}
	if d.CodeBearing {
		return true
	}
	for _, c := range d.TaskClasses {
		if c.CodeBearing {
			return true
		}
	}
	return false
}

// EffectiveRole applies the class overlay to a declared role.
func EffectiveRole(d Document, class, name string) (Role, bool) {
	r, ok := d.Roles[name]
	if !ok {
		return r, false
	}
	o, overlaid := d.TaskClasses[class].Overlays[name]
	if !overlaid {
		return r, true
	}
	if o.Effort != "" {
		routes := make([]Binding, len(r.Routes))
		for i, b := range r.Routes {
			b.Effort = o.Effort
			routes[i] = b
		}
		r.Routes = routes
	}
	if len(o.Briefs) > 0 {
		briefs := map[string]string{}
		for k, v := range r.Briefs {
			briefs[k] = v
		}
		for k, v := range o.Briefs {
			briefs[k] = v
		}
		r.Briefs = briefs
	}
	return r, true
}

// overlayEffortIssues is RouteEffortIssues for task-class overlays: an
// overlay's effort applies to every route of its role, so one a route's
// provider refuses is refused on write.
func overlayEffortIssues(d Document, bad func(string, string)) {
	for name, c := range d.TaskClasses {
		for role, o := range c.Overlays {
			for _, b := range d.Roles[role].Routes {
				if providerRefusesEffort(b.Provider, o.Effort) {
					bad("task_classes."+name+".overlays."+role+".effort", "effort "+o.Effort+" is not supported by provider "+b.Provider)
					break
				}
			}
		}
	}
}

// validateClasses checks task classes, overlays and no-red declarations.
func validateClasses(d Document, outgoing map[string][]string, phases map[string]map[string]bool, bad func(string, string)) {
	if _, ok := d.TaskClasses[d.DefaultClass]; d.DefaultClass != "" && !ok {
		bad("default_class", "unknown task class")
	}
	for name, c := range d.TaskClasses {
		p := "task_classes." + name
		if name == "" {
			bad(p, "empty task class name")
		}
		for role, o := range c.Overlays {
			op := p + ".overlays." + role
			if _, ok := d.Roles[role]; !ok {
				bad(op, "unknown role")
			}
			switch o.Effort {
			case "", "minimal", "low", "medium", "high", "xhigh":
			default:
				bad(op+".effort", "unknown effort")
			}
			if o.Effort == "" && len(o.Briefs) == 0 {
				bad(op, "overlay overrides nothing")
			}
			for key, text := range o.Briefs {
				if key != "default" && (key == "" || !phases[role][key]) {
					bad(op+".briefs."+key, "brief key must be default or a declared phase of a stage using this role")
				}
				if text == "" {
					bad(op+".briefs."+key, "empty brief template")
				}
			}
		}
	}
	if len(d.NoRedStages) > 0 && !anyCodeBearing(d) {
		bad("no_red_stages", "no-red declarations require a code-bearing task class")
	}
	producers := map[string]bool{}
	producerRoles := map[string]bool{}
	for id, s := range d.Stages {
		if ProducesChanges(d,s) {
			producers[id] = true
			producerRoles[s.Role] = true
		}
	}
	// Stages reachable from any change producer can never declare no-red.
	late := map[string]bool{}
	var visit func(string)
	visit = func(n string) {
		for _, to := range outgoing[n] {
			if !late[to] {
				late[to] = true
				visit(to)
			}
		}
	}
	for id := range producers {
		visit(id)
	}
	seen := map[string]bool{}
	for i, id := range d.NoRedStages {
		p := "no_red_stages[" + itoa(i) + "]"
		s, ok := d.Stages[id]
		if !ok || seen[id] {
			bad(p, "unknown or duplicate stage")
			continue
		}
		seen[id] = true
		if s.Primitive != AgentTurn || s.ChangeProducer || producerRoles[s.Role] || late[id] {
			bad(p, "only a pre-code intake or spec turn, never an implementing role, may declare no-red")
		}
		if a, ok := d.Artifacts[s.Output]; ok {
			var schema struct {
				Properties map[string]struct {
					Type any `json:"type"`
				} `json:"properties"`
			}
			if json.Unmarshal(a.Schema, &schema) == nil && schema.Properties != nil && schema.Properties["no_red"].Type != "boolean" {
				bad(p, "output artifact must declare a boolean no_red field")
			}
		}
	}
}

// noRedArm reports whether a Branch case is taken only for a task declared
// no-red: it conjunctively asserts no_red == true on a declaring stage.
func noRedArm(d Document, x Expr) bool {
	return conjunct(x, func(e Expr) bool {
		if e.Op != "eq" || len(e.Args) != 2 {
			return false
		}
		a, b := e.Args[0], e.Args[1]
		return a.Op == "field" && contains(d.NoRedStages, a.Stage) && !a.All && len(a.Path) == 1 && a.Path[0] == "no_red" && b.Op == "literal" && b.Value == true
	})
}

func itoa(i int) string {
	b, _ := json.Marshal(i)
	return string(b)
}
