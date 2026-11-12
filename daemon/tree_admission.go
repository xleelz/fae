package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

type childAdmissionContext struct {
	VisitView *engine.Job
	Parent    engine.ParentRef
	Sources   map[string][]string
}
type childAdmissionKey struct{}
type treeAdmissionReservationsKey struct{}
type treeAdmission struct {
	Parent      *envelope
	Root        *envelope
	Inputs      map[string]any
	Sources     map[string][]string
	Descendants int
	Depth       int
	Detached    bool
}

func (d *Daemon) nextDetachedID(ctx context.Context, parent string) (string, error) {
	for n := 1; n <= 100000; n++ {
		id := fmt.Sprintf("%s.detached.%d", parent, n)
		_, exists, err := d.storedJob(ctx, id)
		if err != nil {
			return "", err
		}
		if !exists {
			return id, nil
		}
	}
	return "", errors.New("recursion_budget: no detached child identity available")
}
func (d *Daemon) taskEnvelope(ctx context.Context, id string) (*envelope, error) {
	s, found, err := d.storedJob(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("unknown parent task %s", id)
	}
	var e envelope
	if err = decodeEnvelope(s.Payload, &e); err != nil {
		return nil, err
	}
	return &e, nil
}
func (d *Daemon) treeDescendantCount(ctx context.Context, root string) (int, error) {
	rows, err := d.store.ListJobs(ctx, 100000)
	if err != nil {
		return 0, err
	}
	if len(rows) >= 100000 {
		return 0, errors.New("recursion_budget: tree census exceeds bounded admission read")
	}
	known := map[string]engine.Job{}
	for _, s := range rows {
		var e envelope
		if decodeEnvelope(s.Payload, &e) == nil {
			known[s.ID] = e.Job
		}
	}
	for id, s := range d.jobs {
		var e envelope
		if decodeEnvelope(s.Payload, &e) == nil {
			known[id] = e.Job
		}
	}
	count := 0
	for id, j := range known {
		if id != root && engine.RootTask(j) == root {
			count++
		}
	}
	return count, nil
}
func parentArtifact(j engine.Job, name string) (any, []string, error) {
	if name == "tree" {
		return engine.TreeValue(j), nil, nil
	}
	inputName := strings.TrimPrefix(name, "input:")
	if v, ok := j.Inputs[inputName]; ok {
		for _, a := range j.Evidence {
			if a.Stage == "input:"+inputName {
				return v, []string{a.ID}, nil
			}
		}
	}
	typ := ""
	if j.Document.Signature != nil {
		if o, ok := j.Document.Signature.Outputs[name]; ok {
			typ = o.Type
		}
	}
	for i := len(j.Evidence) - 1; i >= 0; i-- {
		a := j.Evidence[i]
		if a.ID == name || a.Stage == name || a.Type == name || typ != "" && a.Type == typ {
			value := any(a.Fields)
			if len(a.Value) > 0 {
				decoder := json.NewDecoder(strings.NewReader(string(a.Value)))
				decoder.UseNumber()
				if err := decoder.Decode(&value); err != nil {
					return nil, nil, err
				}
			}
			if typ != "" && j.Document.Signature != nil {
				if out, ok := j.Document.Signature.Outputs[name]; ok && len(out.Schema) > 0 {
					raw, _ := json.Marshal(value)
					if workflow.ValidateArtifact(out.Schema, raw) != nil {
						continue
					}
				}
			}
			return value, []string{a.ID}, nil
		}
	}
	return nil, nil, fmt.Errorf("unknown parent artifact or input %s", name)
}
func (d *Daemon) prepareTreeAdmission(ctx context.Context, p *AdmitParams, doc workflow.Document) (treeAdmission, error) {
	result := treeAdmission{Depth: 1, Sources: map[string][]string{}}
	issues := []string{}
	supplied := map[string]any{}
	for name, v := range p.Inputs {
		supplied[name] = v
	}
	internal, sub := ctx.Value(childAdmissionKey{}).(childAdmissionContext)
	if p.ParentTask != "" && p.maxSessionsSet {
		issues = append(issues, "binding: max_sessions may only be set on a root admission")
	}
	if p.ParentTask != "" && !sub && !strings.HasPrefix(p.ID, p.ParentTask+".detached.") {
		issues = append(issues, "binding: detached id must start with "+p.ParentTask+".detached.")
	}
	if sub {
		for n, ids := range internal.Sources {
			result.Sources[n] = ids
		}
	}
	if p.ParentTask != "" {
		parent, err := d.taskEnvelope(ctx, p.ParentTask)
		if err != nil {
			issues = append(issues, "parent_task: "+err.Error())
		} else {
			result.Parent = parent
			result.Depth = engine.Depth(parent.Job) + 1
			result.Detached = !sub
			if !sub {
				if p.Tree != "" && p.Tree != parent.Job.Tree {
					issues = append(issues, "binding: detached tree must equal parent's last recorded tree")
				}
				p.Tree = parent.Job.Tree
			}
			root := parent
			if engine.RootTask(parent.Job) != parent.Job.TaskID {
				root, err = d.taskEnvelope(ctx, engine.RootTask(parent.Job))
				if err != nil {
					issues = append(issues, "parent_task.root: "+err.Error())
				}
			}
			result.Root = root
			if sub && parent.Job.Document.Recursion != nil {
				counts := map[string]int{}
				rows, readErr := d.store.TreeJobs(ctx, parent.Job.TaskID)
				if readErr != nil {
					issues = append(issues, "recursion_budget: "+readErr.Error())
				} else {
					for _, row := range rows {
						var child envelope
						if decodeEnvelope(row.Payload, &child) != nil || child.Job.Parent == nil || child.Job.Parent.Task != parent.Job.TaskID {
							continue
						}
						prev := child.Job.Parent
						if prev.Stage == internal.Parent.Stage && prev.Visit == internal.Parent.Visit {
							counts["stage"]++
						}
						for k, v := range internal.Parent.VisitGroups {
							if prev.VisitGroups[k] == v {
								counts[k]++
							}
						}
					}
					for _, count := range counts {
						if count >= parent.Job.Document.Recursion.MaxChildrenPerVisit {
							issues = append(issues, "recursion_budget: max_children_per_visit exceeded across visit branches")
							break
						}
					}
				}
			}
			for name, source := range p.FromParent {
				if _, dup := supplied[name]; dup {
					issues = append(issues, "inputs."+name+": supplied directly and through from_parent")
					continue
				}
				v, ids, err := parentArtifact(parent.Job, source)
				if err != nil {
					issues = append(issues, "from_parent."+name+": "+err.Error())
					continue
				}
				supplied[name] = v
				result.Sources[name] = ids
			}
			parentLimit := 5
			if parent.Job.Document.Recursion != nil {
				parentLimit = parent.Job.Document.Recursion.MaxDepth
			}
			childLimit := 5
			if doc.Recursion != nil {
				childLimit = doc.Recursion.MaxDepth
			}
			if result.Depth > d.recursionCeiling() || result.Depth > parentLimit || result.Depth > childLimit {
				issues = append(issues, "recursion_budget: max_depth exceeded")
			}
			ancestor := parent
			cycleBudgeted := doc.Recursion != nil
			for visited := 0; ancestor != nil && visited < d.recursionCeiling(); visited++ {
				cycleBudgeted = cycleBudgeted && ancestor.Job.Document.Recursion != nil
				if ancestor.Job.Document.ID == doc.ID && !cycleBudgeted {
					issues = append(issues, "recursion_budget: cyclic child requires recursion on every document")
					break
				}
				if ancestor.Job.Parent == nil {
					break
				}
				next, lookupErr := d.taskEnvelope(ctx, ancestor.Job.Parent.Task)
				if lookupErr != nil {
					issues = append(issues, lookupErr.Error())
					break
				}
				ancestor = next
			}
			if parent.Job.Document.ID == doc.ID && (parent.Job.Document.Recursion == nil || doc.Recursion == nil) {
				issues = append(issues, "recursion_budget: self child requires recursion on both documents")
			}
			if root != nil {
				count, err := d.treeDescendantCount(ctx, root.Job.TaskID)
				if err != nil {
					issues = append(issues, err.Error())
				} else {
					if reserved, ok := ctx.Value(treeAdmissionReservationsKey{}).(map[string]int); ok {
						count += reserved[root.Job.TaskID]
					}
					result.Descendants = count
					if root.Job.Document.Recursion != nil && count >= root.Job.Document.Recursion.MaxDescendants {
						issues = append(issues, "recursion_budget: max_descendants exceeded")
					}
				}
			}
		}
	} else if len(p.FromParent) > 0 {
		issues = append(issues, "from_parent: parent_task required")
	}
	if p.MaxSessions == 0 {
		p.MaxSessions = doc.MaxSessions
		if p.MaxSessions == 0 {
			p.MaxSessions = -1
		}
	}
	if p.MaxSessions < -1 || p.MaxSessions == 0 {
		issues = append(issues, "max_sessions: must be -1 or positive")
	}
	bound, inputIssues := workflow.BindInputs(doc, supplied)
	for _, issue := range inputIssues {
		issues = append(issues, issue.Error())
	}
	result.Inputs = bound
	if len(issues) > 0 {
		sort.Strings(issues)
		return result, errors.New(strings.Join(issues, "; "))
	}
	p.Inputs = bound
	return result, nil
}
func applyTreeAdmission(j *engine.Job, p AdmitParams, t treeAdmission, ctx context.Context) {
	j.TreeRoot = j.TaskID
	j.TreeDepth = 1
	j.MaxSessions = p.MaxSessions
	j.TreeDescendants = t.Descendants
	if j.Document.Recursion != nil {
		j.RootMaxDescendants = j.Document.Recursion.MaxDescendants
	}
	if t.Parent != nil {
		j.TreeRoot = engine.RootTask(t.Parent.Job)
		j.TreeDepth = t.Depth
		j.Detached = t.Detached
		j.Parent = &engine.ParentRef{Task: t.Parent.Job.TaskID, Detached: t.Detached}
		j.Ancestors = append([]string{t.Parent.Job.TaskID}, t.Parent.Job.Ancestors...)
		if internal, ok := ctx.Value(childAdmissionKey{}).(childAdmissionContext); ok {
			copy := internal.Parent
			j.Parent = &copy
		}
		if t.Root != nil && t.Root.Job.Document.Recursion != nil {
			j.RootMaxDescendants = t.Root.Job.Document.Recursion.MaxDescendants
		}
		if t.Root != nil {
			j.MaxSessions = t.Root.Job.MaxSessions
		}
	}
}

// admissionTreeEvents records the link without editing the parent's task state.
func admissionTreeEvents(j engine.Job) []engine.Event {
	if j.Parent == nil {
		return nil
	}
	return []engine.Event{{Kind: "child-admitted", Stage: "admission", Attempt: 1, Cause: fmt.Sprintf("parent=%s root=%s depth=%d detached=%t", j.Parent.Task, engine.RootTask(j), engine.Depth(j), j.Detached)}}
}

func admissionEvents(j engine.Job) []controlplane.Event {
	if j.Parent == nil {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"child_task": j.TaskID, "parent_task": j.Parent.Task, "tree_root": engine.RootTask(j), "depth": engine.Depth(j), "detached": j.Detached})
	return []controlplane.Event{{Kind: "child-admitted", JobID: j.TaskID, Payload: raw}, {Kind: "child-admitted", JobID: j.Parent.Task, Payload: raw}}
}
func samePinnedJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
