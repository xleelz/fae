package workflow

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Signature is the workflow's public, pinned data contract. Inputs are JSON
// Schemas (their default keyword makes them optional); outputs name an
// artifact type and may additionally constrain its schema.
type Signature struct {
	Inputs  map[string]json.RawMessage `json:"inputs,omitempty"`
	Outputs map[string]SignatureOutput `json:"outputs,omitempty"`
}
type SignatureOutput struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

func (o *SignatureOutput) UnmarshalJSON(raw []byte) error {
	if len(raw) > 0 && raw[0] == '"' {
		return json.Unmarshal(raw, &o.Type)
	}
	type plain SignatureOutput
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode((*plain)(o))
}

// Recursion is a finite run-tree budget, pinned with each revision.
type Recursion struct {
	MaxDepth            int `json:"max_depth"`
	MaxDescendants      int `json:"max_descendants"`
	MaxChildrenPerVisit int `json:"max_children_per_visit"`
}

const DefaultRecursionCeiling = 8

func RecursionCeiling(d Document) int {
	if d.ValidationRecursionCeiling > 0 {
		return d.ValidationRecursionCeiling
	}
	return DefaultRecursionCeiling
}

func (r *WorkflowRef) UnmarshalJSON(raw []byte) error {
	if string(raw) == `"self"` {
		*r = WorkflowRef{ID: "self"}
		return nil
	}
	type plain WorkflowRef
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode((*plain)(r))
}
func signatureNames[T any](m map[string]T) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// BindInputs reports every missing, unknown and invalid input in one refusal.
// Returned values are independently decoded canonical JSON, never caller maps.
func BindInputs(d Document, supplied map[string]any) (map[string]any, []Issue) {
	bound := map[string]any{}
	issues := []Issue{}
	declared := map[string]json.RawMessage{}
	if d.Signature != nil {
		declared = d.Signature.Inputs
	}
	for _, name := range signatureNames(supplied) {
		if _, ok := declared[name]; !ok {
			issues = append(issues, Issue{"inputs." + name, "unknown input"})
		}
	}
	for _, name := range signatureNames(declared) {
		schema := declared[name]
		value, ok := supplied[name]
		if !ok {
			var s map[string]json.RawMessage
			_ = json.Unmarshal(schema, &s)
			if raw, has := s["default"]; has {
				dec := json.NewDecoder(bytes.NewReader(raw))
				dec.UseNumber()
				_ = dec.Decode(&value)
				ok = true
			}
		}
		if !ok {
			issues = append(issues, Issue{"inputs." + name, "required input missing"})
			continue
		}
		raw, err := json.Marshal(value)
		if err == nil {
			err = ValidateArtifact(schema, raw)
		}
		if err != nil {
			issues = append(issues, Issue{"inputs." + name, err.Error()})
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		_ = dec.Decode(&value)
		bound[name] = value
	}
	return bound, issues
}

// InputFields is how JSON values are addressed by the evidence language:
// objects expose their fields directly, other values use the value field.
func InputFields(value any) map[string]any {
	if m, ok := value.(map[string]any); ok {
		return m
	}
	return map[string]any{"value": value}
}
func SyntheticArtifact(d Document, name string) (ArtifactType, bool) {
	if name == "engine:run_stats" {
		return ArtifactType{Version: 1, Schema: json.RawMessage(runStatsSchema)}, true
	}
	if name == "tree" {
		return ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}, true
	}
	if !strings.HasPrefix(name, "input:") || d.Signature == nil {
		return ArtifactType{}, false
	}
	schema, ok := d.Signature.Inputs[strings.TrimPrefix(name, "input:")]
	if !ok {
		return ArtifactType{}, false
	}
	var s map[string]any
	_ = json.Unmarshal(schema, &s)
	if s["type"] != "object" {
		schema, _ = json.Marshal(map[string]any{"type": "object", "required": []string{"value"}, "properties": map[string]any{"value": json.RawMessage(schema)}, "additionalProperties": false})
	}
	return ArtifactType{Version: 1, Schema: schema}, true
}
func ArtifactDeclaration(d Document, name string) (ArtifactType, bool) {
	if name == "failure" {
		return ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","required":["stage","visit","attempt","reason","error"],"properties":{"stage":{"type":"string"},"visit":{"type":"integer"},"attempt":{"type":"integer"},"reason":{"type":"string"},"error":{"type":"string"}}`)}, true
	}
	if a, ok := d.Artifacts[name]; ok {
		return a, true
	}
	return SyntheticArtifact(d, name)
}
func ProducerDeclaration(d Document, name string) (Stage, bool) {
	if source, ok := strings.CutPrefix(name, "failure:"); ok {
		if s, exists := d.Stages[source]; exists && FailureTarget(d, source) != "" {
			return Stage{Primitive: s.Primitive, Output: "failure"}, true
		}
	}
	if s, ok := d.Stages[name]; ok {
		return s, true
	}
	if _, ok := SyntheticArtifact(d, name); ok {
		return Stage{Primitive: AgentTurn, Output: name}, true
	}
	return Stage{}, false
}
func validateSignature(d Document, bad func(string, string)) {
	if d.Recursion != nil {
		r := d.Recursion
		if r.MaxDepth < 1 || r.MaxDepth > RecursionCeiling(d) {
			bad("recursion.max_depth", fmt.Sprintf("must be 1..%d", RecursionCeiling(d)))
		}
		if r.MaxDescendants < 0 || r.MaxDescendants > 100000 {
			bad("recursion.max_descendants", "must be 0..100000")
		}
		if r.MaxChildrenPerVisit < 1 || r.MaxChildrenPerVisit > 256 {
			bad("recursion.max_children_per_visit", "must be 1..256")
		}
	}
	if d.Signature == nil {
		return
	}
	for _, name := range signatureNames(d.Signature.Inputs) {
		schema := d.Signature.Inputs[name]
		p := "signature.inputs." + name
		if !identifier(name, 64) {
			bad(p, "invalid input name")
		}
		if _, err := compileArtifact(schema); err != nil {
			bad(p, err.Error())
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(schema, &fields) == nil {
			if v, ok := fields["default"]; ok {
				if err := ValidateArtifact(schema, v); err != nil {
					bad(p+".default", err.Error())
				}
			}
		}
	}
	for _, name := range signatureNames(d.Signature.Outputs) {
		o := d.Signature.Outputs[name]
		p := "signature.outputs." + name
		if !identifier(name, 64) {
			bad(p, "invalid output name")
		}
		if _, ok := d.Artifacts[o.Type]; !ok {
			bad(p+".type", "unknown artifact type "+o.Type)
		}
		produced := false
		for _, s := range d.Stages {
			produced = produced || StageProducesType(s, o.Type)
		}
		if !produced {
			bad(p+".type", "no stage produces declared output "+o.Type)
		}
		if len(o.Schema) > 0 {
			if _, err := compileArtifact(o.Schema); err != nil {
				bad(p+".schema", err.Error())
			}
		}
	}
}

// StageProducesType includes explicitly published child output aliases.
func StageProducesType(s Stage, typ string) bool {
	if s.Output == typ {
		return true
	}
	if s.Primitive == SubWorkflow {
		for _, m := range s.OutputMap {
			if m.As == typ {
				return true
			}
		}
	}
	return false
}
