package workflow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// ValidateCatalog resolves every exact child pin and checks its public contract.
// The active recursion stack, rather than a global visited set, proves that all
// members of each cycle declare budgets.
func ValidateCatalog(root Document, resolve FootprintResolver) []Issue {
	var issues []Issue
	seen := map[string]bool{}
	var walk func(Document, []Document, []string)
	walk = func(doc Document, stack []Document, keys []string) {
		for _, id := range signatureNames(doc.Stages) {
			s := doc.Stages[id]
			if s.Primitive != SubWorkflow || s.Workflow == nil {
				continue
			}
			ref := *s.Workflow
			child := doc
			key := fmt.Sprintf("%s@%d", ref.ID, ref.Revision)
			if ref.ID == "self" {
				key = doc.ID + "@self"
			} else {
				var err error
				child, err = resolve(ref)
				if err != nil {
					issues = append(issues, Issue{"stages." + id + ".workflow", err.Error()})
					continue
				}
			}
			issues = append(issues, ValidateChildBindings(doc, id, child)...)
			cycle := -1
			for i, d := range append(stack, doc) {
				if d.ID == child.ID {
					cycle = i
					break
				}
			}
			if cycle >= 0 {
				for _, d := range append(stack, doc)[cycle:] {
					if d.Recursion == nil {
						issues = append(issues, Issue{"stages." + id + ".workflow", "mutual recursion requires budgets on every document: " + d.ID})
					}
				}
				if child.Recursion == nil {
					issues = append(issues, Issue{"stages." + id + ".workflow", "recursive child requires budgets: " + child.ID})
				}
				continue
			}
			if len(stack) >= 64 {
				issues = append(issues, Issue{"stages." + id + ".workflow", "catalog nesting exceeds 64"})
				continue
			}
			if !seen[key] {
				seen[key] = true
				walk(child, append(stack, doc), append(keys, key))
			}
		}
	}
	walk(root, nil, nil)
	return issues
}

func ValidateChildBindings(parent Document, stage string, child Document) []Issue {
	var out []Issue
	s := parent.Stages[stage]
	path := "stages." + stage
	add := func(p, m string) { out = append(out, Issue{path + p, m}) }
	if child.Signature != nil {
		mapped := map[string]bool{}
		for _, m := range s.InputMap {
			mapped[m.Name] = true
			var target json.RawMessage = child.Signature.Inputs[m.Name]
			if len(target) > 0 {
				var source json.RawMessage
				var known bool
				var item json.RawMessage
				if s.ItemsFrom != nil {
					if list, ok := projectionSchema(parent, *s.ItemsFrom, nil); ok {
						var spec struct {
							Items json.RawMessage `json:"items"`
						}
						_ = json.Unmarshal(list, &spec)
						item = spec.Items
					}
				}
				projection := m.Projection
				if projection == nil {
					projection = &Projection{Stage: m.Stage, Type: m.Type, Input: m.Input, Field: m.Field}
				}
				source, known = projectionSchema(parent, *projection, item)
				if known && !SchemaSatisfies(source, target) {
					add(".input_map."+m.Name, "source schema is incompatible with child input")
				}
				if len(projection.Default) > 0 && ValidateArtifact(target, projection.Default) != nil {
					add(".input_map."+m.Name, "projection default does not satisfy child input")
				}
			}
			if _, ok := child.Signature.Inputs[m.Name]; !ok {
				add(".input_map."+m.Name, "unknown child input")
			}
		}
		for _, name := range signatureNames(child.Signature.Inputs) {
			var spec map[string]json.RawMessage
			_ = json.Unmarshal(child.Signature.Inputs[name], &spec)
			if _, ok := spec["default"]; !ok && !mapped[name] {
				add(".input_map."+name, "required child input is not mapped")
			}
		}
	}
	for i, m := range s.OutputMap {
		p := fmt.Sprintf(".output_map[%d]", i)
		typ := m.Type
		var schema json.RawMessage
		if m.Output != "" {
			if child.Signature == nil {
				add(p, "child has no declared outputs")
				continue
			}
			decl, ok := child.Signature.Outputs[m.Output]
			if !ok {
				add(p, "unknown child output "+m.Output)
				continue
			}
			if typ != "" && typ != decl.Type {
				add(p, "type differs from child output")
			}
			typ = decl.Type
			schema = child.Artifacts[typ].Schema
			if len(decl.Schema) > 0 {
				schema = decl.Schema
			}
		} else {
			cs, ok := child.Stages[m.Stage]
			if !ok {
				add(p, "unknown child stage "+m.Stage)
				continue
			}
			if typ == "" {
				typ = cs.Output
			}
			if !StageProducesType(cs, typ) {
				add(p, "type is not produced by child stage")
			}
			schema = child.Artifacts[typ].Schema
		}
		if m.As != "" {
			target, ok := parent.Artifacts[m.As]
			if !ok {
				add(p+".as", "unknown parent artifact "+m.As)
			} else if !SchemaSatisfies(schema, target.Schema) {
				add(p+".as", "child output schema does not prove compatibility with "+m.As)
			}
		}
	}
	return out
}

