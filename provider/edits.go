package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// maxEditBytes bounds one edited file's resulting content.
const maxEditBytes = 1 << 20

// Edit is one proposed file change in exactly one form: whole-file Content,
// or Replacements applied in order to the file's current content. Only
// whole-file content can create a file.
type Edit struct {
	Path         string        `json:"path"`
	Content      *string       `json:"content"`
	Replacements []Replacement `json:"replacements"`
}

// Replacement swaps Old, which must occur exactly once in the file at its
// turn, for New.
type Replacement struct {
	Old string `json:"old"`
	New string `json:"new"`
}

type Response struct {
	Output json.RawMessage `json:"output"`
	Edits  []Edit          `json:"edits"`
}

// responseSchema is the provider's structured-output schema: the role output
// plus edits. Codex strict mode needs every property required and no
// additional properties, so each edit has one shape (content a string or
// null, replacements possibly empty) and the bridge checks that exactly one
// form is used. Nullability uses anyOf, which both CLIs' strict schema
// subsets support.
func responseSchema(output json.RawMessage) []byte {
	replacement := map[string]any{"type": "object", "properties": map[string]any{"old": map[string]string{"type": "string"}, "new": map[string]string{"type": "string"}}, "required": []string{"old", "new"}, "additionalProperties": false}
	edit := map[string]any{"type": "object", "properties": map[string]any{"path": map[string]string{"type": "string"}, "content": map[string]any{"anyOf": []any{map[string]string{"type": "string"}, map[string]string{"type": "null"}}}, "replacements": map[string]any{"type": "array", "items": replacement}}, "required": []string{"path", "content", "replacements"}, "additionalProperties": false}
	raw, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{"output": output, "edits": map[string]any{"type": "array", "items": edit}}, "required": []string{"output", "edits"}, "additionalProperties": false})
	return raw
}

// resolveEdits returns each edit's resulting content. Replacements apply in
// order to the content read for this turn; each old text must occur exactly
// once after the earlier replacements of the same edit. Any failure refuses
// the whole response, so nothing is applied.
func resolveEdits(edits []Edit, current map[string][]byte, exists map[string]bool) ([][]byte, error) {
	out := make([][]byte, len(edits))
	for i, edit := range edits {
		if edit.Content != nil {
			out[i] = []byte(*edit.Content)
			continue
		}
		if !exists[edit.Path] {
			return nil, fmt.Errorf("edit %q: replacements need an existing file; create a file with whole-file content", edit.Path)
		}
		text := string(current[edit.Path])
		for j, r := range edit.Replacements {
			if r.Old == "" {
				return nil, fmt.Errorf("edit %q replacements[%d]: old text is empty", edit.Path, j)
			}
			at := strings.Index(text, r.Old)
			if at < 0 {
				return nil, fmt.Errorf("edit %q replacements[%d]: old text not found", edit.Path, j)
			}
			// Overlapping occurrences are ambiguous too.
			if strings.Contains(text[at+1:], r.Old) {
				return nil, fmt.Errorf("edit %q replacements[%d]: old text occurs more than once", edit.Path, j)
			}
			text = text[:at] + r.New + text[at+len(r.Old):]
		}
		if len(current[edit.Path]) <= maxEditBytes && len(text) > maxEditBytes {
			return nil, fmt.Errorf("edit %q: result exceeds %d bytes", edit.Path, maxEditBytes)
		}
		out[i] = []byte(text)
	}
	return out, nil
}
