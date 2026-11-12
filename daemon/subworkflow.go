package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// maxSubWorkflowDepth bounds pinned sub-workflow nesting below a task.
const maxSubWorkflowDepth = 4

// checkSubWorkflows fails a parent admission closed when a pinned child
// workflow revision is unknown, retired, invalid or nested cyclically or too
// deeply (FAE-WF-007: never at bind).
func (d *Daemon) checkSubWorkflows(ctx context.Context, doc workflow.Document, chain []string) error {
	ids := make([]string, 0, len(doc.Stages))
	for id := range doc.Stages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		s := doc.Stages[id]
		if s.Primitive != workflow.SubWorkflow || s.Workflow == nil {
			continue
		}
		ref := *s.Workflow
		child := doc
		if ref.ID != "self" {
			if doc.Recursion == nil && len(chain) >= maxSubWorkflowDepth {
				return fmt.Errorf("sub-workflow %s of stage %s nests cyclically or deeper than %d", ref.ID, id, maxSubWorkflowDepth)
			}
			_, raw, err := d.store.GetWorkflow(ctx, ref.ID, int64(ref.Revision))
			if err != nil {
				return fmt.Errorf("stage %s pins unknown sub-workflow %s revision %d", id, ref.ID, ref.Revision)
			}
			if err = d.store.WorkflowActive(ctx, ref.ID); err != nil {
				return fmt.Errorf("stage %s pins retired sub-workflow %s: %w", id, ref.ID, err)
			}
			child, err = d.parseWorkflow(raw)
			if err != nil {
				return err
			}
		} else if doc.Recursion == nil {
			return fmt.Errorf("stage %s: self selection requires recursion", id)
		}
		var issues []string
		for _, issue := range workflow.ValidateChildBindings(doc, id, child) {
			issues = append(issues, issue.Error())
		}
		if child.Signature != nil {
			mapped := map[string]bool{}
			for _, m := range s.InputMap {
				mapped[m.Name] = true
				if _, ok := child.Signature.Inputs[m.Name]; !ok {
					issues = append(issues, "input_map "+m.Name+" names unknown child input")
				}
			}
			for name, schema := range child.Signature.Inputs {
				var spec map[string]json.RawMessage
				_ = json.Unmarshal(schema, &spec)
				_, hasDefault := spec["default"]
				if !mapped[name] && !hasDefault {
					issues = append(issues, "input_map missing required child input "+name)
				}
			}
		}
		for _, m := range s.OutputMap {
			if m.Output != "" {
				if child.Signature == nil {
					issues = append(issues, "output_map "+m.Name+" names undeclared child output "+m.Output)
				} else if output, ok := child.Signature.Outputs[m.Output]; !ok || m.Type != "" && m.Type != output.Type {
					issues = append(issues, "output_map "+m.Name+" names undeclared or mismatched child output "+m.Output)
				}
				continue
			}
			cs, ok := child.Stages[m.Stage]
			if !ok {
				issues = append(issues, "output_map "+m.Name+" names unknown stage "+m.Stage)
				continue
			}
			if m.Type != "" {
				if _, ok := child.Artifacts[m.Type]; !ok || (cs.Output != "" && cs.Output != m.Type) {
					issues = append(issues, "output_map "+m.Name+" names unknown or mismatched artifact type "+m.Type)
				}
			}
		}
		if len(issues) > 0 {
			sort.Strings(issues)
			return fmt.Errorf("stage %s in sub-workflow %s: %s", id, ref.ID, strings.Join(issues, "; "))
		}
		cycle := ref.ID == "self" || operatorListHas(append(chain, doc.ID), ref.ID)
		if cycle {
			if doc.Recursion == nil || child.Recursion == nil {
				return fmt.Errorf("sub-workflow %s nests cyclically without recursion budgets", ref.ID)
			}
			for _, ancestor := range chain {
				_, raw, err := d.storedWorkflow(ctx, ancestor, 0)
				if err != nil {
					return err
				}
				a, err := d.parseWorkflow(raw)
				if err != nil || a.Recursion == nil {
					return fmt.Errorf("mutual recursion through %s requires budgets on every document", ancestor)
				}
			}
			continue
		}
		if err := d.checkSubWorkflows(ctx, child, append(chain, doc.ID)); err != nil {
			return err
		}
	}
	return nil
}

