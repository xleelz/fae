package workflow

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// ValidationError lists every structural and semantic issue of a refused
// document, each with its document path (FAE-WF-003, FAE-WF-004).
type ValidationError struct{ Issues []Issue }

func (e *ValidationError) Error() string { return fmt.Sprintf("invalid workflow: %v", e.Issues) }

var rawMessage = reflect.TypeOf(json.RawMessage(nil))

// structural compares decoded JSON with the Go shape of t, reporting unknown
// keys and wrong JSON types at their paths. It never stops at the first error.
func structural(v any, t reflect.Type, path string, bad func(string, string)) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(Projection{}) {
		if _, ok := v.(string); ok {
			return
		}
	}
	if t == reflect.TypeOf(WorkflowRef{}) && v == "self" {
		return
	}
	if t == reflect.TypeOf(SignatureOutput{}) {
		if _, ok := v.(string); ok {
			return
		}
	}
	if v == nil || t == rawMessage || t.Kind() == reflect.Interface {
		return
	}
	child := func(key string) string {
		if path == "" {
			return key
		}
		return path + "." + key
	}
	switch t.Kind() {
	case reflect.Struct:
		m, ok := v.(map[string]any)
		if !ok {
			bad(path, "expected object")
			return
		}
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" || !f.IsExported() {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fields[name] = f.Type
		}
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			ft, ok := fields[k]
			if !ok {
				bad(child(k), "unknown field")
				continue
			}
			structural(m[k], ft, child(k), bad)
		}
	case reflect.Map:
		m, ok := v.(map[string]any)
		if !ok {
			bad(path, "expected object")
			return
		}
		for k, e := range m {
			structural(e, t.Elem(), child(k), bad)
		}
	case reflect.Slice:
		list, ok := v.([]any)
		if !ok {
			bad(path, "expected array")
			return
		}
		for i, e := range list {
			structural(e, t.Elem(), fmt.Sprintf("%s[%d]", path, i), bad)
		}
	case reflect.String:
		if _, ok := v.(string); !ok {
			bad(path, "expected string")
		}
	case reflect.Bool:
		if _, ok := v.(bool); !ok {
			bad(path, "expected boolean")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, ok := v.(json.Number)
		if _, err := strconv.ParseInt(string(n), 10, t.Bits()); !ok || err != nil {
			bad(path, "expected integer")
		}
	case reflect.Float32, reflect.Float64:
		if _, ok := v.(json.Number); !ok {
			bad(path, "expected number")
		}
	}
}
func sortIssues(issues []Issue) []Issue {
	sort.Slice(issues, func(i, j int) bool {
		if issues[i].Path == issues[j].Path {
			return issues[i].Message < issues[j].Message
		}
		return issues[i].Path < issues[j].Path
	})
	unique := issues[:0]
	for _, i := range issues {
		if len(unique) == 0 || unique[len(unique)-1] != i {
			unique = append(unique, i)
		}
	}
	return unique
}
