package daemon

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var updateRPCSchema = flag.Bool("update-rpc-schema", false, "rewrite the generated parts of api/schema.json")

const rpcSchemaPath, rpcBaselinePath = "../api/schema.json", "../api/schema-v1-baseline.json"

// jsonSchemaFor describes a Go parameter type as the daemon's strict decoder
// reads it: every JSON field of the struct (embedded structs flattened), no
// others; nested messages are objects.
func jsonSchemaFor(t reflect.Type, top bool) map[string]any {
	nullable := false
	for t.Kind() == reflect.Pointer {
		t, nullable = t.Elem(), true
	}
	var s map[string]any
	switch {
	case t == reflect.TypeOf(json.RawMessage{}) || t.Kind() == reflect.Interface:
		return map[string]any{}
	case t == reflect.TypeOf(time.Time{}):
		s = map[string]any{"type": "string", "format": "date-time"}
	case t.Kind() == reflect.String:
		s = map[string]any{"type": "string"}
	case t.Kind() == reflect.Bool:
		s = map[string]any{"type": "boolean"}
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Uint64:
		s = map[string]any{"type": "integer"}
	case t.Kind() == reflect.Float32 || t.Kind() == reflect.Float64:
		s = map[string]any{"type": "number"}
	case t.Kind() == reflect.Slice || t.Kind() == reflect.Array:
		s, nullable = map[string]any{"type": "array", "items": jsonSchemaFor(t.Elem(), false)}, t.Kind() == reflect.Slice
	case t.Kind() == reflect.Map:
		s, nullable = map[string]any{"type": "object", "additionalProperties": jsonSchemaFor(t.Elem(), false)}, true
	case t.Kind() == reflect.Struct && top:
		props := map[string]any{}
		structFields(t, props)
		s = map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	default:
		s = map[string]any{"type": "object"}
	}
	if nullable {
		s["type"] = []any{s["type"], "null"}
	}
	return s
}
func structFields(t reflect.Type, props map[string]any) {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if tag == "-" || !f.IsExported() {
			continue
		}
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			structFields(f.Type, props)
			continue
		}
		if name == "" {
			name = f.Name
		}
		props[name] = jsonSchemaFor(f.Type, false)
	}
}
func generatedRPCSchema(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(rpcSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err = json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	defs := schema["$defs"].(map[string]any)
	for name := range defs {
		if strings.HasPrefix(name, "rpc") {
			delete(defs, name)
		}
	}
	all := []any{}
	for _, entry := range schema["allOf"].([]any) {
		if m, ok := entry.(map[string]any); !ok || m["$comment"] != "generated from daemon.rpcParams" {
			all = append(all, entry)
		}
	}
	methods := []string{}
	for method := range actionPermissions {
		methods = append(methods, method)
	}
	sort.Strings(methods)
	enum := []any{}
	for _, method := range methods {
		enum = append(enum, method)
		params, ok := rpcParams[method]
		if !ok {
			t.Fatalf("method %s has no rpcParams row", method)
		}
		defs["rpc"+method] = jsonSchemaFor(reflect.TypeOf(params), true)
		all = append(all, map[string]any{"$comment": "generated from daemon.rpcParams", "if": map[string]any{"properties": map[string]any{"method": map[string]any{"const": method}}, "required": []any{"method"}}, "then": map[string]any{"properties": map[string]any{"params": map[string]any{"$ref": "#/$defs/rpc" + method}}}})
	}
	schema["allOf"] = all
	schema["properties"].(map[string]any)["method"] = map[string]any{"type": "string", "enum": enum}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err = enc.Encode(schema); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// paramProperties collects a params schema's property names and required
// fields through $ref and allOf.
func paramProperties(root, s map[string]any, props, required map[string]bool) {
	if ref, ok := s["$ref"].(string); ok {
		node := any(root)
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			node = node.(map[string]any)[part]
		}
		paramProperties(root, node.(map[string]any), props, required)
	}
	for name := range mapOf(s["properties"]) {
		props[name] = true
	}
	for _, r := range listOf(s["required"]) {
		required[r.(string)] = true
	}
	for _, sub := range listOf(s["allOf"]) {
		paramProperties(root, sub.(map[string]any), props, required)
	}
}
func mapOf(v any) map[string]any { m, _ := v.(map[string]any); return m }
func listOf(v any) []any         { l, _ := v.([]any); return l }

// methodContracts is each method's described params: every property name and
// every required field, across the generated and hand-written entries.
func methodContracts(t *testing.T, raw []byte) (map[string]map[string][]string, map[string][]map[string]bool) {
	t.Helper()
	entries := map[string][]map[string]bool{}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string][]string{}
	for _, entry := range listOf(schema["allOf"]) {
		m := mapOf(entry)
		selector := mapOf(mapOf(mapOf(m["if"])["properties"])["method"])
		methods := listOf(selector["enum"])
		if c, ok := selector["const"]; ok {
			methods = append(methods, c)
		}
		params := mapOf(mapOf(mapOf(m["then"])["properties"])["params"])
		props, required := map[string]bool{}, map[string]bool{}
		paramProperties(schema, params, props, required)
		for _, method := range methods {
			entries[method.(string)] = append(entries[method.(string)], props)
			c := out[method.(string)]
			if c == nil {
				c = map[string][]string{"properties": {}, "required": {}}
			}
			for p := range props {
				if !slices.Contains(c["properties"], p) {
					c["properties"] = append(c["properties"], p)
				}
			}
			for r := range required {
				if !slices.Contains(c["required"], r) {
					c["required"] = append(c["required"], r)
				}
			}
			sort.Strings(c["properties"])
			sort.Strings(c["required"])
			out[method.(string)] = c
		}
	}
	return out, entries
}

