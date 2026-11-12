package workflow

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
)

// SchemaValidFixture deterministically constructs a bounded rehearsal value.
// Validation is authoritative: unsupported constraints produce a refusal rather
// than an invalid record masquerading as generated evidence.
func SchemaValidFixture(schema json.RawMessage, seed int64) (json.RawMessage, error) {
	var root any
	if err := json.Unmarshal(schema, &root); err != nil {
		return nil, err
	}
	for n := int64(0); n < 32; n++ {
		v, err := fixtureValue(root, root, uint64(seed+n), 0)
		if err != nil {
			return nil, err
		}
		raw, err := json.Marshal(v)
		if err == nil && ValidateArtifact(schema, raw) == nil {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("schema has no supported deterministic rehearsal fixture; provide a literal scenario output")
}
func fixtureValue(node, root any, seed uint64, depth int) (any, error) {
	if depth > 16 {
		return nil, fmt.Errorf("rehearsal fixture exceeds schema depth16")
	}
	if b, ok := node.(bool); ok {
		if b {
			return nil, nil
		}
		return nil, fmt.Errorf("false schema has no fixture")
	}
	s, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid fixture schema")
	}
	if ref, ok := s["$ref"].(string); ok {
		if !strings.HasPrefix(ref, "#/") {
			return nil, fmt.Errorf("unsupported fixture reference %s", ref)
		}
		target := root
		for _, part := range strings.Split(ref[2:], "/") {
			m, ok := target.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid fixture reference %s", ref)
			}
			target = m[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
		}
		return fixtureValue(target, root, seed, depth+1)
	}
	if value, ok := s["const"]; ok {
		return value, nil
	}
	if values, ok := s["enum"].([]any); ok && len(values) > 0 {
		return values[int(seed%uint64(len(values)))], nil
	}
	if value, ok := s["default"]; ok {
		return value, nil
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		if choices, ok := s[key].([]any); ok && len(choices) > 0 {
			return fixtureValue(choices[int(seed%uint64(len(choices)))], root, seed, depth+1)
		}
	}
	kind, _ := s["type"].(string)
	if kinds, ok := s["type"].([]any); ok && len(kinds) > 0 {
		kind, _ = kinds[int(seed%uint64(len(kinds)))].(string)
	}
	if kind == "" {
		if _, ok := s["properties"]; ok {
			kind = "object"
		} else if _, ok := s["items"]; ok {
			kind = "array"
		} else {
			kind = "object"
		}
	}
	number := func(key string, fallback float64) float64 {
		if n, ok := s[key].(float64); ok {
			return n
		}
		return fallback
	}
	switch kind {
	case "null":
		return nil, nil
	case "boolean":
		return seed%2 == 0, nil
	case "string":
		value := fmt.Sprintf("generated-%x", seed)
		if format, _ := s["format"].(string); format != "" {
			switch format {
			case "date-time":
				value = "2026-01-01T00:00:00Z"
			case "date":
				value = "2026-01-01"
			case "time":
				value = "00:00:00Z"
			case "email":
				value = "generated@example.invalid"
			case "uri", "uri-reference":
				value = "https://example.invalid/generated"
			case "uuid":
				value = "00000000-0000-4000-8000-000000000000"
			case "hostname":
				value = "example.invalid"
			case "ipv4":
				value = "192.0.2.1"
			case "ipv6":
				value = "2001:db8::1"
			}
		}
		minimum, maximum := number("minLength", 0), number("maxLength", fixtureStringLimit)
		if minimum < 0 || minimum > fixtureStringLimit || maximum < minimum {
			return nil, fmt.Errorf("fixture string bounds unsupported")
		}
		minLength, maxLength := int(minimum), int(math.Min(maximum, fixtureStringLimit))
		if pattern, ok := s["pattern"].(string); ok {
			return patternFixture(pattern, seed, minLength, maxLength)
		}
		runes := []rune(value)
		for len(runes) < minLength {
			runes = append(runes, 'x')
		}
		if len(runes) > maxLength {
			runes = runes[:maxLength]
		}
		return string(runes), nil
	case "number", "integer":
		lower, upper := number("minimum", 0), number("maximum", math.MaxFloat64)
		if ex, ok := s["exclusiveMinimum"].(float64); ok {
			lower = math.Max(lower, ex+1)
		}
		if ex, ok := s["exclusiveMaximum"].(float64); ok {
			upper = math.Min(upper, ex-1)
		}
		value := lower
		if kind == "integer" {
			value = math.Ceil(value)
		}
		if mult := number("multipleOf", 0); mult > 0 {
			value = math.Ceil(value/mult) * mult
		}
		if value > upper {
			value = upper
		}
		return value, nil
	case "array":
		count := int(number("minItems", 0))
		if count > 256 {
			return nil, fmt.Errorf("fixture array exceeds256 items")
		}
		out := []any{}
		item := s["items"]
		if item == nil {
			item = map[string]any{}
		}
		prefixes, _ := s["prefixItems"].([]any)
		for i := 0; i < count; i++ {
			spec := item
			if i < len(prefixes) {
				spec = prefixes[i]
			}
			v, err := fixtureValue(spec, root, seed+uint64(i)+1, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case "object":
		out := map[string]any{}
		properties, _ := s["properties"].(map[string]any)
		keys := []string{}
		required, _ := s["required"].([]any)
		for _, k := range required {
			if key, ok := k.(string); ok {
				keys = append(keys, key)
			}
		}
		sort.Strings(keys)
		for i, key := range keys {
			spec := properties[key]
			if spec == nil {
				spec = map[string]any{}
			}
			v, err := fixtureValue(spec, root, seed+uint64(i)+1, depth+1)
			if err != nil {
				return nil, err
			}
			out[key] = v
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported fixture type %s", kind)
}
