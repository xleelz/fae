package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SimulateScenario drives real engine Jobs, including their independent child
// evidence and fan frames. It never creates processes, files or provider calls.
func SimulateScenario(doc workflow.Document, task string, s workflow.Scenario, o workflow.SimulationOptions) (workflow.Simulation, error) {
	ceiling := workflow.RecursionCeiling(doc)
	if task == "" {
		task = s.Task
	}
	if task == "" {
		task = "simulation"
	}
	root, err := NewLive(task, 1, doc, "simulated-tree")
	if err != nil {
		return workflow.Simulation{}, err
	}
	root.TreeRoot, root.TreeDepth, root.MaxSessions = task, 1, -1
	scratch := s.ScratchRoot
	if scratch == "" {
		scratch = filepath.Join(os.TempDir(), "fae-rehearsal")
	}
	if err := scenarioDirectories(&root, nil, scratch); err != nil {
		return workflow.Simulation{}, err
	}
	if doc.MaxSessions > 0 {
		root.MaxSessions = doc.MaxSessions
	}
	footprintResolver := func(ref workflow.WorkflowRef) (workflow.Document, error) {
		if ref.ID == "self" {
			return doc, nil
		}
		if child, ok := s.Workflows[fmt.Sprintf("%s@%d", ref.ID, ref.Revision)]; ok {
			return child, nil
		}
		if ref.Revision == 1 {
			if child, ok := s.Workflows[ref.ID]; ok {
				return child, nil
			}
		}
		return workflow.Document{}, fmt.Errorf("unknown footprint child %s@%d", ref.ID, ref.Revision)
	}
	rootFootprint, footprintErr := workflow.DeriveFootprint(doc, footprintResolver, root.MaxSessions)
	if footprintErr != nil {
		return workflow.Simulation{}, footprintErr
	}
	root.Footprint = &rootFootprint
	if doc.Recursion != nil {
		root.RootMaxDescendants = doc.Recursion.MaxDescendants
	}
	root, err = BindTaskInputs(root, s.Inputs, nil)
	if err != nil {
		return workflow.Simulation{}, err
	}
	if o.Normal {
		root.Disposition = domain.Disposition{Mode: domain.Normal, Live: true, Destination: "simulated", PushNamespace: "simulated/"}
	} else {
		root.Disposition = domain.Disposition{Mode: domain.Local}
	}
	jobs := map[string]Job{task: root}
	executor := NewScenarioScripted(s)
	clock := newScenarioClock()
	generated := map[string][]string{}
	refusals := []workflow.SimulationRefusal{}
	branches := map[string]map[string]workflow.SimulationBranch{}
	var collectBranches func(Job)
	collectBranches = func(j Job) {
		if j.Fan == nil {
			return
		}
		for _, b := range j.Fan.Branches {
			path := b.ID
			if j.BranchContext != nil {
				path = j.BranchContext.Path + "/" + path
			}
			if branches[j.TaskID] == nil {
				branches[j.TaskID] = map[string]workflow.SimulationBranch{}
			}
			state := domain.State(b.Stage)
			if b.Frame != nil {
				state = b.Frame.State
			}
			qualified := fmt.Sprintf("%s@%d/%s", j.Fan.Stage, j.Fan.Visit, path)
			node := workflow.SimulationBranch{ID: qualified, FanStage: j.Fan.Stage, Visit: j.Fan.Visit, Path: path, Parent: j.TaskID, Outcome: string(b.Status), State: state}
			if b.Frame != nil {
				node.Evidence = b.Frame.Evidence
				collectBranches(*b.Frame)
			}
			branches[j.TaskID][qualified] = node
		}
	}

	admit := func(parent Job, ref ChildRef, inputs map[string]any, sources map[string][]string, detached bool, maxSessions int) (Job, error) {
		if detached && maxSessions != 0 {
			return Job{}, fmt.Errorf("binding: max_sessions may only be set on a root admission")
		}
		if len(ref.Task) > 2048 {
			return Job{}, fmt.Errorf("child task id exceeds2048 bytes")
		}
		childDoc := parent.Document
		if ref.WorkflowID != parent.Document.ID || ref.Revision != parent.Revision {
			var ok bool
			childDoc, ok = s.Workflows[fmt.Sprintf("%s@%d", ref.WorkflowID, ref.Revision)]
			if !ok && ref.Revision == 1 {
				childDoc, ok = s.Workflows[ref.WorkflowID]
			}
			if !ok || childDoc.ID != ref.WorkflowID {
				return Job{}, fmt.Errorf("unknown child workflow %s revision%d", ref.WorkflowID, ref.Revision)
			}
		}
		cycleBudgeted := childDoc.Recursion != nil
		for _, ancestorID := range append([]string{parent.TaskID}, parent.Ancestors...) {
			ancestor, exists := jobs[ancestorID]
			if exists {
				cycleBudgeted = cycleBudgeted && ancestor.Document.Recursion != nil
			}
			if exists && ancestor.Document.ID == childDoc.ID {
				if !cycleBudgeted {
					return Job{}, fmt.Errorf("recursion_budget: cycle requires budgets on every document")
				}
			}
		}
		depth := Depth(parent) + 1
		limit := 5
		if parent.Document.Recursion != nil {
			limit = parent.Document.Recursion.MaxDepth
		}
		childLimit := 5
		if childDoc.Recursion != nil {
			childLimit = childDoc.Recursion.MaxDepth
		}
		limit = min(limit, childLimit)
		if depth > limit || depth > ceiling {
			return Job{}, fmt.Errorf("recursion_budget: depth%d exceeds%d", depth, limit)
		}
		count := 0
		for _, j := range jobs {
			if RootTask(j) == RootTask(parent) && j.TaskID != RootTask(parent) {
				count++
			}
		}
		if treeRoot, exists := jobs[RootTask(parent)]; exists && treeRoot.Document.Recursion != nil && count >= treeRoot.Document.Recursion.MaxDescendants {
			return Job{}, fmt.Errorf("recursion_budget: max_descendants exceeded")
		}
		groups := map[string]int{}
		if parent.BranchContext != nil {
			groups = parent.BranchContext.VisitGroups
		}
		if !detached && parent.Document.Recursion != nil {
			counts := map[string]int{}
			for _, prior := range jobs {
				p := prior.Parent
				if p == nil || p.Task != parent.TaskID {
					continue
				}
				if p.Stage == ref.Stage && p.Visit == ref.Visit {
					counts["stage"]++
				}
				for k, v := range groups {
					if p.VisitGroups[k] == v {
						counts[k]++
					}
				}
			}
			for _, n := range counts {
				if n >= parent.Document.Recursion.MaxChildrenPerVisit {
					return Job{}, fmt.Errorf("recursion_budget: max_children_per_visit exceeded across fan branches")
				}
			}
		}
		childDoc.ValidationRecursionCeiling = ceiling
		child, err := NewLive(ref.Task, ref.Revision, childDoc, parent.Tree)
		if err != nil {
			return Job{}, err
		}
		child.Parent = &ParentRef{VisitGroups: groups, Task: parent.TaskID, Stage: ref.Stage, Visit: ref.Visit, BranchID: childBranchID(parent), Detached: detached}
		child.Detached = detached
		child.TreeRoot = RootTask(parent)
		child.TreeDepth = depth
		child.RootMaxDescendants = parent.RootMaxDescendants
		child.Ancestors = append([]string{parent.TaskID}, parent.Ancestors...)
		child.MaxSessions = parent.MaxSessions
		if err := scenarioDirectories(&child, &parent, scratch); err != nil {
			return Job{}, err
		}
		child.Disposition = parent.Disposition
		f, e := workflow.DeriveFootprint(child.Document, footprintResolver, child.MaxSessions)
		if e != nil {
			return Job{}, e
		}
		child.Footprint = &f
		return BindTaskInputs(child, inputs, sources)
	}
	applyActions := func(parent Job, d Dispatch, rule workflow.ScenarioRule) (Job, error) {
		for _, a := range rule.Detached {
			index := 1
			id := ""
			for {
				id = fmt.Sprintf("%s.detached.%d", parent.TaskID, index)
				if _, exists := jobs[id]; !exists {
					break
				}
				index++
			}
			inputs := map[string]any{}
			sources := map[string][]string{}
			for k, v := range a.Inputs {
				inputs[k] = v
			}
			for k, name := range a.FromParent {
				value, ids, e := scenarioParentValue(parent, name)
				if e != nil {
					return parent, e
				}
				if _, duplicate := inputs[k]; duplicate {
					return parent, fmt.Errorf("duplicate detached input %s", k)
				}
				inputs[k], sources[k] = value, ids
			}
			revision := a.Revision
			if revision == 0 {
				revision = 1
			}
			child, e := admit(parent, ChildRef{Task: id, WorkflowID: a.Workflow, Revision: revision}, inputs, sources, true, a.MaxSessions)
			if e != nil {
				refusals = append(refusals, workflow.SimulationRefusal{Parent: parent.TaskID, Task: id, Reason: e.Error()})
			} else {
				jobs[id] = child
			}
		}
		if request := rule.ChangeRequest; request != nil {
			target := request.Target
			if target == "parent" && parent.Parent != nil {
				target = parent.Parent.Task
			}
			valid := false
			for _, ancestor := range parent.Ancestors {
				valid = valid || ancestor == target
			}
			if !valid {
				return parent, fmt.Errorf("scenario change request target is not an ancestor")
			}
			r := AncestorChangeRequest{ID: d.ID + "/change-request", Source: parent.TaskID, Target: target, Artifact: request.Artifact, Value: request.Value, State: "pending", CreatedAt: time.Unix(0, 0).UTC()}
			parent.AncestorRequests = append(parent.AncestorRequests, r)
			events := []Event{}
			event(&parent, Event{Kind: "change-request", Stage: d.Stage, Request: &r, Cause: "scenario change request"}, &events)
		}
		return parent, nil
	}
	options := Options{ValidateChangeRequest: func(j Job, stage workflow.Stage) error {
		target := stage.Target
		if target == "parent" && j.Parent != nil {
			target = j.Parent.Task
		}
		ancestor := false
		for _, id := range j.Ancestors {
			ancestor = ancestor || id == target
		}
		parent, exists := jobs[target]
		if !ancestor || !exists {
			return fmt.Errorf("target is not an available ancestor")
		}
		if !workflow.DeclaresNamedArtifact(parent.Document, stage.Artifact) {
			return fmt.Errorf("ancestor has no declared artifact %s", stage.Artifact)
		}
		return nil
	}}
	for round := 0; round < 100000; round++ {
		ids := make([]string, 0, len(jobs))
		for id := range jobs {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		progress := false
		for _, id := range ids {
			j := jobs[id]
			j.IncomingRequests = scenarioIncoming(jobs, id)
			collectBranches(j)
			if j.State.Terminal() || j.State == domain.Escalated {
				continue
			}
			j.TreeDescendants = 0
			for _, other := range jobs {
				for _, ancestor := range other.Ancestors {
					if ancestor == id {
						j.TreeDescendants++
						break
					}
				}
			}
			visits := PendingSubWorkflowVisits(j)
			for _, visit := range visits {
				view := j
				if visit.BranchID != "" {
					var exists bool
					view, exists = BranchJob(j, visit.BranchID)
					if !exists {
						continue
					}
				}
				refs := view.Children()
				if len(refs) == 0 {
					refs = []ChildRef{visit.Ref}
				}
				loaded := make([]*Job, len(refs))
				allDone := true
				changed := false
				for i, ref := range refs {
					child, exists := jobs[ref.Task]
					if !exists {
						inputs, sources, e := ChildMappedInputValues(view, i)
						if e == nil {
							child, e = admit(view, ref, inputs, sources, false, view.MaxSessions)
						}
						if e != nil {
							refusals = append(refusals, workflow.SimulationRefusal{Parent: id, Task: ref.Task, Reason: e.Error()})
							if visit.BranchID != "" {
								j, _, err = FailBranchChild(j, visit.BranchID, e.Error())
							} else {
								j, _, err = ChildFailed(j, e.Error())
							}
							if err != nil {
								return workflow.Simulation{}, err
							}
							changed = true
							allDone = false
							break
						}
						jobs[ref.Task] = child
						allDone = false
						changed = true
						continue
					}
					if child.State == domain.Escalated && view.Document.Stages[ref.Stage].OnChildEscalation == "wait" {
						allDone = false
						continue
					}
					if !child.State.Terminal() && child.State != domain.Escalated {
						allDone = false
						continue
					}
					if child.State != domain.Done {
						reason := fmt.Sprintf("child %s ended %s", ref.Task, child.State)
						if visit.BranchID != "" {
							j, _, err = FailBranchChild(j, visit.BranchID, reason)
						} else {
							j, _, err = ChildFailed(j, reason)
						}
						changed = true
						allDone = false
						break
					}
					copy := child
					loaded[i] = &copy
				}
				if err != nil {
					return workflow.Simulation{}, err
				}
				if allDone {
					if visit.BranchID != "" {
						if len(loaded) > 1 {
							j, _, err = BindBranchChildren(j, visit.BranchID, loaded)
						} else {
							j, _, err = BindBranchChild(j, visit.BranchID, *loaded[0])
						}
					} else if len(loaded) > 1 {
						err = j.BindChildren(loaded)
					} else {
						j, _, err = BindChild(j, *loaded[0])
					}
					if err != nil {
						return workflow.Simulation{}, err
					}
					changed = true
				}
				jobs[id] = j
				progress = progress || changed
			}
			if len(visits) > 0 && j.Fan == nil {
				continue
			}
			decisions := PendingDecisions(j)
			for _, req := range decisions {
				d := Dispatch{TaskID: id, Stage: req.Stage, Visit: req.Visit, BranchID: req.Branch}
				rule, matched, e := scenarioRule(s, d, "")
				if e != nil {
					return workflow.Simulation{}, e
				}
				if matched && (rule.Decision != "" || rule.Items != nil) {
					j, _, _, e = Decide(j, Decision{ID: req.ID, Branch: req.Branch, Option: rule.Decision, Items: rule.Items, Message: "simulated", Actor: "operator"})
					if e != nil {
						return workflow.Simulation{}, e
					}
					jobs[id] = j
					progress = true
				}
			}
			if len(decisions) > 0 && j.Fan == nil {
				continue
			}
			if j.Parked {
				if j.Reason == ParkLoopBackoff {
					j.AdmissionRetryAfter = time.Time{}
					j.Parked = false
					j.Reason = ""
				} else {
					continue
				}
			}
			oldHistory := len(j.History)
			oldState := j.State
			var next Job
			var dispatches []*Dispatch
			var e error
			if j.Fan != nil && j.Fan.Parallel || j.Document.Stages[string(j.State)].Primitive == workflow.FanOut && j.Document.Stages[string(j.State)].Parallel {
				next, dispatches, _, e = PrepareParallelFanOut(j, options)
			} else {
				var dispatch *Dispatch
				next, dispatch, _, e = PrepareWith(j, options)
				if dispatch != nil {
					dispatches = append(dispatches, dispatch)
				}
			}
			if e != nil {
				return workflow.Simulation{}, e
			}
			j = next
			collectBranches(j)
			for _, dispatch := range dispatches {
				rule, _, e := scenarioRule(s, *dispatch, dispatch.Sessions[0].Role)
				if e != nil {
					return workflow.Simulation{}, e
				}
				j, e = applyActions(j, *dispatch, rule)
				if e != nil {
					return workflow.Simulation{}, e
				}
				clock.enqueue(*dispatch)
				progress = true
			}
			progress = progress || oldHistory != len(j.History) || oldState != j.State
			collectBranches(j)
			jobs[id] = j
		}
		started, err := clock.startReady(jobs, s)
		if err != nil {
			return workflow.Simulation{}, err
		}
		progress = progress || started
		if !progress {
			dispatch, complete, advanced := clock.advance()
			if advanced {
				if complete {
					j := jobs[dispatch.TaskID]
					r, e := executor.execute(context.Background(), dispatch, false)
					if e != nil {
						return workflow.Simulation{}, e
					}
					if dispatch.BranchID != "" {
						j, _, e = CompleteBranch(j, dispatch.BranchID, dispatch.ID, r)
					} else {
						j, _, e = Complete(j, dispatch.ID, r)
					}
					if e != nil {
						return workflow.Simulation{}, e
					}
					if executor.Generated(dispatch.ID) {
						generated[dispatch.TaskID] = append(generated[dispatch.TaskID], dispatch.ID)
					}
					jobs[dispatch.TaskID] = j
					collectBranches(j)
				}
				continue
			}
			result := scenarioReport(jobs, task, generated)
			clock.report(&result)
			result.Refusals = refusals
			for id, job := range jobs {
				for _, event := range job.History {
					if (event.Kind == "sub-workflow-failed" || event.Kind == "failure-route") && (strings.Contains(event.Cause, "recursion_budget") || strings.Contains(event.Cause, "width")) {
						duplicate := false
						for _, existing := range result.Refusals {
							if existing.Parent == id && strings.Contains(event.Cause, existing.Reason) {
								duplicate = true
							}
						}
						if !duplicate {
							result.Refusals = append(result.Refusals, workflow.SimulationRefusal{Parent: id, Task: event.DispatchID, Reason: event.Cause})
						}
					}
				}
			}
			sort.Slice(result.Refusals, func(i, j int) bool {
				if result.Refusals[i].Parent == result.Refusals[j].Parent {
					return result.Refusals[i].Task < result.Refusals[j].Task
				}
				return result.Refusals[i].Parent < result.Refusals[j].Parent
			})
			for i := range result.Tree {
				node := &result.Tree[i]
				paths := []string{}
				for path := range branches[node.Task] {
					paths = append(paths, path)
				}
				sort.Strings(paths)
				for _, path := range paths {
					branch := branches[node.Task][path]
					if node.State == domain.Done && branch.Outcome != "failed" && branch.Outcome != "skipped" {
						branch.Outcome = "passed"
					}
					node.Branches = append(node.Branches, branch)
				}
			}
			return result, nil
		}
	}
	return workflow.Simulation{}, fmt.Errorf("scenario exceeded100000 scheduler rounds")
}
func scenarioParentValue(j Job, name string) (any, []string, error) {
	if value, ok := j.Inputs[name]; ok {
		return value, []string{"input:" + name}, nil
	}
	if j.Document.Signature != nil {
		if output, ok := j.Document.Signature.Outputs[name]; ok {
			name = output.Type
		}
	}
	for i := len(j.Evidence) - 1; i >= 0; i-- {
		a := j.Evidence[i]
		if a.ID == name || a.Type == name || a.Stage == name {
			return a.Fields, []string{a.ID}, nil
		}
	}
	return nil, nil, fmt.Errorf("unknown parent value %s", name)
}
func scenarioReport(jobs map[string]Job, root string, generated map[string][]string) workflow.Simulation {
	j := jobs[root]
	result := workflow.Simulation{Footprint: j.Footprint, State: j.State, Reason: j.Reason, Evidence: j.Evidence, Records: j.GateRecords}
	ids := []string{}
	for id := range jobs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		task := jobs[id]
		node := workflow.SimulationTask{Workflow: task.Document.ID, Revision: task.Revision, Task: id, Root: RootTask(task), Depth: Depth(task), Detached: task.Detached, State: task.State, Reason: task.Reason, Evidence: task.Evidence, Generated: generated[id]}
		node.FailureRoutes, node.Refusals = FailureReports(task)
		for _, request := range scenarioIncoming(jobs, id) {
			raw, _ := json.Marshal(request)
			node.IncomingRequests = append(node.IncomingRequests, raw)
		}
		for _, request := range task.AncestorRequests {
			raw, _ := json.Marshal(request)
			node.ChangeRequests = append(node.ChangeRequests, raw)
		}
		if task.Parent != nil {
			node.Parent = task.Parent.Task
		}
		for _, e := range task.History {
			if e.Kind == "transition" {
				node.Path = append(node.Path, string(e.To))
				if id == root {
					result.Transitions = append(result.Transitions, workflow.Transition{From: e.From, To: e.To})
					result.Path = append(result.Path, string(e.To))
				}
			}
			if id == root && e.Kind == "gate" && e.Gate != nil {
				result.Gates = append(result.Gates, *e.Gate)
			}
		}
		result.Tree = append(result.Tree, node)
	}
	return result
}
