package workflow

import "encoding/json"

// JoinOutputIssues applies the new-write join contract without making stored
// pre-extension revisions unreadable. Legacy joins may have emitted no artifact.
func JoinOutputIssues(d Document) []Issue {
	issues := []Issue{}
	for id, s := range d.Stages {
		if s.Primitive != FanIn {
			continue
		}
		path := "stages." + id + ".output"
		typ, ok := d.Artifacts[s.Output]
		if s.Output == "" || !ok {
			issues = append(issues, Issue{path, "a new FanIn must declare a list output artifact"})
			continue
		}
		var shape struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(typ.Schema, &shape) != nil || shape.Type != "array" {
			issues = append(issues, Issue{path, "a new FanIn output schema must have type array"})
		}
	}
	return sortIssues(issues)
}
