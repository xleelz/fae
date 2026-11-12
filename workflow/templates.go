package workflow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var typedReference = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_.:-]*\??)\}\}`)
var referenceName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.:-]*$`)

func validateBriefTemplates(d Document, bad func(string, string)) {
	validate := func(label, stage, text string) {
		for _, match := range typedReference.FindAllStringSubmatch(text, -1) {
			name := strings.TrimSuffix(match[1], "?")
			optional := strings.HasSuffix(match[1], "?")
			if !optional {
				if nullable, known := ReferenceMayBeAbsent(d, stage, name); known && nullable {
					bad(label, "reference may be null or absent; use {{"+name+"?}}")
				}
			}
			if name == "visit" {
				continue
			}
			if strings.HasPrefix(name, "loop.") {
				parts := strings.Split(name, ".")
				if len(parts) != 3 || parts[2] != "iteration" || !LoopEncloses(d, parts[1], stage) {
					bad(label, "loop reference does not enclose stage: "+name)
				}
				continue
			}
			if name == "task_id" || name == "stage" || name == "output_dir" || name == "staging_dir" {
				if name == "staging_dir" && !d.Staging {
					bad(label, "staging_dir needs a workflow staging area")
				}
				continue
			}
			parts := strings.Split(name, ".")
			switch parts[0] {
			case "input":
				if len(parts) < 2 || d.Signature == nil || d.Signature.Inputs[parts[1]] == nil {
					bad(label, "reference names undeclared input "+name)
				} else if len(parts) > 2 && !schemaReferencePath(d.Signature.Inputs[parts[1]], parts[2:]) {
					bad(label, "input reference path outside schema: "+name)
				}
			case "artifact":
				if len(parts) < 2 {
					bad(label, "artifact reference needs type")
				} else if typ, ok := ArtifactDeclaration(d, parts[1]); !ok {
					bad(label, "reference names undeclared artifact "+name)
				} else if !artifactTemplateBound(d.Stages[stage], parts[1]) {
					bad(label, "artifact reference is not bound by the stage: "+name)
				} else if len(parts) > 2 && !schemaReferencePath(typ.Schema, parts[2:]) {
					bad(label, "artifact reference path outside schema: "+name)
				}
			case "failure":
				entered := false
				for from := range d.Stages {
					if FailureHandlerOnly(d, from, stage) {
						entered = true
					}
				}
				if !entered {
					bad(label, "failure reference outside a failure handler")
				}
			case "item", "siblings":
				schema, ok := branchItemSchema(d, stage, map[string]bool{})
				if !ok {
					bad(label, "item reference outside a typed dynamic FanOut")
				} else if len(parts) > 1 {
					raw, _ := json.Marshal(schema)
					if !schemaReferencePath(raw, parts[1:]) {
						bad(label, "item reference path outside branch element schema: "+name)
					}
				}
			case "tree":
			default:
				bad(label, "unknown typed reference "+name)
			}
		}
	}
	for id, s := range d.Stages {
		if s.Reads != nil {
			for _, pattern := range append(append([]string{}, s.Reads.Allow...), s.Reads.Deny...) {
				validate("stages."+id+".reads", id, pattern)
			}
		}
		for i, arg := range s.Command {
			if HasRuntimeReferences([]string{arg}) {
				validate("stages."+id+".command["+fmt.Sprint(i)+"]", id, arg)
			}
		}
		validate("stages."+id+".brief", id, s.Brief)
		validatePresenceSyntax(id, s.Brief, "stages."+id+".brief", validate, bad)
		for _, pattern := range s.Writes {
			validate("stages."+id+".writes", id, pattern)
		}
		if s.Generate != nil {
			for _, pattern := range s.Generate.Writes {
				validate("stages."+id+".generate.writes", id, pattern)
			}
		}
		for _, role := range stageRoles(s) {
			if policy := d.Roles[role].Reads; policy != nil {
				for _, pattern := range append(append([]string{}, policy.Allow...), policy.Deny...) {
					validate("roles."+role+".reads", id, pattern)
				}
			}
			for phase, text := range d.Roles[role].Briefs {
				validate("roles."+role+".briefs."+phase, id, text)
				validatePresenceSyntax(id, text, "roles."+role+".briefs."+phase, validate, bad)
			}
			for _, pattern := range d.Roles[role].Writes {
				validate("roles."+role+".writes", id, pattern)
			}
		}
	}
}
func schemaReferencePath(raw json.RawMessage, path []string) bool {
	var root map[string]any
	if json.Unmarshal(raw, &root) != nil {
		return false
	}
	return schemaHasReferencePath(root, root, path, 0)
}
func schemaHasReferencePath(schema, root map[string]any, path []string, depth int) bool {
	if depth > 64 {
		return false
	}
	if len(path) == 0 {
		return true
	}
	if ref, ok := schema["$ref"].(string); ok && strings.HasPrefix(ref, "#/") {
		var value any = root
		for _, part := range strings.Split(ref[2:], "/") {
			m, ok := value.(map[string]any)
			if !ok {
				return false
			}
			value = m[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
		}
		if target, ok := value.(map[string]any); ok && schemaHasReferencePath(target, root, path, depth+1) {
			return true
		}
	}
	for _, keyword := range []string{"anyOf", "oneOf", "allOf"} {
		if alternatives, ok := schema[keyword].([]any); ok {
			for _, candidate := range alternatives {
				if m, ok := candidate.(map[string]any); ok && schemaHasReferencePath(m, root, path, depth+1) {
					return true
				}
			}
		}
	}
	properties, _ := schema["properties"].(map[string]any)
	child, ok := properties[path[0]].(map[string]any)
	return ok && schemaHasReferencePath(child, root, path[1:], depth+1)
}

// The closest lexical fan supplies item; an enclosing sibling's declaration
// must never validate a reference in an unrelated stage.
func branchItemSchema(d Document, stage string, seen map[string]bool) (map[string]any, bool) {
	selected := ""
	size := int(^uint(0) >> 1)
	for id, fan := range d.Stages {
		if fan.Primitive != FanOut {
			continue
		}
		members := BranchOf(d, id)
		if _, ok := members[stage]; ok && len(members) < size {
			selected = id
			size = len(members)
		}
	}
	if selected == "" || seen[selected] {
		return nil, false
	}
	seen[selected] = true
	fan := d.Stages[selected]
	bf := fan.BranchesFrom
	if bf == nil {
		return nil, false
	}
	projection := Projection{Stage: bf.Stage, Type: bf.Type, Input: bf.Input, Field: bf.Field}
	if bf.Projection != nil {
		projection = *bf.Projection
	}
	var schema map[string]any
	var raw json.RawMessage
	if projection.Input != "" && d.Signature != nil {
		raw = d.Signature.Inputs[projection.Input]
	} else if projection.Type != "" {
		a, ok := ArtifactDeclaration(d, projection.Type)
		if ok {
			raw = a.Schema
		}
	} else if projection.Stage != "" {
		producer, ok := ProducerDeclaration(d, projection.Stage)
		if ok {
			a, exists := ArtifactDeclaration(d, producer.Output)
			if exists {
				raw = a.Schema
			}
		}
	} else if projection.Source == "item" {
		var ok bool
		schema, ok = branchItemSchema(d, selected, seen)
		if !ok {
			return nil, false
		}
	}
	if schema == nil {
		if json.Unmarshal(raw, &schema) != nil {
			return nil, false
		}
	}
	if projection.Field != "" {
		for _, part := range strings.Split(projection.Field, ".") {
			props, ok := schema["properties"].(map[string]any)
			if !ok {
				return nil, false
			}
			next, ok := props[part].(map[string]any)
			if !ok {
				return nil, false
			}
			schema = next
		}
	}
	items, ok := schema["items"].(map[string]any)
	return items, ok
}

func artifactTemplateBound(stage Stage, typ string) bool {
	for _, input := range stage.Inputs {
		if input.Type == typ {
			return true
		}
	}
	return typ == "engine:run_stats" && stage.RunStats != ""
}
