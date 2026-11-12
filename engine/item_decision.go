package engine

import (
	"fmt"
	"github.com/xleelz/fae/workflow"
	"strconv"
	"strings"
)

func decisionItems(j Job, s workflow.Stage) ([]any, error) {
	if s.ItemsFrom == nil {
		return nil, nil
	}
	value, _, err := ResolveProjection(j, *s.ItemsFrom)
	if err != nil {
		return nil, err
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("items_from must select a list")
	}
	if len(items) > 10000 {
		return nil, fmt.Errorf("decision items exceed 10000")
	}
	return items, nil
}
func bindItemDecisions(items []any, options []workflow.DecisionOption, choices []workflow.ItemDecision) ([]map[string]any, error) {
	allowed := map[string]bool{}
	for _, o := range options {
		allowed[o.ID] = true
	}
	picked := map[int]workflow.ItemDecision{}
	issues := []string{}
	ids := map[string]int{}
	for i, item := range items {
		if object, ok := item.(map[string]any); ok {
			if id, ok := object["id"]; ok {
				key := fmt.Sprint(id)
				if _, dup := ids[key]; dup {
					ids[key] = -1
				} else {
					ids[key] = i
				}
			}
		}
	}
	for _, choice := range choices {
		index := -1
		if choice.Index != nil {
			index = *choice.Index
		}
		if choice.ID != "" {
			if choice.Index != nil {
				issues = append(issues, "choice supplies both index and id")
				continue
			}
			if value, ok := ids[choice.ID]; ok {
				index = value
			}
		}
		if index < 0 || index >= len(items) {
			issues = append(issues, "unknown item "+choice.ID)
			continue
		}
		if _, dup := picked[index]; dup {
			issues = append(issues, "item "+strconv.Itoa(index)+" decided twice")
			continue
		}
		if !allowed[choice.Option] {
			issues = append(issues, "item "+strconv.Itoa(index)+": undeclared option "+choice.Option)
			continue
		}
		if len(choice.Rationale) > 4096 {
			issues = append(issues, "item "+strconv.Itoa(index)+": rationale exceeds 4096 bytes")
			continue
		}
		picked[index] = choice
	}
	out := make([]map[string]any, len(items))
	for i, item := range items {
		choice, ok := picked[i]
		if !ok {
			issues = append(issues, "item "+strconv.Itoa(i)+": option missing")
			continue
		}
		out[i] = map[string]any{"index": i, "item": item, "option": choice.Option, "rationale": choice.Rationale}
		if object, ok := item.(map[string]any); ok {
			if id, ok := object["id"]; ok {
				out[i]["id"] = id
			}
		}
	}
	if len(issues) > 0 {
		return nil, fmt.Errorf("%s", strings.Join(issues, "; "))
	}
	return out, nil
}
