package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// ParkSubWorkflow is the park reason of a task waiting for its child task.
const ParkSubWorkflow = "SubWorkflowPending"

// ChildRef is the child task a parent's SubWorkflow visit waits for, pinned
// to the child workflow id and revision (its own molecule). When a SubWorkflow
// stage explicitly fans out to N > 1 children via items_from, Children holds all
// N child references while ChildRef itself names the primary (index 0) child.
type ChildRef struct {
	Stage      string     `json:"stage"`
	Visit      int        `json:"visit"`
	Task       string     `json:"task"`
	WorkflowID string     `json:"workflow_id"`
	Revision   int        `json:"revision"`
	Children   []ChildRef `json:"children,omitempty"`
}

// ParentRef links a child task to the parent visit that started it.
type ParentRef struct {
	VisitGroups map[string]int `json:"visit_groups,omitempty"`
	Detached    bool           `json:"detached,omitempty"`
	BranchID    string         `json:"branch_id,omitempty"`
	Task        string         `json:"task"`
	Stage       string         `json:"stage"`
	Visit       int            `json:"visit"`
}

// ChildID names the child task of one parent SubWorkflow visit.
func ChildID(parent, stage string, visit int) string {
	id := fmt.Sprintf("%s.%s.%d", parent, stage, visit)
	return id
}

// ChildIDIndexed names the index-th child task of a multi-child SubWorkflow visit.
func ChildIDIndexed(parentID, stage string, visit, index int) string {
	if index < 0 {
		return ChildID(parentID, stage, visit)
	}
	id := fmt.Sprintf("%s.%s.%d.%d", parentID, stage, visit, index)
	return id
}

// childVisitID preserves the branch address as part of the hierarchical task ID.
func childVisitID(j Job, stage string, visit, index int) string {
	id := ChildID(j.TaskID, stage, visit)
	if j.BranchContext != nil {
		parts := strings.Split(j.BranchContext.Path, "/")
		if len(j.BranchContext.IndexPath) > 0 {
			parts = nil
			for _, index := range j.BranchContext.IndexPath {
				parts = append(parts, fmt.Sprint(index))
			}
		} else {
			for i := range parts {
				parts[i] = strings.TrimPrefix(parts[i], "branch-")
			}
		}
		id += "." + strings.Join(parts, ".")
	}
	if index >= 0 {
		id += fmt.Sprintf(".%d", index)
	}
	id += fanVisitSuffix(j.BranchContext)
	return id
}

