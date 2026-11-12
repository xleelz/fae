package engine

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

func RootTask(j Job) string {
	if j.TreeRoot != "" {
		return j.TreeRoot
	}
	return j.TaskID
}
func Depth(j Job) int {
	if j.TreeDepth > 0 {
		return j.TreeDepth
	}
	return 1
}
func TreeValue(j Job) map[string]any {
	parent := ""
	if j.Parent != nil {
		parent = j.Parent.Task
	}
	value := map[string]any{"task": j.TaskID, "parent": parent, "root": RootTask(j), "depth": Depth(j)}
	if RootTask(j) == j.TaskID && j.Document.Recursion != nil || j.RootMaxDescendants > 0 {
		max := j.RootMaxDescendants
		if RootTask(j) == j.TaskID && j.Document.Recursion != nil {
			max = j.Document.Recursion.MaxDescendants
		}
		value["remaining_descendants"] = max - j.TreeDescendants
	}
	return value
}
func treeEvidence(j Job) workflow.Evidence {
	fields := TreeValue(j)
	raw, _ := json.Marshal(fields)
	id, _ := domain.ContentHash(raw)
	return workflow.Evidence{ID: id, TaskID: j.TaskID, Type: "tree", Stage: "tree", Version: 1, Tree: j.Tree, Role: "engine", Session: "tree", Provenance: true, Fields: fields}
}
func withTaskData(j Job, evidence []workflow.Evidence) []workflow.Evidence {
	filtered := evidence[:0]
	for _, a := range evidence {
		if a.Stage != "tree" {
			filtered = append(filtered, a)
		}
	}
	evidence = filtered
	seen := map[string]bool{}
	for _, a := range evidence {
		seen[a.Stage] = true
	}
	for _, a := range j.Evidence {
		if strings.HasPrefix(a.Stage, "input:") && !seen[a.Stage] {
			evidence = append(evidence, a)
		}
	}
	return append(evidence, treeEvidence(j))
}

// BindTaskInputs pins canonical values and their producing parent artifacts.
// It runs at admission only, before dispatch and before any mutable task state.
func BindTaskInputs(j Job, supplied map[string]any, sources map[string][]string) (Job, error) {
	values, issues := workflow.BindInputs(j.Document, supplied)
	if len(issues) > 0 {
		return j, &workflow.ValidationError{Issues: issues}
	}
	j.Inputs = values
	j.InputSources = sources
	for name, value := range values {
		stage := "input:" + name
		fields := workflow.InputFields(value)
		raw, err := json.Marshal(fields)
		if err != nil {
			return j, err
		}
		id, err := domain.ContentHash(raw)
		if err != nil {
			return j, err
		}
		j.Evidence = append(j.Evidence, workflow.Evidence{ID: id, TaskID: j.TaskID, Type: stage, Version: 1, Stage: stage, Tree: j.Tree, Role: "engine", Session: stage, Provenance: true, Fields: fields})
		j.Provenance = append(j.Provenance, Provenance{ArtifactID: id, TaskID: j.TaskID, Stage: stage, Primitive: workflow.AgentTurn, Role: "engine", Session: stage, DispatchID: stage, Tree: j.Tree, ExecutionKind: j.ExecutionKind, ProducedAt: time.Now().UTC(), Inputs: sources[name], WorkflowHash: j.WorkflowHash, WorkflowRevision: j.Revision})
	}
	return j, nil
}
func validateInputEvidence(j Job, a workflow.Evidence) (bool, error) {
	if !strings.HasPrefix(a.Stage, "input:") {
		return false, nil
	}
	name := strings.TrimPrefix(a.Stage, "input:")
	value, ok := j.Inputs[name]
	if j.Document.Signature == nil {
		return true, fmt.Errorf("input evidence without signature")
	}
	if !ok || a.Type != a.Stage || a.Version != 1 || a.TaskID != j.TaskID || a.Role != "engine" || a.Session != a.Stage || !a.Provenance {
		return true, fmt.Errorf("invalid pinned input %s", name)
	}
	want, _ := json.Marshal(workflow.InputFields(value))
	got, _ := json.Marshal(a.Fields)
	h, _ := domain.ContentHash(want)
	g, _ := domain.ContentHash(got)
	if h != g || a.ID != h {
		return true, fmt.Errorf("pinned input %s identity mismatch", name)
	}
	raw, _ := json.Marshal(value)
	schema, declared := j.Document.Signature.Inputs[name]
	if !declared {
		return true, fmt.Errorf("undeclared pinned input %s", name)
	}
	if err := workflow.ValidateArtifact(schema, raw); err != nil {
		return true, err
	}
	for _, p := range j.Provenance {
		if p.ArtifactID == a.ID && p.Stage == a.Stage && p.TaskID == j.TaskID && p.DispatchID == a.Stage && p.ExecutionKind == j.ExecutionKind && !p.ProducedAt.IsZero() {
			return true, nil
		}
	}
	return true, fmt.Errorf("pinned input %s provenance missing", name)
}
func missingDeclaredOutputs(j Job) []string {
	if j.Document.Signature == nil {
		return nil
	}
	var missing []string
	for name, o := range j.Document.Signature.Outputs {
		found := false
		for _, a := range j.Evidence {
			if a.Type == o.Type && a.Provenance {
				if len(o.Schema) > 0 {
					raw, _ := workflow.EvidenceJSON(a)
					if workflow.ValidateArtifact(o.Schema, raw) != nil {
						continue
					}
				}
				found = true
			}
		}
		if !found {
			missing = append(missing, name)
		}
	}
	return missing
}