// FAE-ARC-004: the checked-in RPC schema describes every method and each of
// its parameters, and evolves additively.
func TestRPCSchemaCoversEveryMethodAndEvolvesAdditively(t *testing.T) {
	want := generatedRPCSchema(t)
	if *updateRPCSchema {
		if err := os.WriteFile(rpcSchemaPath, want, 0644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(rpcSchemaPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("api/schema.json is stale; run go test ./daemon -run TestRPCSchema -update-rpc-schema and review the diff")
	}
	for method := range rpcParams {
		if _, ok := actionPermissions[method]; !ok {
			t.Errorf("rpcParams row %s names no RPC", method)
		}
	}
	var doc any
	if err = json.Unmarshal(got, &doc); err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	if err = c.AddResource("schema.json", doc); err != nil {
		t.Fatal(err)
	}
	compiled, err := c.Compile("schema.json")
	if err != nil {
		t.Fatal(err)
	}
	contracts, entries := methodContracts(t, got)
	for method, params := range rpcParams {
		// Every field the strict decoder accepts is described by every entry
		// that selects the method, hand-written ones included.
		fields := map[string]any{}
		structFields(reflect.TypeOf(params), fields)
		for i, props := range entries[method] {
			for field := range fields {
				if !props[field] {
					t.Errorf("%s: schema entry %d omits parameter %s", method, i, field)
				}
			}
		}
		generated, err := c.Compile("schema.json#/$defs/rpc" + method)
		if err != nil {
			t.Fatal(method, err)
		}
		encoded, _ := json.Marshal(params)
		var value any
		_ = json.Unmarshal(encoded, &value)
		if err = generated.Validate(value); err != nil {
			t.Errorf("%s: Go-encoded params fail the schema: %v", method, err)
		}
	}
	if err = compiled.Validate(map[string]any{"version": 1, "method": "NoSuchMethod"}); err == nil {
		t.Error("an unknown method validated")
	}
	if err = compiled.Validate(map[string]any{"version": 1, "method": "Leases", "params": map[string]any{"unknown": true}}); err == nil {
		t.Error("an undeclared parameter validated")
	}
	// Additive evolution: against the recorded version-1 contract, no method or
	// parameter disappears and no parameter becomes newly required, so a
	// request an older client built still validates.
	if _, err = os.Stat(rpcBaselinePath); os.IsNotExist(err) && *updateRPCSchema {
		raw, _ := json.MarshalIndent(contracts, "", "  ")
		if err = os.WriteFile(rpcBaselinePath, append(raw, '\n'), 0644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(rpcBaselinePath)
	if err != nil {
		t.Fatal(err)
	}
	var baseline map[string]map[string][]string
	if err = json.Unmarshal(raw, &baseline); err != nil {
		t.Fatal(err)
	}
	for method, old := range baseline {
		now, ok := contracts[method]
		if !ok {
			t.Errorf("method %s removed from version 1", method)
			continue
		}
		for _, p := range old["properties"] {
			if !slices.Contains(now["properties"], p) {
				t.Errorf("%s: parameter %s removed from version 1", method, p)
			}
		}
		for _, r := range now["required"] {
			if !slices.Contains(old["required"], r) {
				t.Errorf("%s: parameter %s newly required in version 1", method, r)
			}
		}
	}
}