// awaitChild parks a task arriving at a SubWorkflow stage and records the
// pinned child (or N children selected by items_from) it
// waits for; the daemon admits each child and binds the result(s).
func awaitChild(j Job, s workflow.Stage, events []Event) (Job, *Dispatch, []Event, error) {
	stage := string(j.State)
	j.Counts[stage]++
	visit := j.Counts[stage]
	wid, revision := s.Workflow.ID, s.Workflow.Revision
	if wid == "self" {
		wid, revision = j.Document.ID, j.Revision
	}
	child := ChildRef{Stage: stage, Visit: visit, Task: childVisitID(j, stage, visit, -1), WorkflowID: wid, Revision: revision}
	j.Child = &child
	j.Parked = true
	j.Reason = ParkSubWorkflow
	for _, mapping := range s.InputMap {
		if mapping.Projection == nil || s.ItemsFrom != nil {
			continue
		}
		if err := recordProjection(&j, stage, "input_map."+mapping.Name, *mapping.Projection, &events); err != nil {
			failed, more, e := ChildFailed(j, "binding: "+err.Error())
			return failed, nil, append(events, more...), e
		}
	}
	n := -1
	if s.ItemsFrom != nil {
		items, _, err := ExplicitChildItems(j, s)
		if err != nil {
			failed, more, e := ChildFailed(j, "binding: "+err.Error())
			return failed, nil, append(events, more...), e
		}
		n = len(items)
		_, itemIDs, _ := ExplicitChildItems(j, s)
		for i, item := range items {
			for _, m := range s.InputMap {
				if m.Projection != nil {
					if err := recordProjectionItem(&j, stage, fmt.Sprintf("input_map.%s[%d]", m.Name, i), *m.Projection, &item, itemIDs, &events); err != nil {
						failed, more, e := ChildFailed(j, "binding: "+err.Error())
						return failed, nil, append(events, more...), e
					}
				}
			}
		}
		if err := recordProjection(&j, stage, "items_from", *s.ItemsFrom, &events); err != nil {
			return j, nil, events, err
		}
	}
	if j.Document.Recursion != nil && max(n, 1) > j.Document.Recursion.MaxChildrenPerVisit {
		failed, more, err := ChildFailed(j, "recursion_budget: max_children_per_visit exceeded")
		return failed, nil, append(events, more...), err
	}

	if n == 0 {
		raw := []byte(`{"children":[],"child_tasks":[],"outputs":{},"artifacts":{}}`)
		if err := workflow.ValidateArtifact(j.Document.Artifacts[s.Output].Schema, raw); err != nil {
			failed, more, e := ChildFailed(j, "empty child outputs: "+err.Error())
			return failed, nil, append(events, more...), e
		}
		err := bindChildOutput(&j, child, raw, nil, "empty child visit", &events)
		return j, nil, events, err
	}
	if n > maxImplicitChildren(j) {
		event(&j, Event{Kind: "sub-workflow", Stage: stage, Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("child %s on %s revision %d", j.Child.Task, j.Child.WorkflowID, j.Child.Revision), DispatchID: j.Child.Task}, &events)
		failedJob, failEvents, err := ChildFailed(j, fmt.Sprintf("sub-workflow fan-out items count %d exceeds maximum 16", n))
		if err != nil {
			return j, nil, nil, err
		}
		events = append(events, failEvents...)
		return failedJob, nil, events, nil
	}
	if n > 1 {
		children := make([]ChildRef, n)
		for idx := 0; idx < n; idx++ {
			children[idx] = ChildRef{
				Stage:      stage,
				Visit:      visit,
				Task:       childVisitID(j, stage, visit, idx),
				WorkflowID: wid,
				Revision:   revision,
			}
		}
		j.SetChildren(children)
	}
	event(&j, Event{Kind: "sub-workflow", Stage: stage, Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("child %s on %s revision %d", j.Child.Task, j.Child.WorkflowID, j.Child.Revision), DispatchID: j.Child.Task}, &events)
	return j, nil, events, nil
}

// ExplicitChildItems resolves only the declared list; artifact field names never fan out.
func ExplicitChildItems(j Job, s workflow.Stage) ([]any, []string, error) {
	if s.ItemsFrom == nil {
		return nil, nil, nil
	}
	value, ids, err := ResolveProjection(j, *s.ItemsFrom)
	if err != nil {
		return nil, nil, err
	}
	items, ok := value.([]any)
	if !ok {
		return nil, nil, fmt.Errorf("items_from must resolve to an array")
	}
	return items, ids, nil
}

// ChildMappedInputValues binds each child's own signature inputs. The selected
// item is visible only to source:item projections, never injected as a wrapper.
func ChildMappedInputValues(j Job, index int) (map[string]any, map[string][]string, error) {
	ref, ok := PendingChild(j)
	if !ok {
		return nil, nil, errors.New("no pending sub-workflow")
	}
	s := j.Document.Stages[ref.Stage]
	if s.ItemsFrom == nil {
		return MappedInputValues(j, s)
	}
	items, ids, err := ExplicitChildItems(j, s)
	if err != nil {
		return nil, nil, err
	}
	if index < 0 || index >= len(items) {
		return nil, nil, fmt.Errorf("child item index %d out of range", index)
	}
	values, sources, err := mappedInputValues(j, s, &items[index])
	for name := range values {
		sources[name] = append(sources[name], ids...)
	}
	return values, sources, err
}
func maxImplicitChildren(j Job) int {
	if j.Document.Recursion != nil {
		return min(256, j.Document.Recursion.MaxChildrenPerVisit)
	}
	return 16
}

