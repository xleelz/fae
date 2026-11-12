package workflow

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

func RuntimeReference(name string) bool {
	name = strings.TrimSuffix(name, "?")
	if name == "visit" || strings.HasPrefix(name, "loop.") {
		return true
	}
	return name == "output_dir" || name == "staging_dir" || strings.HasPrefix(name, "input.") || strings.HasPrefix(name, "artifact.") || strings.HasPrefix(name, "item.") || strings.HasPrefix(name, "tree.") || strings.HasPrefix(name, "failure.")
}
func HasRuntimeReferences(command []string) bool {
	for _, arg := range command {
		for _, m := range typedReference.FindAllStringSubmatch(arg, -1) {
			if RuntimeReference(m[1]) {
				return true
			}
		}
	}
	return false
}

// BindCommandValues substitutes typed runtime values inside one literal argv
// entry. It never invokes a shell and never re-interprets substituted text.
func BindCommandValues(s Stage, inputs []Evidence, values map[string]any) ([]string, error) {
	var out []string
	for i, arg := range s.Command {
		refs := typedReference.FindAllStringSubmatch(arg, -1)
		has := false
		for _, ref := range refs {
			has = has || RuntimeReference(ref[1])
		}
		if !has {
			single := s
			single.Command = []string{arg}
			args, err := BindCommand(single, inputs)
			if err != nil {
				return nil, err
			}
			out = append(out, args...)
			continue
		}
		if i == 0 {
			return nil, fmt.Errorf("runtime reference cannot choose executable")
		}
		var rendered strings.Builder
		offset := 0
		matches := typedReference.FindAllStringSubmatchIndex(arg, -1)
		for ri, ref := range refs {
			if !RuntimeReference(ref[1]) {
				return nil, fmt.Errorf("mixed runtime and legacy binding in one argument")
			}
			value, ok := values[strings.TrimSuffix(ref[1], "?")]
			if !ok {
				return nil, fmt.Errorf("unbound command reference %s", ref[1])
			}
			var literal string
			switch v := value.(type) {
			case string:
				literal = v
			case json.Number:
				literal = v.String()
			case float64, int, int64, bool:
				literal = fmt.Sprint(v)
			default:
				return nil, fmt.Errorf("command reference %s is not scalar", ref[1])
			}
			if strings.ContainsRune(literal, 0) {
				return nil, fmt.Errorf("command reference contains NUL")
			}
			match := matches[ri]
			rendered.WriteString(arg[offset:match[0]])
			rendered.WriteString(literal)
			offset = match[1]
		}
		rendered.WriteString(arg[offset:])
		out = append(out, rendered.String())
	}
	return out, nil
}

// The exact binding is rederived from checkpointed typed inputs by the engine;
// this shape check preserves literal prefixes/suffixes and argv boundaries.
func matchRuntimeArgument(template, value string) bool {
	if strings.ContainsRune(value, 0) {
		return false
	}
	pattern := "(?s)^"
	offset := 0
	for _, m := range typedReference.FindAllStringSubmatchIndex(template, -1) {
		if !RuntimeReference(template[m[2]:m[3]]) {
			return false
		}
		pattern += regexp.QuoteMeta(template[offset:m[0]]) + ".*"
		offset = m[1]
	}
	pattern += regexp.QuoteMeta(template[offset:]) + "$"
	return regexp.MustCompile(pattern).MatchString(value)
}
