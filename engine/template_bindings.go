package engine

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/workflow"
	"regexp"
	"strings"
)

var runtimeTemplateReference = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_.:-]*\??)\}\}`)

// ResolveRuntimeReference shares the same typed vocabulary between briefs,
// argv and scope binding. Missing optional data is distinct from a bound null.
func ResolveRuntimeReference(j Job, name string, visit int) (any, bool, error) {
	name = strings.TrimSuffix(name, "?")
	switch name {
	case "task_id":
		return j.TaskID, true, nil
	case "stage":
		return string(j.State), true, nil
	case "visit":
		return visit, true, nil
	case "output_dir":
		return j.OutputDir, j.OutputDir != "", nil
	case "staging_dir":
		return j.StagingDir, j.StagingDir != "", nil
	}
	if strings.HasPrefix(name, "loop.") {
		parts := strings.Split(name, ".")
		if len(parts) != 3 || parts[2] != "iteration" || !workflow.LoopEncloses(j.Document, parts[1], string(j.State)) {
			return nil, false, fmt.Errorf("loop reference is outside its body: %s", name)
		}
		n := j.Counts[parts[1]]
		return n, n > 0, nil
	}
	if strings.HasPrefix(name, "siblings.") {
		if j.BranchContext == nil {
			return nil, false, fmt.Errorf("siblings reference outside FanOut")
		}
		out := []any{}
		for _, sibling := range j.BranchContext.Siblings {
			value := sibling
			for _, part := range strings.Split(strings.TrimPrefix(name, "siblings."), ".") {
				m, ok := value.(map[string]any)
				if !ok {
					return nil, false, fmt.Errorf("sibling field missing in %s", name)
				}
				value, ok = m[part]
				if !ok || value == nil {
					return nil, false, fmt.Errorf("sibling field missing in %s", name)
				}
			}
			out = append(out, value)
		}
		return out, true, nil
	}
	raw, _ := json.Marshal(name)
	var projection workflow.Projection
	if err := json.Unmarshal(raw, &projection); err != nil {
		return nil, false, err
	}
	value, _, err := ResolveProjection(j, projection)
	if err != nil {
		if strings.Contains(err.Error(), "is missing") {
			return nil, false, nil
		}
		return nil, false, err
	}
	return value, value != nil, nil
}

func BindDispatchReferences(j Job, d *Dispatch) error {
	if d.TemplateValues == nil {
		d.TemplateValues = map[string]any{}
	}
	d.TemplateValues["visit"] = d.Visit
	if j.BranchContext != nil {
		d.BranchSiblings = append([]any(nil), j.BranchContext.Siblings...)
	}
	bind := func(name string, optional bool) (bool, error) {
		value, present, err := ResolveRuntimeReference(j, name, d.Visit)
		if err != nil {
			return false, err
		}
		if !present && !optional {
			return false, fmt.Errorf("typed reference %s is absent", name)
		}
		d.TemplateValues[name] = value
		return present, nil
	}
	var brief func([]workflow.TemplatePart) error
	brief = func(parts []workflow.TemplatePart) error {
		for _, part := range parts {
			switch part.Kind {
			case "reference":
				if _, err := bind(part.Name, part.Optional); err != nil {
					return err
				}
			case "present", "absent":
				present, err := bind(part.Name, true)
				if err != nil {
					return err
				}
				if d.TemplatePresence == nil {
					d.TemplatePresence = map[string]bool{}
				}
				d.TemplatePresence[part.Name] = present
				if present == (part.Kind == "present") {
					if err := brief(part.Children); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	patterns := append([]string{}, d.Config.Writes...)
	patterns = append(patterns, d.WriteScope...)
	if d.Config.Generate != nil {
		patterns = append(patterns, d.Config.Generate.Writes...)
	}
	if d.Config.Check != nil {
		patterns = append(patterns, d.Config.Check.Writes...)
	}
	policies := []*workflow.ReadPolicy{d.Config.Reads, j.Reads}
	roles := []string{d.Config.Role}
	if d.Config.Primitive == workflow.Review {
		roles = d.Config.Reviewers
	}
	for _, roleName := range roles {
		role, ok := workflow.EffectiveRole(j.Document, d.TaskClass, roleName)
		if !ok {
			continue
		}
		patterns = append(patterns, role.Writes...)
		policies = append(policies, role.Reads)
		template := d.Config.Brief
		if template == "" {
			template = role.Briefs[d.Config.Phase]
		}
		if template == "" {
			template = role.Briefs["default"]
		}
		parts, err := workflow.ParseBriefTemplate(template)
		if err != nil {
			return err
		}
		if err = brief(parts); err != nil {
			return err
		}
	}
	for _, policy := range policies {
		if policy != nil {
			patterns = append(patterns, policy.Allow...)
			patterns = append(patterns, policy.Deny...)
			for _, set := range policy.AllowSets {
				patterns = append(patterns, set...)
			}
		}
	}
	for _, pattern := range patterns {
		for _, match := range runtimeTemplateReference.FindAllStringSubmatch(pattern, -1) {
			if _, err := bind(strings.TrimSuffix(match[1], "?"), false); err != nil {
				return err
			}
		}
	}
	return nil
}