func validateChild(j Job) error {
	c := j.Child
	if c == nil {
		return nil
	}
	s, ok := j.Document.Stages[c.Stage]
	if !ok || s.Primitive != workflow.SubWorkflow || s.Workflow == nil || !matchesChildWorkflow(j, s, c.WorkflowID, c.Revision) || c.Visit < 1 {
		return errors.New("invalid sub-workflow child reference")
	}
	if len(c.Children) > 1 {
		if len(c.Children) > maxImplicitChildren(j) || c.Task != childVisitID(j, c.Stage, c.Visit, 0) {
			return errors.New("invalid sub-workflow child reference")
		}
		for i, ch := range c.Children {
			if ch.Stage != c.Stage || ch.Visit != c.Visit || !matchesChildWorkflow(j, s, ch.WorkflowID, ch.Revision) || ch.Task != childVisitID(j, c.Stage, c.Visit, i) || len(ch.Children) != 0 {
				return errors.New("invalid sub-workflow child reference")
			}
		}
		return nil
	}
	if len(c.Children) == 1 || c.Task != childVisitID(j, c.Stage, c.Visit, -1) {
		return errors.New("invalid sub-workflow child reference")
	}
	return nil
}

// PendingChild reports the child task a parent is waiting for, if any.
func PendingChild(j Job) (ChildRef, bool) {
	if j.Child == nil || !j.Parked || j.Reason != ParkSubWorkflow || string(j.State) != j.Child.Stage || j.InFlight != nil || j.State.Terminal() {
		return ChildRef{}, false
	}
	return *j.Child, true
}

// ChildInputs is the structured child task content: the parent's mapped
// artifacts by name, each the latest matching artifact of the parent.
func ChildInputs(j Job) (json.RawMessage, error) {
	ref, ok := PendingChild(j)
	if !ok {
		return nil, errors.New("no pending sub-workflow")
	}
	s := j.Document.Stages[ref.Stage]
	inputs := map[string]any{}
	if s.ItemsFrom != nil {
		var err error
		inputs, _, err = ChildMappedInputValues(j, 0)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]any{"parent_task": j.TaskID, "parent_stage": ref.Stage, "inputs": inputs})
	}
	for _, m := range s.InputMap {
		if m.Projection != nil {
			value, _, err := ResolveProjection(j, *m.Projection)
			if err != nil {
				return nil, fmt.Errorf("binding: input_map %s: %w", m.Name, err)
			}
			inputs[m.Name] = value
			continue
		}
		for _, a := range latestInputs(j) {
			if (m.Stage == "" || a.Stage == m.Stage) && (m.Type == "" || a.Type == m.Type) {
				inputs[m.Name] = map[string]any{"artifact_id": a.ID, "type": a.Type, "stage": a.Stage, "fields": a.Fields}
			}
		}
	}
	return json.Marshal(map[string]any{"parent_task": j.TaskID, "parent_stage": ref.Stage, "inputs": inputs})
}

