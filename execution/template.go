package execution

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

var briefReference = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_.:-]*\??)\}\}`)

// renderStageTemplate preserves instruction text while inserting each bound
// reference as a typed JSON data block at its exact position.
func renderStageTemplate(template string, d engine.Dispatch, b Binding) (any, error) {
	if !strings.Contains(template, "{{") {
		return template, nil
	}
	parts, err := workflow.ParseBriefTemplate(template)
	if err != nil {
		return nil, err
	}
	blocks := []any{}
	var render func([]workflow.TemplatePart) error
	render = func(parts []workflow.TemplatePart) error {
		for _, part := range parts {
			if part.Kind == "text" {
				blocks = append(blocks, map[string]any{"kind": "template_text", "text": part.Text})
				continue
			}
			value, err := resolveTemplateValue(part.Name, d, b)
			missing := err != nil && (strings.Contains(err.Error(), "missing") || strings.Contains(err.Error(), "unbound") || strings.Contains(err.Error(), "absent"))
			if err != nil && !missing {
				return err
			}
			present := err == nil && value != nil
			if part.Kind == "present" || part.Kind == "absent" {
				if bound, ok := d.TemplatePresence[part.Name]; ok {
					present = bound
				}
				keep := present == (part.Kind == "present")
				blocks = append(blocks, map[string]any{"kind": "template_presence", "reference": part.Name, "present": present, "kept": keep, "section": part.Kind})
				if keep {
					if err := render(part.Children); err != nil {
						return err
					}
				}
				continue
			}
			if !present {
				if !part.Optional {
					return fmt.Errorf("template reference %s is absent", part.Name)
				}
				blocks = append(blocks, map[string]any{"kind": "quoted_data", "reference": part.Name, "status": "absent", "type": "absent"})
				continue
			}
			blocks = append(blocks, map[string]any{"kind": "quoted_data", "reference": part.Name, "value": value})
		}
		return nil
	}
	if err := render(parts); err != nil {
		return nil, err
	}
	return blocks, nil
}
func resolveTemplateValue(name string, d engine.Dispatch, b Binding) (any, error) {
	name = strings.TrimSuffix(name, "?")
	if value, ok := d.TemplateValues[name]; ok {
		return value, nil
	}
	if name == "visit" {
		return d.Visit, nil
	}
	if strings.HasPrefix(name, "loop.") && b.TemplateJob != nil {
		value, present, err := engine.ResolveRuntimeReference(*b.TemplateJob, name, d.Visit)
		if err != nil {
			return nil, err
		}
		if !present {
			return nil, fmt.Errorf("template reference %s is absent", name)
		}
		return value, nil
	}
	switch name {
	case "task_id":
		return d.TaskID, nil
	case "stage":
		return d.Stage, nil
	case "output_dir":
		if b.OutputDir == "" {
			return nil, fmt.Errorf("output directory missing")
		}
		return b.OutputDir, nil
	case "staging_dir":
		if b.StagingDir == "" {
			return nil, fmt.Errorf("staging directory missing")
		}
		return b.StagingDir, nil
	}
	var p workflow.Projection
	raw, _ := json.Marshal(name)
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Source == "item" {
		v := d.BranchItem
		if p.Field == "" {
			return v, nil
		}
		return lookupTemplateField(v, p.Field)
	}
	if b.TemplateJob != nil {
		v, _, err := engine.ResolveProjection(*b.TemplateJob, p)
		return v, err
	}
	for i := len(d.Inputs) - 1; i >= 0; i-- {
		a := d.Inputs[i]
		if p.Input != "" && a.Stage != "input:"+p.Input {
			continue
		}
		if p.Type != "" && a.Type != p.Type {
			continue
		}
		if p.Stage != "" && a.Stage != p.Stage {
			continue
		}
		if p.Source == "failure" && a.Type != "failure" {
			continue
		}
		var v any = a.Fields
		if p.Input != "" && len(a.Fields) == 1 {
			if x, ok := a.Fields["value"]; ok {
				v = x
			}
		}
		if p.Field == "" {
			return v, nil
		}
		return lookupTemplateField(v, p.Field)
	}
	return nil, fmt.Errorf("template reference %s is unbound", name)
}
func lookupTemplateField(value any, path string) (any, error) {
	for _, name := range strings.Split(path, ".") {
		m, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("template field %s missing", path)
		}
		value, ok = m[name]
		if !ok {
			return nil, fmt.Errorf("template field %s missing", path)
		}
	}
	return value, nil
}

// BoundWriteScopes binds item/input references without widening the intersection.
func BoundWriteScopes(role workflow.Role, d engine.Dispatch, b Binding) ([][]string, error) {
	layers := workflow.EffectiveWriteScopes(role, d.Config, d.WriteScope, b.TaskWrites)
	if d.Config.ChangeProducer && d.Config.Generate == nil && len(role.Writes) == 0 && len(d.Config.Writes) == 0 {
		layers = append([][]string{append([]string{}, b.Scope.Files...)}, layers...)
	}

	for i, patterns := range layers {
		expanded, err := expandScopePatterns(patterns, d, b)
		if err != nil {
			return nil, err
		}
		layers[i] = expanded
	}
	return layers, nil
}
func expandScopePatterns(patterns []string, d engine.Dispatch, b Binding) ([]string, error) {
	if patterns == nil {
		return nil, nil
	}
	out := []string{}
	for _, pattern := range patterns {
		matches := briefReference.FindAllStringSubmatch(pattern, -1)
		value := pattern
		if len(matches) == 1 && matches[0][0] == pattern {
			v, err := resolveTemplateValue(matches[0][1], d, b)
			if err != nil {
				return nil, err
			}
			if list, ok := v.([]any); ok {
				for _, item := range list {
					path, ok := item.(string)
					if !ok || !workflow.ValidWriteGlob(path) {
						return nil, fmt.Errorf("scope reference must contain safe path globs")
					}
					out = append(out, path)
				}
				continue
			}
		}
		for _, match := range matches {
			v, err := resolveTemplateValue(match[1], d, b)
			if err != nil {
				return nil, err
			}
			text, ok := scopeLiteral(v)
			if !ok {
				return nil, fmt.Errorf("scope reference %s must be scalar", match[1])
			}
			if text == "" || strings.Contains(text, "..") || strings.ContainsAny(text, "*?[]\\\x00\n\r") || strings.HasPrefix(text, "/") {
				return nil, fmt.Errorf("scope reference %s is not a safe literal path", match[1])
			}
			value = strings.ReplaceAll(value, match[0], text)
		}
		if !workflow.ValidWriteGlob(value) {
			return nil, fmt.Errorf("invalid bound write scope %s", value)
		}
		out = append(out, value)
	}
	return out, nil
}

// BoundScopePatterns binds command write globs with the same typed values as
// agentic writes, without changing the command's authored specification.
func BoundScopePatterns(patterns []string, d engine.Dispatch, b Binding) ([]string, error) {
	return expandScopePatterns(patterns, d, b)
}

func boundReadPolicy(policy *workflow.ReadPolicy, d engine.Dispatch, b Binding) (*workflow.ReadPolicy, error) {
	if policy == nil {
		return nil, nil
	}
	copy := *policy
	var err error
	if copy.Allow, err = expandScopePatterns(policy.Allow, d, b); err != nil {
		return nil, err
	}
	siblingPatterns, siblingErr := expandSiblingDenies(policy.Deny, d)
	if siblingErr != nil {
		return nil, siblingErr
	}
	if copy.Deny, err = expandScopePatterns(siblingPatterns, d, b); err != nil {
		return nil, err
	}
	copy.AllowSets = nil
	for _, set := range policy.AllowSets {
		expanded, e := expandScopePatterns(set, d, b)
		if e != nil {
			return nil, e
		}
		copy.AllowSets = append(copy.AllowSets, expanded)
	}
	return &copy, nil
}

// BoundReadPolicy resolves the same typed policy for trusted command sessions.
func BoundReadPolicy(policy *workflow.ReadPolicy, d engine.Dispatch, b Binding) (*workflow.ReadPolicy, error) {
	return boundReadPolicy(policy, d, b)
}
