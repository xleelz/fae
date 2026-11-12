package workflow

import (
	"fmt"
	"slices"
	"strings"
)

// TargetRequirements is what a target configuration pinned for this workflow
// must provide for the content to run as written (FAE-WF-001): the literal
// HostCommand vectors its allowlist must permit, and whether it must declare
// a diff classifier (no-red, FAE-WF-073) or always-required checks (FAE-WF-085).
type TargetRequirements struct {
	Commands       [][]string `json:"commands"`
	DiffClassifier bool       `json:"diff_classifier,omitempty"`
	RequiredChecks bool       `json:"required_checks,omitempty"`
}

// UnmetTargetRequirements lists what a target configuration lacks; nil
// requirements (schema-1 content) ask for nothing.
func UnmetTargetRequirements(r *TargetRequirements, allowed [][]string, classifier, checks bool) []string {
	if r == nil {
		return nil
	}
	out := []string{}
	for _, c := range r.Commands {
		if !slices.ContainsFunc(allowed, func(a []string) bool { return slices.Equal(a, c) }) {
			out = append(out, "allowlisted command "+strings.Join(c, " "))
		}
	}
	if r.DiffClassifier && !classifier {
		out = append(out, "a diff classifier")
	}
	if r.RequiredChecks && !checks {
		out = append(out, "always-required tests or a compile check")
	}
	return out
}

// validateGrants checks tool-profile shims and effect grants, stage
// overrides that may only tighten them, effect stages acting under a grant
// and, from schema 2, the target-requirements section.
func validateGrants(d Document, bad func(string, string)) {
	names := func(p string, list []string, what string) {
		seen := map[string]bool{}
		for _, n := range list {
			if !identifier(n, 64) || seen[n] {
				bad(p, "unique "+what+" names required, not "+fmt.Sprintf("%q", n))
			}
			seen[n] = true
		}
	}
	for name, r := range d.Roles {
		names("roles."+name+".profile.shims", r.Profile.Shims, "command shim")
		names("roles."+name+".profile.effects", r.Profile.Effects, "effect subject")
	}
	for id, s := range d.Stages {
		p := "stages." + id
		if s.Profile != nil {
			names(p+".profile.shims", s.Profile.Shims, "command shim")
			names(p+".profile.effects", s.Profile.Effects, "effect subject")
			for _, role := range stageRoles(s) {
				r := d.Roles[role].Profile
				widened := len(r.Shims) > 0 && slices.ContainsFunc(s.Profile.Shims, func(n string) bool { return !slices.Contains(r.Shims, n) })
				widened = widened || slices.ContainsFunc(s.Profile.Effects, func(n string) bool { return !slices.Contains(r.Effects, n) })
				if widened {
					bad(p+".profile", "a stage may only tighten role "+role+"'s shims and effect grants")
				}
			}
		}
		if d.SchemaVersion >= 2 && s.Primitive == ExternalEffect && s.Role != "" && !slices.Contains(d.Roles[s.Role].Profile.Effects, s.Subject) {
			bad(p+".role", "role "+s.Role+" holds no effect grant for "+s.Subject)
		}
	}
	r := d.TargetRequirements
	if r == nil {
		if d.SchemaVersion >= 2 {
			bad("target_requirements", "required in schema 2: the default target-configuration requirements")
		}
		return
	}
	for i, c := range r.Commands {
		used := false
		for _, s := range d.Stages {
			used = used || s.Primitive == HostCommand && len(s.Bindings) == 0 && len(s.ArgTemplates) == 0 && slices.Equal(s.Command, c)
		}
		if len(c) == 0 || !used {
			bad(fmt.Sprintf("target_requirements.commands[%d]", i), "must be the literal vector of a HostCommand stage")
		}
	}
	for id, s := range d.Stages {
		if s.Primitive == HostCommand && len(s.Bindings) == 0 && len(s.ArgTemplates) == 0 && !slices.ContainsFunc(r.Commands, func(c []string) bool { return slices.Equal(c, s.Command) }) {
			bad("target_requirements.commands", "lacks the vector of HostCommand stage "+id)
		}
	}
}