func validateTreeEvidence(j Job, a workflow.Evidence) (bool, error) {
	if a.Stage != "tree" {
		return false, nil
	}
	raw, _ := json.Marshal(a.Fields)
	id, _ := domain.ContentHash(raw)
	if a.Type != "tree" || a.Version != 1 || a.TaskID != j.TaskID || a.Role != "engine" || !a.Provenance || id != a.ID {
		return true, fmt.Errorf("invalid tree evidence")
	}
	return true, nil
}
func recordTaskDataEvidence(j *Job, evidence []workflow.Evidence) {
	for _, a := range evidence {
		if a.Stage != "tree" {
			continue
		}
		seen := false
		for _, old := range j.Evidence {
			seen = seen || old.ID == a.ID && old.Stage == a.Stage
		}
		if seen {
			continue
		}
		j.Evidence = append(j.Evidence, a)
		j.Provenance = append(j.Provenance, Provenance{ArtifactID: a.ID, DispatchID: "tree", TaskID: j.TaskID, Stage: "tree", Primitive: workflow.AgentTurn, Role: "engine", Session: "tree", Tree: a.Tree, ExecutionKind: j.ExecutionKind, ProducedAt: time.Now().UTC(), WorkflowHash: j.WorkflowHash, WorkflowRevision: j.Revision})
	}
}

// MappedInputValues resolves SubWorkflow mappings as literal data while retaining
// source artifact IDs, including pinned task inputs and the read-only tree value.
func MappedInputValues(j Job, s workflow.Stage) (map[string]any, map[string][]string, error) {
	return mappedInputValues(j, s, nil)
}
func mappedInputValues(j Job, s workflow.Stage, item *any) (map[string]any, map[string][]string, error) {
	values := map[string]any{}
	sources := map[string][]string{}
	var issues []string
	for _, m := range s.InputMap {
		if m.Projection != nil {
			value, ids, err := resolveProjection(j, *m.Projection, item)
			if err != nil {
				issues = append(issues, "input_map."+m.Name+": "+err.Error())
			} else {
				values[m.Name], sources[m.Name] = value, ids
			}
			continue
		}
		var found *workflow.Evidence
		selector := m.Stage
		if m.Input != "" {
			selector = "input:" + m.Input
		}
		for _, a := range latestInputs(j) {
			if (selector == "" || a.Stage == selector) && (m.Type == "" || a.Type == m.Type) {
				copy := a
				found = &copy
			}
		}
		if found == nil {
			issues = append(issues, "input_map."+m.Name+": source missing")
			continue
		}
		var value any = found.Fields
		if strings.HasPrefix(found.Stage, "input:") {
			value = j.Inputs[strings.TrimPrefix(found.Stage, "input:")]
		}
		if m.Field != "" {
			var ok bool
			for _, part := range strings.Split(m.Field, ".") {
				object, isMap := value.(map[string]any)
				if !isMap {
					ok = false
					break
				}
				value, ok = object[part]
				if !ok {
					break
				}
			}
			if !ok {
				issues = append(issues, "input_map."+m.Name+": field "+m.Field+" missing")
				continue
			}
		}
		values[m.Name] = value
		sources[m.Name] = []string{found.ID}
	}
	if len(issues) > 0 {
		return values, sources, fmt.Errorf("%s", strings.Join(issues, "; "))
	}
	return values, sources, nil
}

func expressionReadsTree(x workflow.Expr) bool {
	if x.Stage == "tree" || x.Type == "tree" {
		return true
	}
	for _, a := range x.Args {
		if expressionReadsTree(a) {
			return true
		}
	}
	return false
}
