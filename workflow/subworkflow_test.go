package workflow

import (
	"encoding/json"
	"testing"
)

// subDoc consults a pinned child workflow between the test gate and review.
func subDoc() Document {
	d := protected()
	d.Artifacts["consult"] = ArtifactType{1, json.RawMessage(`{"type":"object","required":["outputs"]}`)}
	d.Stages["consult"] = Stage{Primitive: SubWorkflow, Workflow: &WorkflowRef{ID: "child", Revision: 2}, Output: "consult", Failure: "stop", InputMap: []ArtifactMap{{Name: "change", Stage: "change"}, {Name: "run", Type: "report", Stage: "test"}}, OutputMap: []ArtifactMap{{Name: "note", Stage: "draft"}}}
	for i, e := range d.Edges {
		if e == (Edge{"gate", "review"}) {
			d.Edges[i] = Edge{"gate", "consult"}
		}
	}
	d.Edges = append(d.Edges, Edge{"consult", "review"}, Edge{"consult", "stop"})
	return d
}

// FAE-WF-030 SubWorkflow conformance: a pinned child, named input and output
// mappings, the mapped output artifact and an optional failure route.
func TestSubWorkflowValidation(t *testing.T) {
	if issues := Validate(subDoc()); len(issues) > 0 {
		t.Fatal(issues)
	}
	edit := func(stage string, f func(*Stage)) Document {
		d := subDoc()
		s := d.Stages[stage]
		f(&s)
		d.Stages[stage] = s
		return d
	}
	for _, c := range []struct {
		name, path, message string
		d                   Document
	}{
		{"no workflow", "stages.consult.workflow", "pinned child", edit("consult", func(s *Stage) { s.Workflow = nil })},
		{"unpinned revision", "stages.consult.workflow", "revision >= 1", edit("consult", func(s *Stage) { s.Workflow.Revision = 0 })},
		{"self", "stages.consult.workflow", "cannot run itself", edit("consult", func(s *Stage) { s.Workflow.ID = "demo" })},
		{"no output", "stages.consult.output", "required", edit("consult", func(s *Stage) { s.Output = "" })},
		{"role", "stages.consult", "no role", edit("consult", func(s *Stage) { s.Role = "coder" })},
		{"failure not an edge", "stages.consult.failure", "must be an edge", edit("consult", func(s *Stage) { s.Failure = "commit" })},
		{"duplicate input", "stages.consult.input_map[1].name", "unique input name", edit("consult", func(s *Stage) { s.InputMap[1].Name = "change" })},
		{"unselected input", "stages.consult.input_map[0]", "by stage and/or type", edit("consult", func(s *Stage) { s.InputMap[0].Stage = "" })},
		{"unknown input stage", "stages.consult.input_map[0].stage", "unknown stage", edit("consult", func(s *Stage) { s.InputMap[0].Stage = "nowhere" })},
		{"unknown input type", "stages.consult.input_map[1].type", "unknown artifact type", edit("consult", func(s *Stage) { s.InputMap[1].Type = "Rport" })},
		{"no outputs", "stages.consult.output_map", "between 1 and 32", edit("consult", func(s *Stage) { s.OutputMap = nil })},
		{"output without stage", "stages.consult.output_map[0].stage", "producing child stage", edit("consult", func(s *Stage) { s.OutputMap[0].Stage = "" })},
		{"mapping on gate", "stages.gate", "belong to SubWorkflow", edit("gate", func(s *Stage) { s.OutputMap = []ArtifactMap{{Name: "x", Stage: "y"}} })},
	} {
		expectIssue(t, c.name, c.d, c.path, c.message)
	}
	s := subDoc().Stages["consult"]
	if !FailureRouted(s) {
		t.Fatal("a sub-workflow with a failure route is failure-routed")
	}
	s.Failure = ""
	if FailureRouted(s) {
		t.Fatal("a sub-workflow without a failure route escalates")
	}
}