// SchemaSatisfies conservatively proves structural schema containment. Unknown
// target constraints require the same source constraint, never a sampled value.
func SchemaSatisfies(source, target json.RawMessage) bool {
	var a, b any
	if json.Unmarshal(source, &a) != nil || json.Unmarshal(target, &b) != nil {
		return false
	}
	if sourceObject, ok := a.(map[string]any); ok {
		if finite, ok := sourceObject["enum"].([]any); ok && len(finite) > 0 {
			for _, value := range finite {
				raw, _ := json.Marshal(value)
				if ValidateArtifact(target, raw) != nil {
					return false
				}
			}
			return true
		}
	}
	return schemaContains(a, b)
}
func schemaContains(a, b any) bool {
	if reflect.DeepEqual(a, b) || b == true {
		return true
	}
	if a == false {
		return true
	}
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if !aok || !bok {
		return false
	}
	for k, want := range bm {
		got, has := am[k]
		switch k {
		case "$schema", "title", "description", "default", "examples":
			continue
		case "type":
			if !schemaTypesContained(got, want) {
				return false
			}
		case "additionalProperties":
			if want == true {
				continue
			}
			if want == false {
				if got != false {
					return false
				}
				ap, _ := am["properties"].(map[string]any)
				bp, _ := bm["properties"].(map[string]any)
				for name := range ap {
					if _, ok := bp[name]; !ok {
						return false
					}
				}
			} else if !reflect.DeepEqual(got, want) {
				return false
			}
		case "required":
			source, _ := am[k].([]any)
			targetRequired, ok := want.([]any)
			if !ok {
				return false
			}
			for _, v := range targetRequired {
				found := false
				for _, s := range source {
					found = found || s == v
				}
				if !found {
					return false
				}
			}
		case "properties":
			wp, _ := want.(map[string]any)
			gp, _ := got.(map[string]any)
			for n, ws := range wp {
				if gs, ok := gp[n]; ok {
					if !schemaContains(gs, ws) {
						return false
					}
				} else if am["additionalProperties"] != false {
					return false
				}
			}
		case "items":
			if !has || !schemaContains(got, want) {
				return false
			}
		case "minimum", "minLength", "minItems", "minProperties":
			gv, ok := got.(float64)
			wv, wok := want.(float64)
			if !ok || !wok || gv < wv {
				return false
			}
		case "maximum", "maxLength", "maxItems", "maxProperties":
			gv, ok := got.(float64)
			wv, wok := want.(float64)
			if !ok || !wok || gv > wv {
				return false
			}
		case "enum":
			gv, ok := got.([]any)
			wv, wok := want.([]any)
			if !ok || !wok {
				return false
			}
			for _, v := range gv {
				found := false
				for _, w := range wv {
					found = found || reflect.DeepEqual(v, w)
				}
				if !found {
					return false
				}
			}
		default:
			if !has || !reflect.DeepEqual(got, want) {
				return false
			}
		}
	}
	return true
}

func projectionSchema(d Document, p Projection, item json.RawMessage) (json.RawMessage, bool) {
	if len(p.Literal) > 0 {
		var value any
		if json.Unmarshal(p.Literal, &value) != nil {
			return nil, false
		}
		return literalSchema(value), true
	}
	if p.Object != nil {
		properties := map[string]json.RawMessage{}
		required := []string{}
		for name, member := range p.Object {
			schema, ok := projectionSchema(d, member, item)
			if !ok {
				return nil, false
			}
			properties[name] = schema
			required = append(required, name)
		}
		raw, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false})
		return raw, true
	}
	if p.Lookup != nil {
		var common json.RawMessage
		for _, raw := range p.Lookup.Table {
			var value any
			if json.Unmarshal(raw, &value) != nil {
				return nil, false
			}
			schema := literalSchema(value)
			if common == nil {
				common = schema
			} else {
				var a, b map[string]any
				_ = json.Unmarshal(common, &a)
				_ = json.Unmarshal(schema, &b)
				if a["type"] != b["type"] {
					return nil, false
				}
				delete(a, "enum")
				common, _ = json.Marshal(a)
			}
		}
		return common, len(common) > 0
	}
	var raw json.RawMessage
	switch {
	case p.Source == "item":
		raw = item
	case p.Input != "" && d.Signature != nil:
		raw = d.Signature.Inputs[p.Input]
	case p.Type != "":
		raw = d.Artifacts[p.Type].Schema
	case p.Stage != "":
		raw = d.Artifacts[d.Stages[p.Stage].Output].Schema
	default:
		return nil, false
	}
	if len(raw) == 0 {
		return nil, false
	}
	selectPath := func(raw json.RawMessage, path string) (json.RawMessage, bool) {
		for _, part := range strings.Split(path, ".") {
			var s struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if json.Unmarshal(raw, &s) != nil {
				return nil, false
			}
			var ok bool
			raw, ok = s.Properties[part]
			if !ok {
				return nil, false
			}
		}
		return raw, true
	}
	var ok bool
	if p.Field != "" {
		raw, ok = selectPath(raw, p.Field)
		if !ok {
			if len(p.Default) > 0 {
				var v any
				_ = json.Unmarshal(p.Default, &v)
				return literalSchema(v), true
			}
			return nil, false
		}
	}
	if p.Index != nil {
		var s struct {
			Items json.RawMessage `json:"items"`
		}
		if json.Unmarshal(raw, &s) != nil || len(s.Items) == 0 {
			return nil, false
		}
		raw = s.Items
	}
	if p.Then != "" {
		raw, ok = selectPath(raw, p.Then)
		if !ok {
			return nil, false
		}
	}
	return raw, true
}
func literalSchema(value any) json.RawMessage {
	typ := "null"
	switch value.(type) {
	case string:
		typ = "string"
	case bool:
		typ = "boolean"
	case float64:
		typ = "number"
	case []any:
		typ = "array"
	case map[string]any:
		typ = "object"
	}
	raw, _ := json.Marshal(map[string]any{"type": typ, "enum": []any{value}})
	return raw
}

func schemaTypesContained(source, target any) bool {
	list := func(v any) []any {
		if a, ok := v.([]any); ok {
			return a
		}
		return []any{v}
	}
	for _, s := range list(source) {
		ok := false
		for _, t := range list(target) {
			ok = ok || s == t || (s == "integer" && t == "number")
		}
		if !ok {
			return false
		}
	}
	return true
}