// ChildInputItems returns structured content for each explicitly selected child.
// Signature binding uses ChildMappedInputValues, never these admission wrappers.
func ChildInputItems(j *Job) ([]json.RawMessage, error) {
	if j == nil {
		return nil, errors.New("nil job")
	}
	ref, ok := PendingChild(*j)
	if !ok {
		return nil, errors.New("no pending sub-workflow")
	}
	stage := j.Document.Stages[ref.Stage]
	if stage.ItemsFrom == nil {
		raw, err := ChildInputs(*j)
		return []json.RawMessage{raw}, err
	}
	items, _, err := ExplicitChildItems(*j, stage)
	if err != nil {
		return nil, err
	}
	if len(items) > maxImplicitChildren(*j) {
		return nil, fmt.Errorf("sub-workflow items count exceeds maximum %d", maxImplicitChildren(*j))
	}
	out := make([]json.RawMessage, len(items))
	for i, item := range items {
		values, _, err := ChildMappedInputValues(*j, i)
		if err != nil {
			return nil, err
		}
		out[i], err = json.Marshal(map[string]any{"parent_task": j.TaskID, "parent_stage": ref.Stage, "inputs": values, "item": item, "item_index": i})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// BindChild binds a terminal child task's result into its waiting parent. A
// Done child's mapped artifacts become the parent stage's output artifact,
// with provenance naming the child task and its artifacts, and the parent
// takes its success route. A child that ended without Done (cancelled or
// retired) or lacks a mapped artifact takes the failure route if declared,
// else escalates. A child still running is never bound.
func BindChild(input Job, child Job) (Job, []Event, error) {
	j := clone(input)
	events := []Event{}
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	if err := Validate(child); err != nil {
		return input, nil, err
	}
	ref, ok := PendingChild(j)
	if !ok {
		return input, nil, errors.New("parent is not waiting for a sub-workflow")
	}
	if child.TaskID != ref.Task || child.Document.ID != ref.WorkflowID || child.Revision != ref.Revision || child.Parent == nil || (!matchesParent(child.Parent, j.TaskID, ref.Stage, ref.Visit) || child.Parent.BranchID != childBranchID(j)) {
		return input, nil, errors.New("child task does not match the pinned sub-workflow")
	}
	if !child.State.Terminal() && child.State != domain.Escalated {
		return input, nil, errors.New("child task has not finished")
	}
	s := j.Document.Stages[ref.Stage]
	fail := func(detail string) (Job, []Event, error) { return ChildFailed(input, detail) }
	if child.State == domain.Escalated && s.OnChildEscalation == "wait" {
		return input, nil, nil
	}
	if child.State != domain.Done {
		return fail(fmt.Sprintf("child %s ended %s: %s", ref.Task, child.State, bounded(child.Reason)))
	}
	outputs, artifacts, inputs := map[string]any{}, map[string]any{}, []string{}
	for _, m := range s.OutputMap {
		found := selectMappedChildOutput(child, m)
		if found == nil {
			return fail(fmt.Sprintf("child %s produced no %s artifact for output %s", ref.Task, m.Stage, m.Name))
		}
		if err := publishChildAlias(&j, ref, child, m, *found); err != nil {
			return fail(err.Error())
		}
		outputs[m.Name], artifacts[m.Name] = found.Fields, found.ID
		inputs = append(inputs, found.ID)
	}
	fields := map[string]any{"child_task": child.TaskID, "workflow_id": child.Document.ID, "revision": child.Revision, "workflow_hash": child.WorkflowHash, "outputs": outputs, "artifacts": artifacts}
	raw, err := json.Marshal(fields)
	if err != nil {
		return input, nil, err
	}
	if err := workflow.ValidateArtifact(j.Document.Artifacts[s.Output].Schema, raw); err != nil {
		return fail(fmt.Sprintf("mapped child outputs do not satisfy %s: %v", s.Output, err))
	}
	if err := bindChildOutput(&j, ref, raw, inputs, fmt.Sprintf("child %s Done on %s revision %d", ref.Task, child.Document.ID, child.Revision), &events); err != nil {
		return input, nil, err
	}
	return j, events, Validate(j)
}

// BindChildren binds all N completed child tasks of a multi-child SubWorkflow
// visit into parent. It verifies that every child matches parent.Children() and
// is Done, extracts each child's mapped outputs, binds both the primary (first
// child's) mapped output keys and "children": [<child0_mapped>, ...] into the
// parent's stage Evidence, records Provenance, clears parent.Child and
// parent.Children, unparks parent, and advances parent to the next stage.
func BindChildren(parent *Job, children []*Job) error {
	if parent == nil {
		return errors.New("parent job is nil")
	}
	j := clone(*parent)
	if err := Validate(j); err != nil {
		return err
	}
	ref, ok := PendingChild(j)
	if !ok {
		return errors.New("parent is not waiting for a sub-workflow")
	}
	expected := ref.Children
	if len(expected) <= 1 {
		if len(children) != 1 || children[0] == nil {
			return errors.New("expected 1 child job for single-child sub-workflow")
		}
		next, _, err := BindChild(*parent, *children[0])
		if err != nil {
			return err
		}
		*parent = next
		return nil
	}
	if len(children) != len(expected) {
		return fmt.Errorf("children count %d does not match pending sub-workflow count %d", len(children), len(expected))
	}

	for i, exp := range expected {
		child := children[i]
		if child == nil {
			return fmt.Errorf("child %d (%s) is nil", i, exp.Task)
		}
		if err := Validate(*child); err != nil {
			return err
		}
		if child.TaskID != exp.Task || child.Document.ID != exp.WorkflowID || child.Revision != exp.Revision || child.Parent == nil || (!matchesParent(child.Parent, j.TaskID, exp.Stage, exp.Visit) || child.Parent.BranchID != childBranchID(j)) {
			return fmt.Errorf("child %s does not match pinned sub-workflow %s", child.TaskID, exp.Task)
		}
		if !child.State.Terminal() && child.State != domain.Escalated {
			return fmt.Errorf("child %s has not finished", exp.Task)
		}
	}

	s := j.Document.Stages[ref.Stage]
	fail := func(detail string) error {
		next, _, err := ChildFailed(*parent, detail)
		if err != nil {
			return err
		}
		*parent = next
		return nil
	}
	childrenMapped := make([]map[string]any, len(expected))
	childTasks := make([]string, len(expected))
	var allInputs []string
	var primaryOutputs, primaryArtifacts map[string]any

	for i, exp := range expected {
		child := children[i]
		childTasks[i] = child.TaskID
		if child.State != domain.Done {
			return fail(fmt.Sprintf("child %s ended %s: %s", exp.Task, child.State, bounded(child.Reason)))
		}
		outputs, artifacts := map[string]any{}, map[string]any{}
		for _, m := range s.OutputMap {
			found := selectMappedChildOutput(*child, m)
			if found == nil {
				return fail(fmt.Sprintf("child %s produced no %s artifact for output %s", exp.Task, m.Stage, m.Name))
			}
			if err := publishChildAlias(&j, ref, *child, m, *found); err != nil {
				return fail(err.Error())
			}
			outputs[m.Name], artifacts[m.Name] = found.Fields, found.ID
			allInputs = append(allInputs, found.ID)
		}
		entry := map[string]any{
			"child_task":    child.TaskID,
			"workflow_id":   child.Document.ID,
			"revision":      child.Revision,
			"workflow_hash": child.WorkflowHash,
			"outputs":       outputs,
			"artifacts":     artifacts,
		}
		for k, v := range outputs {
			if _, exists := entry[k]; !exists {
				entry[k] = v
			}
		}
		childrenMapped[i] = entry
		if i == 0 {
			primaryOutputs = outputs
			primaryArtifacts = artifacts
		}
	}

	fields := map[string]any{
		"child_task":    children[0].TaskID,
		"child_tasks":   childTasks,
		"workflow_id":   children[0].Document.ID,
		"revision":      children[0].Revision,
		"workflow_hash": children[0].WorkflowHash,
		"outputs":       primaryOutputs,
		"artifacts":     primaryArtifacts,
		"children":      childrenMapped,
	}
	for k, v := range primaryOutputs {
		if _, exists := fields[k]; !exists {
			fields[k] = v
		}
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if err := workflow.ValidateArtifact(j.Document.Artifacts[s.Output].Schema, raw); err != nil {
		return fail(fmt.Sprintf("mapped child outputs do not satisfy %s: %v", s.Output, err))
	}
	events := []Event{}
	cause := fmt.Sprintf("%d children Done on %s revision %d", len(expected), children[0].Document.ID, children[0].Revision)
	if err := bindChildOutput(&j, ref, raw, allInputs, cause, &events); err != nil {
		return err
	}
	if err := Validate(j); err != nil {
		return err
	}
	*parent = j
	return nil
}

// bindChildOutput records a child's mapped result, already validated against
// the stage's output type, as the parent stage's artifact and takes its
// success route.
func bindChildOutput(j *Job, ref ChildRef, raw []byte, inputs []string, cause string, events *[]Event) error {
	s := j.Document.Stages[ref.Stage]
	typ := j.Document.Artifacts[s.Output]
	id, err := domain.ContentHash(raw)
	if err != nil {
		return err
	}
	var decoded map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&decoded); err != nil {
		return err
	}
	session := ref.Task
	if len(ref.Children) > 1 {
		tasks := make([]string, len(ref.Children))
		for i, c := range ref.Children {
			tasks[i] = c.Task
		}
		if joined := strings.Join(tasks, ","); len(joined) <= 255 {
			session = joined
		}
	}
	j.Evidence = append(j.Evidence, workflow.Evidence{ID: id, TaskID: j.TaskID, Type: s.Output, Version: typ.Version, Stage: ref.Stage, Tree: j.Tree, Role: "sub-workflow", Session: session, Provenance: true, Fields: decoded})
	j.Provenance = append(j.Provenance, Provenance{BranchID: childBranchID(*j), ExecutionKind: j.ExecutionKind, ArtifactID: id, DispatchID: session, TaskID: j.TaskID, Stage: ref.Stage, Primitive: workflow.SubWorkflow, Role: "sub-workflow", Session: session, Tree: j.Tree, Inputs: inputs, ProducedAt: time.Now().UTC(), WorkflowHash: j.WorkflowHash, WorkflowRevision: j.Revision})
	j.Parked, j.Reason, j.Child = false, "", nil
	event(j, Event{Kind: "sub-workflow-bound", Stage: ref.Stage, DispatchID: session, Attempt: j.stagedAttempt(), Cause: cause}, events)
	next, err := route(*j, s)
	if err != nil {
		return err
	}
	return move(j, next, "completed "+session, events)
}

// ChildFailed ends a parent's SubWorkflow visit without a result (the child
// ended without Done, lacked a mapped artifact, or could not be admitted): the
// stage's failure route when declared, else a HumanDecisionRequired escalation.
func ChildFailed(input Job, detail string) (Job, []Event, error) {
	j := clone(input)
	events := []Event{}
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	ref, ok := PendingChild(j)
	if !ok {
		return input, nil, errors.New("parent is not waiting for a sub-workflow")
	}
	s := j.Document.Stages[ref.Stage]
	j.Parked, j.Reason, j.Child = false, "", nil
	event(&j, Event{Kind: "sub-workflow-failed", Stage: ref.Stage, DispatchID: ref.Task, Attempt: j.stagedAttempt(), Cause: bounded(detail)}, &events)
	if s.Failure != "" {
		reason := "child_failed"
		switch {
		case strings.Contains(detail, "recursion_budget"):
			reason = "recursion_budget"
		case strings.Contains(detail, "binding:") || strings.Contains(detail, "projection"):
			reason = "binding"
		case strings.Contains(detail, "admission refused"):
			reason = "admission"
		}
		if err := routeFailure(&j, reason, detail, ref.Task, &events); err != nil {
			return input, nil, err
		}
		return j, events, Validate(j)
	}
	stop(&j, domain.HumanDecisionRequired, "SubWorkflowFailed: "+bounded(detail), &events)
	return j, events, Validate(j)
}

// Obsolete reports whether a child task's parent no longer waits for it (the
// parent was cancelled, retired, restarted or moved on); such a child is
// retired as obsolete rather than left running.
func Obsolete(child Job, parent *Job) bool {
	if child.Parent == nil || child.Detached || child.Parent.Detached || child.State.Terminal() {
		return false
	}
	if parent == nil {
		return true
	}
	view := *parent
	if child.Parent.BranchID != "" {
		var exists bool
		view, exists = BranchJob(*parent, child.Parent.BranchID)
		if !exists {
			return true
		}
	}
	ref, ok := PendingChild(view)
	if !ok {
		return true
	}
	if len(ref.Children) > 1 {
		for _, c := range ref.Children {
			if c.Task == child.TaskID && c.Stage == child.Parent.Stage && c.Visit == child.Parent.Visit {
				return false
			}
		}
		return true
	}
	return ref.Task != child.TaskID
}

func matchesChildWorkflow(j Job, s workflow.Stage, id string, revision int) bool {
	if s.Workflow == nil {
		return false
	}
	if s.Workflow.ID == "self" {
		return id == j.Document.ID && revision == j.Revision
	}
	return id == s.Workflow.ID && revision == s.Workflow.Revision
}

func selectMappedChildOutput(child Job, m workflow.ArtifactMap) *workflow.Evidence {
	typ := m.Type
	var outputSchema json.RawMessage
	if m.Output != "" {
		if child.Document.Signature == nil {
			return nil
		}
		declared, ok := child.Document.Signature.Outputs[m.Output]
		if !ok {
			return nil
		}
		typ = declared.Type
		outputSchema = declared.Schema
	}
	var found *workflow.Evidence
	for _, a := range latestInputs(child) {
		if (m.Output != "" || a.Stage == m.Stage) && (typ == "" || a.Type == typ) {
			if len(outputSchema) > 0 {
				raw, _ := workflow.EvidenceJSON(a)
				if workflow.ValidateArtifact(outputSchema, raw) != nil {
					continue
				}
			}
			copy := a
			found = &copy
		}
	}
	return found
}
func matchesParent(p *ParentRef, task, stage string, visit int) bool {
	return p != nil && p.Task == task && p.Stage == stage && p.Visit == visit && !p.Detached
}

func childBranchID(j Job) string {
	if j.BranchContext != nil {
		return j.BranchContext.Path
	}
	return ""
}

func publishChildAlias(j *Job, ref ChildRef, child Job, m workflow.ArtifactMap, found workflow.Evidence) error {
	if m.As == "" {
		return nil
	}
	typ, ok := j.Document.Artifacts[m.As]
	if !ok {
		return fmt.Errorf("unknown alias artifact %s", m.As)
	}
	raw, err := workflow.EvidenceJSON(found)
	if err != nil {
		return err
	}
	if err = workflow.ValidateArtifact(typ.Schema, raw); err != nil {
		return fmt.Errorf("child output alias %s: %w", m.As, err)
	}
	alias := found
	alias.TaskID = j.TaskID
	alias.Type = m.As
	alias.Version = typ.Version
	alias.Stage = ref.Stage
	alias.Tree = j.Tree
	alias.Role = "sub-workflow"
	alias.Session = child.TaskID
	alias.Provenance = true
	j.Evidence = append(j.Evidence, alias)
	j.Provenance = append(j.Provenance, Provenance{BranchID: childBranchID(*j), ExecutionKind: j.ExecutionKind, ArtifactID: alias.ID, DispatchID: child.TaskID, TaskID: j.TaskID, Stage: ref.Stage, Primitive: workflow.SubWorkflow, Role: "sub-workflow", Session: child.TaskID, Tree: j.Tree, Inputs: []string{found.ID}, ProducedAt: time.Now().UTC(), WorkflowHash: j.WorkflowHash, WorkflowRevision: j.Revision})
	return nil
}
