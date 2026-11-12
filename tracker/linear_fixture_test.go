package tracker

import (
	"encoding/json"
	"net/http"
)

// Older test payloads omitted selected non-null connection metadata. Supply
// complete empty pages for omitted optional fixture collections, as the real
// query returns, while retaining deliberately malformed values and cursors.
func linearFixtureHandler(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { next(linearFixtureWriter{w}, r) }
}

type linearFixtureWriter struct{ http.ResponseWriter }

func (w linearFixtureWriter) Write(raw []byte) (int, error) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return w.ResponseWriter.Write(raw)
	}
	var walk func(any)
	walk = func(v any) {
		switch obj := v.(type) {
		case map[string]any:
			if states, ok := obj["workflowStates"].(map[string]any); ok {
				if _, exists := states["pageInfo"]; !exists {
					states["pageInfo"] = map[string]any{"hasNextPage": false, "endCursor": nil}
				}
			}
			if _, hasID := obj["identifier"]; hasID {
				if _, hasTitle := obj["title"]; hasTitle {
					if _, hasState := obj["state"]; hasState {
						for _, key := range []string{"inverseRelations", "labels", "children"} {
							connection, exists := obj[key]
							if !exists {
								obj[key] = map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false, "endCursor": nil}}
								continue
							}
							if c, ok := connection.(map[string]any); ok {
								if _, hasNodes := c["nodes"].([]any); hasNodes {
									if _, hasInfo := c["pageInfo"]; !hasInfo {
										c["pageInfo"] = map[string]any{"hasNextPage": false, "endCursor": nil}
									}
								}
							}
						}
					}
				}
			}
			for _, child := range obj {
				walk(child)
			}
		case []any:
			for _, child := range obj {
				walk(child)
			}
		}
	}
	walk(value)
	body, err := json.Marshal(value)
	if err != nil {
		return 0, err
	}
	if _, err := w.ResponseWriter.Write(body); err != nil {
		return 0, err
	}
	return len(raw), nil
}
