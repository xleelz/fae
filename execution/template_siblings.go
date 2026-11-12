package execution

import (
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/engine"
	"strings"
)

func scopeLiteral(v any) (string, bool) {
	switch value := v.(type) {
	case string:
		return value, true
	case int, int64, float64, json.Number:
		return fmt.Sprint(value), true
	default:
		return "", false
	}
}
func expandSiblingDenies(patterns []string, d engine.Dispatch) ([]string, error) {
	if patterns == nil {
		return nil, nil
	}
	out := []string{}
	for _, pattern := range patterns {
		matches := briefReference.FindAllStringSubmatch(pattern, -1)
		has := false
		for _, match := range matches {
			has = has || strings.HasPrefix(match[1], "siblings.")
		}
		if !has {
			out = append(out, pattern)
			continue
		}
		for _, sibling := range d.BranchSiblings {
			expanded := pattern
			for _, match := range matches {
				name := strings.TrimSuffix(match[1], "?")
				if !strings.HasPrefix(name, "siblings.") {
					continue
				}
				value, err := lookupTemplateField(sibling, strings.TrimPrefix(name, "siblings."))
				if err != nil {
					return nil, fmt.Errorf("sibling binding: %w", err)
				}
				text, ok := scopeLiteral(value)
				if !ok || text == "" || strings.Contains(text, "..") || strings.ContainsAny(text, "*?[]\\\x00\n\r") || strings.HasPrefix(text, "/") {
					return nil, fmt.Errorf("sibling binding %s is not a safe literal path", name)
				}
				expanded = strings.ReplaceAll(expanded, match[0], text)
			}
			out = append(out, expanded)
		}
	}
	return out, nil
}
