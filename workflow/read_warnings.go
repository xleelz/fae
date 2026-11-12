package workflow

import "sort"

// ReadPolicyWarnings never re-admits a denied path. A proven dead allow is a
// content warning, rather than a change to deny-wins semantics.
func ReadPolicyWarnings(d Document) []Issue {
	out := []Issue{}
	seen := map[string]bool{}
	for id, stage := range d.Stages {
		roles := stageRoles(stage)
		if len(roles) == 0 {
			roles = []string{""}
		}
		for _, name := range roles {
			role := d.Roles[name]
			denies := []string{}
			if role.Reads != nil {
				denies = append(denies, role.Reads.Deny...)
			}
			if stage.Reads != nil {
				denies = append(denies, stage.Reads.Deny...)
			}
			for _, layer := range []struct {
				path   string
				policy *ReadPolicy
			}{{"roles." + name + ".reads", role.Reads}, {"stages." + id + ".reads", stage.Reads}} {
				if layer.policy == nil {
					continue
				}
				for _, allow := range layer.policy.Allow {
					if GlobScopeNarrows([]string{allow}, denies) {
						key := layer.path + "/" + allow
						if !seen[key] {
							seen[key] = true
							out = append(out, Issue{layer.path, "dead allow " + allow + ": covered by deny; deny wins"})
						}
					}
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path+out[i].Message < out[j].Path+out[j].Message })
	return out
}
