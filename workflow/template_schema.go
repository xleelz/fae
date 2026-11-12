package workflow

import (
	"encoding/json"
	"strings"
)

func validatePresenceSyntax(stage, text, label string, validate func(string, string, string), bad func(string, string)) {
	parts, err := ParseBriefTemplate(text)
	if err != nil {
		bad(label, err.Error())
		return
	}
	var visit func([]TemplatePart)
	visit = func(parts []TemplatePart) {
		for _, part := range parts {
			if part.Kind == "present" || part.Kind == "absent" {
				validate(label, stage, "{{"+part.Name+"?}}")
				visit(part.Children)
			}
		}
	}
	visit(parts)
}

func ReferenceMayBeAbsent(d Document, stage, name string) (bool, bool) {
	parts := strings.Split(name, ".")
	var raw json.RawMessage
	var path []string
	switch parts[0] {
	case "input":
		if len(parts) < 2 || d.Signature == nil {
			return false, false
		}
		raw = d.Signature.Inputs[parts[1]]
		path = parts[2:]
	case "artifact":
		if len(parts) < 2 {
			return false, false
		}
		typ, ok := ArtifactDeclaration(d, parts[1])
		if !ok {
			return false, false
		}
		raw = typ.Schema
		path = parts[2:]
	case "item", "siblings":
		schema, ok := branchItemSchema(d, stage, map[string]bool{})
		if !ok {
			return false, false
		}
		raw, _ = json.Marshal(schema)
		path = parts[1:]
	default:
		return false, false
	}
	if len(raw) == 0 {
		return false, false
	}
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return false, false
	}
	return nullableSchemaPath(root, root, path, 0), true
}

func nullableSchemaPath(schema, root map[string]any, path []string, depth int) bool {
	if depth > 64 {
		return true
	}
	if ref, ok := schema["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
		var value any = root
		for _, key := range strings.Split(ref[2:], "/") {
			m, ok := value.(map[string]any)
			if !ok {
				return true
			}
			value = m[strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")]
		}
		m, ok := value.(map[string]any)
		if !ok {
			return true
		}
		return nullableSchemaPath(m, root, path, depth+1)
	}
	// Validate null against the selected fragment, retaining its local definitions.
	// This handles unions and missing type constraints as JSON Schema specifies.
	fragment := make(map[string]any, len(schema)+2)
	for key, value := range schema {
		fragment[key] = value
	}
	if _, ok := fragment["$defs"]; !ok {
		if defs, exists := root["$defs"]; exists {
			fragment["$defs"] = defs
		}
	}
	if _, ok := fragment["definitions"]; !ok {
		if defs, exists := root["definitions"]; exists {
			fragment["definitions"] = defs
		}
	}
	raw, _ := json.Marshal(fragment)
	if ValidateArtifact(raw, json.RawMessage("null")) == nil {
		return true
	}
	for _, keyword := range []string{"anyOf", "oneOf"} {
		if alternatives, ok := schema[keyword].([]any); ok {
			for _, candidate := range alternatives {
				if m, ok := candidate.(map[string]any); ok && schemaHasReferencePath(m, root, path, depth+1) && nullableSchemaPath(m, root, path, depth+1) {
					return true
				}
			}
		}
	}
	if len(path) == 0 {
		return false
	}
	props, _ := schema["properties"].(map[string]any)
	next, ok := props[path[0]].(map[string]any)
	if !ok {
		return false
	}
	required := false
	if keys, ok := schema["required"].([]any); ok {
		for _, key := range keys {
			required = required || key == path[0]
		}
	}
	if !required {
		return true
	}
	return nullableSchemaPath(next, root, path[1:], depth+1)
}

// LoopEncloses excludes a post-loop stage that cannot return to the loop.
func LoopEncloses(d Document, loop, stage string) bool {
	if d.Stages[loop].Primitive != Loop {
		return false
	}
	edges := map[string][]string{}
	for _, edge := range d.Edges {
		edges[edge.From] = append(edges[edge.From], edge.To)
	}
	reaches := func(from, to string) bool {
		queue := []string{from}
		seen := map[string]bool{}
		for len(queue) > 0 {
			n := queue[0]
			queue = queue[1:]
			if n == to {
				return true
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			queue = append(queue, edges[n]...)
		}
		return false
	}
	return reaches(loop, stage) && reaches(stage, loop)
}
