package workflow

import (
	"fmt"
	"sort"
)

// validateSubWorkflows checks SubWorkflow stages: a pinned child workflow
// (id and revision >= 1, never this workflow itself), uniquely named input
// mappings from this workflow's stages or types, at least one uniquely named
// output mapping from a child stage, the parent output artifact and an
// optional failure route taken when the child ends without Done. The child's
// existence and nesting are checked when a parent task is admitted.
func validateSubWorkflows(d Document, outgoing map[string][]string, bad func(string, string)) {
	ids := make([]string, 0, len(d.Stages))
	for id := range d.Stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s, p := d.Stages[id], "stages."+id
		if s.Primitive != SubWorkflow {
			if s.Workflow != nil || len(s.InputMap) > 0 || len(s.OutputMap) > 0 {
				bad(p, "workflow, input_map and output_map belong to SubWorkflow stages")
			}
			continue
		}
		if s.Workflow != nil && s.Workflow.ID == "self" {
			if d.Recursion == nil {
				bad(p+".workflow", "self selection requires recursion budgets")
			}
		} else if s.Workflow == nil || s.Workflow.ID == "" || s.Workflow.Revision < 1 {
			bad(p+".workflow", "a pinned child workflow id and revision >= 1 required")
		} else if s.Workflow.ID == d.ID && d.Recursion == nil {
			bad(p+".workflow", "a workflow cannot run itself as a sub-workflow")
		}
		if s.Output == "" {
			bad(p+".output", "required: the artifact the child's outputs are mapped into")
		}
		if s.Role != "" || s.ChangeProducer {
			bad(p, "a sub-workflow runs as its own child task: no role and no change production in the parent")
		}
		if s.Failure != "" && !contains(outgoing[id], s.Failure) {
			bad(p+".failure", "failure route must be an edge")
		}
		names := map[string]bool{}
		for i, m := range s.InputMap {
			mp := fmt.Sprintf("%s.input_map[%d]", p, i)
			if !identifier(m.Name, 64) || names["in:"+m.Name] {
				bad(mp+".name", "unique input name required")
			}
			names["in:"+m.Name] = true
			if m.Stage == "" && m.Type == "" && m.Input == "" && m.Projection == nil {
				bad(mp, "select the parent artifact by stage and/or type")
			}
			if _, ok := ProducerDeclaration(d, m.Stage); m.Stage != "" && !ok {
				bad(mp+".stage", "unknown stage "+m.Stage)
			}
			if _, ok := ArtifactDeclaration(d, m.Type); m.Type != "" && !ok {
				bad(mp+".type", "unknown artifact type "+m.Type)
			}
		}
		if len(s.OutputMap) == 0 || len(s.OutputMap) > 32 {
			bad(p+".output_map", "between 1 and 32 mapped child artifacts required")
		}
		for i, m := range s.OutputMap {
			mp := fmt.Sprintf("%s.output_map[%d]", p, i)
			if !identifier(m.Name, 64) || names["out:"+m.Name] {
				bad(mp+".name", "unique output name required")
			}
			names["out:"+m.Name] = true
			if m.As != "" {
				if m.Output == "" {
					bad(mp+".as", "as requires a declared child output")
				}
				if _, ok := d.Artifacts[m.As]; !ok {
					bad(mp+".as", "unknown parent artifact type "+m.As)
				}
			}
			if m.Stage == "" && m.Output == "" {
				bad(mp+".stage", "the producing child stage is required")
			}
		}
	}
}
