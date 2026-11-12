package engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/xleelz/fae/workflow"
)

var commandReference = regexp.MustCompile(`\{\{([A-Za-z_][A-Za-z0-9_.:-]*\??)\}\}`)

func commandTemplateValues(j Job, s workflow.Stage) (map[string]any, error) {
	values := map[string]any{}
	for _, arg := range s.Command {
		for _, m := range commandReference.FindAllStringSubmatch(arg, -1) {
			name := strings.TrimSuffix(m[1], "?")
			if !workflow.RuntimeReference(name) {
				continue
			}
			if name == "visit" || strings.HasPrefix(name, "loop.") || strings.HasSuffix(m[1], "?") {
				value, present, err := ResolveRuntimeReference(j, name, j.Counts[string(j.State)]+1)
				if err != nil {
					return nil, err
				}
				if !present {
					return nil, fmt.Errorf("command %s is absent", name)
				}
				values[name] = value
				continue
			}
			switch name {
			case "output_dir":
				values[name] = j.OutputDir
			case "staging_dir":
				values[name] = j.StagingDir
			default:
				raw, _ := json.Marshal(name)
				var p workflow.Projection
				if err := json.Unmarshal(raw, &p); err != nil {
					return nil, err
				}
				value, _, err := ResolveProjection(j, p)
				if err != nil {
					return nil, fmt.Errorf("command %s: %w", name, err)
				}
				values[name] = value
			}
			if v, ok := values[name].(string); ok && strings.HasSuffix(name, "_dir") && v == "" {
				return nil, fmt.Errorf("command directory %s is unavailable", name)
			}
		}
	}
	return values, nil
}
func BindStageCommand(j Job, s workflow.Stage) ([]string, error) {
	values, err := commandTemplateValues(j, s)
	if err != nil {
		return nil, err
	}
	return workflow.BindCommandValues(s, boundInputs(j, s), values)
}
