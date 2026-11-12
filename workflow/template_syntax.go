package workflow

import (
	"fmt"
	"strings"
)

// TemplatePart separates authored instructions from quoted data and presence
// decisions. Data is never parsed as further template syntax.
type TemplatePart struct {
	Kind     string
	Name     string
	Optional bool
	Text     string
	Children []TemplatePart
}

func ParseBriefTemplate(text string) ([]TemplatePart, error) {
	steps := 0
	var parse func(int, string, int) ([]TemplatePart, int, error)
	parse = func(offset int, section string, depth int) ([]TemplatePart, int, error) {
		if depth > 64 {
			return nil, 0, fmt.Errorf("template nesting exceeds 64")
		}
		out := []TemplatePart{}
		for offset < len(text) {
			steps++
			if steps > 10000 {
				return nil, 0, fmt.Errorf("template exceeds 10000 parts")
			}
			start := strings.Index(text[offset:], "{{")
			if start < 0 {
				out = append(out, TemplatePart{Kind: "text", Text: text[offset:]})
				offset = len(text)
				break
			}
			start += offset
			if start > offset {
				out = append(out, TemplatePart{Kind: "text", Text: text[offset:start]})
			}
			end := strings.Index(text[start+2:], "}}")
			if end < 0 {
				return nil, 0, fmt.Errorf("unterminated template reference")
			}
			end += start + 2
			token := strings.TrimSpace(text[start+2 : end])
			offset = end + 2
			if strings.HasPrefix(token, "/") {
				if token != "/"+section {
					return nil, 0, fmt.Errorf("unmatched template section %s", token)
				}
				return out, offset, nil
			}
			kind, name := "reference", token
			for _, candidate := range []string{"present", "absent"} {
				if strings.HasPrefix(token, "#"+candidate+" ") {
					kind = candidate
					name = strings.TrimSpace(strings.TrimPrefix(token, "#"+candidate+" "))
				}
			}
			optional := strings.HasSuffix(name, "?")
			name = strings.TrimSuffix(name, "?")
			if !referenceName.MatchString(name) {
				return nil, 0, fmt.Errorf("invalid typed reference %q", token)
			}
			part := TemplatePart{Kind: kind, Name: name, Optional: optional}
			if kind != "reference" {
				var err error
				part.Children, offset, err = parse(offset, kind, depth+1)
				if err != nil {
					return nil, 0, err
				}
			}
			out = append(out, part)
		}
		if section != "" {
			return nil, 0, fmt.Errorf("unclosed template section %s", section)
		}
		return out, offset, nil
	}
	parts, _, err := parse(0, "", 0)
	return parts, err
}