// reconcileSubWorkflows runs each drive round under the writer lock. It
// admits the pinned child task(s) of every parent waiting at a SubWorkflow
// stage, binds finished child result(s) into the parent, and retires (as
// obsolete) every unfinished child whose parent no longer waits for it, so
// cancelling, retiring or restarting a parent cancels its children.
func (d *Daemon) reconcileSubWorkflows(ctx context.Context) error {
	if d.pendingCheckpoint != nil || d.draining {
		return nil
	}
	decoded := map[string]*envelope{}
	load := func(id string) *envelope {
		if e, seen := decoded[id]; seen {
			return e
		}
		var e *envelope
		if s, ok, _ := d.storedJob(ctx, id); ok {
			var x envelope
			if decodeEnvelope(s.Payload, &x) == nil {
				e = &x
			}
		}
		decoded[id] = e
		return e
	}
	ids := []string{}
	for id, s := range d.jobs {
		// Only unfinished tasks wait for a child or can be an obsolete child.
		if s.State != "Done" && s.State != "Retired" && s.State != "Cancelled" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if s, ok := d.jobs[id]; !ok || s.State == "Done" || s.State == "Retired" || s.State == "Cancelled" {
			delete(decoded, id)
			continue
		}
		e := load(id)
		if e == nil {
			continue
		}
		var err error
		if ref, waiting := engine.PendingChild(e.Job); waiting {
			err = d.admitOrBindSubWorkflow(ctx, id, *e, ref, load, decoded)
		} else if visits := engine.PendingSubWorkflowVisits(e.Job); len(visits) > 0 {
			err = d.reconcileBranchChildren(ctx, id, *e, visits, load, decoded)
		} else if p := e.Job.Parent; p != nil {
			var parent *engine.Job
			if pe := load(p.Task); pe != nil {
				parent = &pe.Job
			}
			if engine.Obsolete(e.Job, parent) && !d.active[id] {
				err = d.retireObsoleteChild(ctx, id, *e)
			}
		}
		delete(decoded, id) // a step may have checkpointed it
		if err != nil {
			if err = d.taskFault(ctx, id, err); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Daemon) admitOrBindSubWorkflow(ctx context.Context, id string, e envelope, ref engine.ChildRef, load func(string) *envelope, decoded map[string]*envelope) error {
	childrenRefs := e.Job.Children()
	if len(childrenRefs) <= 1 {
		err := d.serveChild(ctx, id, e, ref, load(ref.Task))
		delete(decoded, ref.Task)
		return err
	}

	items, err := engine.ChildInputItems(&e.Job)
	if err != nil {
		return taskErr("sub-workflow", err)
	}
	if len(items) != len(childrenRefs) {
		return taskErr("sub-workflow", fmt.Errorf("child input items (%d) do not match children count (%d)", len(items), len(childrenRefs)))
	}

	// 1. Check whether any already-admitted child ended without Done (Failed / Cancelled / Retired).
	for _, childRef := range childrenRefs {
		ce := load(childRef.Task)
		if ce == nil || (!ce.Job.State.Terminal() && ce.Job.State != domain.Escalated) {
			continue
		}
		if ce.Job.State == domain.Escalated && e.Job.Document.Stages[ref.Stage].OnChildEscalation == "wait" {
			continue
		}
		if ce.Job.State != domain.Done {
			events, failErr := e.Job.FailChild(childRef.Task, fmt.Sprintf("child %s ended %s: %s", childRef.Task, ce.Job.State, ce.Job.Reason))
			if failErr != nil {
				return taskErr("sub-workflow", failErr)
			}
			saved, persistErr := d.persist(ctx, d.jobs[id], e, events)
			if persistErr != nil {
				return persistErr
			}
			d.jobs[id] = saved
			if err := d.retireObsoleteChildren(ctx, id, childrenRefs, load, decoded); err != nil {
				return err
			}
			d.signal()
			return nil
		}
	}

	// 2. Admit any child in childrenRefs that has not yet been admitted.
	allDone := true
	loadedChildren := make([]*engine.Job, len(childrenRefs))
	for idx, childRef := range childrenRefs {
		ce := load(childRef.Task)
		if ce == nil {
			allDone = false
			if _, taken := d.jobs[childRef.Task]; taken || d.quarantined[childRef.Task] != "" {
				continue
			}
			if _, err := d.store.GetJob(ctx, childRef.Task); err == nil {
				continue
			} else if !errors.Is(err, sql.ErrNoRows) {
				return &controlplane.RetryableError{Attempts: 1, Cause: err}
			}
			if err := d.admitChildWithContent(ctx, id, e, childRef, items[idx]); err != nil {
				return err
			}
			delete(decoded, childRef.Task)
			// Reload parent in case admission refusal failed the parent visit.
			if snap, ok := d.jobs[id]; ok {
				var updated envelope
				if decodeEnvelope(snap.Payload, &updated) == nil {
					if _, stillWaiting := engine.PendingChild(updated.Job); !stillWaiting {
						return d.retireObsoleteChildren(ctx, id, childrenRefs, load, decoded)
					}
				}
			}
			continue
		}
		if !ce.Job.State.Terminal() || ce.Job.State != domain.Done {
			allDone = false
			continue
		}
		jobCopy := ce.Job
		loadedChildren[idx] = &jobCopy
	}

	if !allDone {
		return nil
	}

	// 3. All children in parent.Children are Done: bind their aggregated outputs and advance the parent.
	prevLen := len(e.Job.History)
	var events []engine.Event
	if err := e.Job.BindChildren(loadedChildren); err != nil {
		var failErr error
		events, failErr = e.Job.FailChild(ref.Task, "children cannot be bound: "+err.Error())
		if failErr != nil {
			return taskErr("sub-workflow", failErr)
		}
	} else {
		events = e.Job.History[prevLen:]
	}
	saved, err := d.persist(ctx, d.jobs[id], e, events)
	if err != nil {
		return err
	}
	d.jobs[id] = saved
	d.signal()
	return nil
}

func (d *Daemon) serveChild(ctx context.Context, id string, e envelope, ref engine.ChildRef, child *envelope) error {
	if child == nil {
		if _, taken := d.jobs[ref.Task]; taken || d.quarantined[ref.Task] != "" {
			return nil // undecodable or quarantined child: the operator decides
		}
		if _, err := d.store.GetJob(ctx, ref.Task); err == nil {
			return nil // durable but uncached: never re-admit over it
		} else if !errors.Is(err, sql.ErrNoRows) {
			return &controlplane.RetryableError{Attempts: 1, Cause: err}
		}
		return d.admitChild(ctx, id, e, ref)
	}
	if child.Job.State == domain.Escalated && e.Job.Document.Stages[ref.Stage].OnChildEscalation == "wait" {
		return nil
	}
	if !child.Job.State.Terminal() && child.Job.State != domain.Escalated {
		return nil
	}
	next, events, err := engine.BindChild(e.Job, child.Job)
	if err != nil {
		next, events, err = engine.ChildFailed(e.Job, "child "+ref.Task+" cannot be bound: "+err.Error())
		if err != nil {
			return taskErr("sub-workflow", err)
		}
	}
	e.Job = next
	saved, err := d.persist(ctx, d.jobs[id], e, events)
	if err != nil {
		return err
	}
	d.jobs[id] = saved
	d.signal()
	return nil
}

// admitChild admits the pinned child task through the ordinary admission
// path: its own binding to the child workflow revision, Local disposition, the
// parent's run, priority and target, the parent's resolved file scope as its
// declared files (subWorkflowChildFiles), and the mapped parent artifacts as
// its structured content. A refused admission fails the parent's visit.
func (d *Daemon) admitChild(ctx context.Context, id string, e envelope, ref engine.ChildRef) error {
	content, err := engine.ChildInputs(e.Job)
	if err != nil {
		return taskErr("sub-workflow", err)
	}
	return d.admitChildWithContent(ctx, id, e, ref, content)
}

func (d *Daemon) admitChildWithContent(ctx context.Context, id string, e envelope, ref engine.ChildRef, content json.RawMessage) error {
	a := e.Admission
	files, err := d.subWorkflowChildFiles(ctx, id, e)
	p := AdmitParams{ID: ref.Task, WorkflowID: ref.WorkflowID, Revision: int64(ref.Revision), Tree: e.Job.Tree, Title: "sub-workflow " + ref.Stage + " of " + id, Content: string(content), Priority: a.Priority, RunID: a.RunID, Mode: "Local", TargetConfig: a.TargetConfig, TargetRevision: a.TargetRevision, AuthorizingMessage: a.AuthorizingMessage, DeclaredFiles: files}
	p.ParentTask = id
	childContext := childAdmissionContext{VisitView: &e.Job, Parent: engine.ParentRef{Task: id, Stage: ref.Stage, Visit: ref.Visit}, Sources: map[string][]string{}}
	if e.Job.BranchContext != nil {
		childContext.Parent.BranchID = e.Job.BranchContext.Path
		childContext.Parent.VisitGroups = e.Job.BranchContext.VisitGroups
	}
	if stage := e.Job.Document.Stages[ref.Stage]; stage.Reads != nil {
		p.Reads = stage.Reads
	}
	p.ReadOnlyFiles = append([]string{}, e.Job.Document.Stages[ref.Stage].ReadOnlyFiles...)
	_, childRaw, lookupErr := d.storedWorkflow(ctx, ref.WorkflowID, int64(ref.Revision))
	if lookupErr == nil {
		childDoc, parseErr := d.parseWorkflow(childRaw)
		if parseErr == nil && childDoc.Signature != nil {
			index := 0
			for i, c := range e.Job.Children() {
				if c.Task == ref.Task {
					index = i
					break
				}
			}
			p.Inputs, childContext.Sources, err = engine.ChildMappedInputValues(e.Job, index)
		}
	}
	raw, _ := json.Marshal(p)
	var snapshot controlplane.JobSnapshot
	if err == nil {
		snapshot, err = d.prepareAdmission(context.WithValue(ctx, childAdmissionKey{}, childContext), raw)
	}
	if err == nil {
		var child envelope
		if err = decodeEnvelope(snapshot.Payload, &child); err == nil {
			parentRef := childContext.Parent
			child.Job.Parent = &parentRef
			snapshot.Payload, err = json.Marshal(child)
		}
	}
	var saved controlplane.JobSnapshot
	if err == nil {
		saved, err = d.writeAdmissionRow(ctx, controlplane.Audit{Actor: "daemon", Action: "SubWorkflowAdmit", MessageID: a.AuthorizingMessage, Inputs: raw, Outcome: "ok"}, ref.Task, snapshot)
	}
	var transient *controlplane.RetryableError
	if errors.As(err, &transient) || ctx.Err() != nil {
		return err
	}
	if err != nil {
		next, events, failErr := engine.ChildFailed(e.Job, "child "+ref.Task+" admission refused: "+err.Error())
		if e.Job.BranchContext != nil {
			var root envelope
			if decodeErr := decodeEnvelope(d.jobs[id].Payload, &root); decodeErr != nil {
				return decodeErr
			}
			next, events, failErr = engine.FailBranchChild(root.Job, e.Job.BranchContext.Path, "child "+ref.Task+" admission refused: "+err.Error())
			e = root
		}
		if failErr != nil {
			return taskErr("sub-workflow", failErr)
		}
		e.Job = next
		saved, err = d.persist(ctx, d.jobs[id], e, events)
		if err != nil {
			return err
		}
		d.jobs[id] = saved
		return nil
	}
	if _, exists := d.jobs[ref.Task]; !exists {
		d.jobs[ref.Task] = saved
	}
	d.notifyEvents()
	d.signal()
	return nil
}

// subWorkflowChildFiles are a SubWorkflow child's declared files: its
// parent's file scope when the child is admitted, resolved by the one scope
// resolver (the parent's pinned files, else its admission's or its latest
// Spec's declared files, plus its approved scope widens). The child task runs
// on the parent's behalf (FAE-WF-030 maps the parent's artifacts in and the
// child's back), so it works within the parent's reviewed scope; a derived
// scope child no longer starts with no files, which refused every change
// turn. Without live execution the same derived rule applies with no pinned
// configuration. A parent with no files yet gives none.
func (d *Daemon) subWorkflowChildFiles(ctx context.Context, id string, e envelope) ([]string, error) {
	x := &liveExecutor{}
	if d.live != nil {
		selected, err := d.liveForAdmission(ctx, e.Admission)
		if err != nil {
			return nil, err
		}
		x = selected
	}
	scope, err := x.resolveLiveTaskScope(id, e.Job.ApprovedScope, workflow.Stage{}, func() (envelope, error) { return e, nil })
	if err != nil || len(scope.Files) == 0 {
		return nil, err
	}
	return scope.Files, nil
}

func (d *Daemon) retireObsoleteChildren(ctx context.Context, _ string, children []engine.ChildRef, load func(string) *envelope, decoded map[string]*envelope) error {
	for _, c := range children {
		ce := load(c.Task)
		if ce == nil {
			if s, ok, _ := d.storedJob(ctx, c.Task); ok {
				var x envelope
				if decodeEnvelope(s.Payload, &x) == nil {
					ce = &x
				}
			}
		}
		if ce == nil || ce.Job.State.Terminal() || d.active[c.Task] {
			continue
		}
		if err := d.retireObsoleteChild(ctx, c.Task, *ce); err != nil {
			return err
		}
		delete(decoded, c.Task)
	}
	return nil
}

// retireObsoleteChild escalates, then retires as obsolete, a child task whose
// parent no longer waits for it. Each step is an ordinary audited checkpoint.
func (d *Daemon) retireObsoleteChild(ctx context.Context, id string, e envelope) error {
	now := time.Now().UTC()
	reason := "parent " + e.Job.Parent.Task + " no longer waits for this sub-workflow"
	for _, c := range []engine.Control{{Method: "Escalate", Reason: reason}, {Method: "Retire", Reason: "obsolete " + reason}} {
		if c.Method == "Escalate" && e.Job.State == "Escalated" {
			continue
		}
		if c.Method == "Retire" && e.Job.InFlight != nil {
			break // held turn: retired once its dispatch is resolved
		}
		next, events, err := engine.ApplyControl(e.Job, c, now)
		if err != nil {
			return taskErr("sub-workflow", err)
		}
		e.Job = next
		saved, err := d.persist(ctx, d.jobs[id], e, events)
		if err != nil {
			return err
		}
		d.jobs[id] = saved
	}
	d.publishStatus()
	return nil
}
