package proof

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

var fieldPath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*(\.[A-Za-z_][A-Za-z0-9_-]*)*$`)

func knownOutcome(o Outcome) bool {
	switch o {
	case Proved, Disproved, Unknown, Error, Unsupported, Skipped:
		return true
	}
	return false
}
func validateJSONFields(f *JSONFields) error {
	if f == nil {
		return fmt.Errorf("formal parser json requires json_fields")
	}
	for _, p := range []string{f.Outcome, f.Obligations, f.Verified, f.Failed, f.Unknown, f.Skipped} {
		if len(p) > 512 || !fieldPath.MatchString(p) || strings.Count(p, ".") >= 16 {
			return fmt.Errorf("formal JSON count and outcome paths must be bounded dotted object fields")
		}
	}
	if f.ToolVersion != "" && (len(f.ToolVersion) > 512 || !fieldPath.MatchString(f.ToolVersion) || strings.Count(f.ToolVersion, ".") >= 16) {
		return fmt.Errorf("formal JSON tool version path must be a bounded dotted object field")
	}
	if len(f.StatusMap) == 0 || len(f.StatusMap) > 64 {
		return fmt.Errorf("formal JSON status_map must explicitly map 1 to 64 statuses")
	}
	for k, v := range f.StatusMap {
		if len(k) == 0 || len(k) > 256 || !knownOutcome(v) {
			return fmt.Errorf("formal JSON status_map contains an invalid status or outcome")
		}
	}
	return nil
}
func lookupField(obj any, p string) (any, error) {
	for _, s := range strings.Split(p, ".") {
		m, ok := obj.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("formal JSON field %s crosses a non-object", p)
		}
		obj, ok = m[s]
		if !ok {
			return nil, fmt.Errorf("formal JSON field %s is missing", p)
		}
	}
	return obj, nil
}
func mappedJSON(r *Result, raw string, f *JSONFields) error {
	d := json.NewDecoder(strings.NewReader(raw))
	d.UseNumber()
	fields := 0
	obj, e := readJSONValue(d, 0, &fields)
	if e != nil {
		return fmt.Errorf("malformed formal JSON report: %w", e)
	}
	if _, ok := obj.(map[string]any); !ok {
		return fmt.Errorf("formal JSON report must be an object")
	}
	if _, e = d.Token(); e != io.EOF {
		return fmt.Errorf("formal JSON report must contain exactly one object")
	}
	status, e := lookupField(obj, f.Outcome)
	if e != nil {
		return e
	}
	s, ok := status.(string)
	if !ok {
		return fmt.Errorf("formal JSON outcome field must be a string")
	}
	r.Outcome, ok = f.StatusMap[s]
	if !ok {
		return fmt.Errorf("unmapped formal JSON status %q", s)
	}
	for _, v := range []struct {
		path string
		out  *int
	}{{f.Obligations, &r.Obligations}, {f.Verified, &r.Verified}, {f.Failed, &r.Failed}, {f.Unknown, &r.Unknown}, {f.Skipped, &r.Skipped}} {
		value, e := lookupField(obj, v.path)
		if e != nil {
			return e
		}
		n, ok := value.(json.Number)
		if !ok {
			return fmt.Errorf("formal JSON count %s must be an integer number", v.path)
		}
		count, e := n.Int64()
		if e != nil || count < 0 || count > 1000000000 {
			return fmt.Errorf("formal JSON count %s is outside its integer bound", v.path)
		}
		*v.out = int(count)
	}
	if f.ToolVersion != "" {
		value, e := lookupField(obj, f.ToolVersion)
		if e != nil {
			return e
		}
		version, ok := value.(string)
		if !ok || version == "" {
			return fmt.Errorf("formal JSON tool version must be a nonempty string")
		}
		if r.ToolVersion != "" && r.ToolVersion != version {
			return fmt.Errorf("conflicting observed tool versions")
		}
		r.ToolVersion = version
	}
	return nil
}

// readJSONValue retains exact integer tokens and rejects duplicate fields even
// outside the selected paths. Bounds apply to ignored report sections too.
func readJSONValue(d *json.Decoder, depth int, fields *int) (any, error) {
	if depth > 16 {
		return nil, fmt.Errorf("formal JSON nesting exceeds 16")
	}
	*fields++
	if *fields > 100000 {
		return nil, fmt.Errorf("formal JSON report exceeds item bound")
	}
	t, e := d.Token()
	if e != nil {
		return nil, e
	}
	delim, ok := t.(json.Delim)
	if !ok {
		return t, nil
	}
	switch delim {
	case '{':
		m := map[string]any{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return nil, e
			}
			k, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("invalid JSON object key")
			}
			if _, ok = m[k]; ok {
				return nil, fmt.Errorf("duplicate formal JSON object field %s", k)
			}
			v, e := readJSONValue(d, depth+1, fields)
			if e != nil {
				return nil, e
			}
			m[k] = v
		}
		end, e := d.Token()
		if e != nil || end != json.Delim('}') {
			return nil, fmt.Errorf("unterminated JSON object")
		}
		return m, nil
	case '[':
		var a []any
		for d.More() {
			v, e := readJSONValue(d, depth+1, fields)
			if e != nil {
				return nil, e
			}
			a = append(a, v)
		}
		end, e := d.Token()
		if e != nil || end != json.Delim(']') {
			return nil, fmt.Errorf("unterminated JSON array")
		}
		return a, nil
	}
	return nil, fmt.Errorf("unexpected JSON delimiter")
}
