package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// reconcileBranchChildren advances one branch visit at a time, checkpointing
// the enclosing task rather than replacing it with a branch execution view.
func (d *Daemon) reconcileBranchChildren(ctx context.Context, id string, root envelope, visits []engine.ChildVisit, load func(string) *envelope, decoded map[string]*envelope) error {
	for _, visit := range visits {
		view, ok := engine.BranchJob(root.Job, visit.BranchID)
		if !ok {
			continue
		}
		refs := view.Children()
		if len(refs) == 0 {
			refs = []engine.ChildRef{visit.Ref}
		}
		children := make([]*engine.Job, len(refs))
		allDone := true
		var items []json.RawMessage
		if len(refs) > 1 {
			var err error
			items, err = engine.ChildInputItems(&view)
			if err != nil {
				return err
			}
			if len(items) != len(refs) {
				return fmt.Errorf("branch child item count mismatch")
			}
		}
		for i, ref := range refs {
			child := load(ref.Task)
			if child == nil {
				branch := root
				branch.Job = view
				var err error
				if len(refs) > 1 {
					err = d.admitChildWithContent(ctx, id, branch, ref, items[i])
				} else {
					err = d.admitChild(ctx, id, branch, ref)
				}
				delete(decoded, ref.Task)
				return err // admission may have routed refusal in root; reload next round
			}
			if child.Job.State == domain.Escalated && view.Document.Stages[ref.Stage].OnChildEscalation == "wait" {
				allDone = false
				continue
			}
			if !child.Job.State.Terminal() && child.Job.State != domain.Escalated {
				allDone = false
				continue
			}
			if child.Job.State != domain.Done {
				next, events, err := engine.FailBranchChild(root.Job, visit.BranchID, fmt.Sprintf("child %s ended %s: %s", ref.Task, child.Job.State, child.Job.Reason))
				if err != nil {
					return err
				}
				root.Job = next
				saved, err := d.persist(ctx, d.jobs[id], root, events)
				if err == nil {
					d.jobs[id] = saved
					d.signal()
				}
				return err
			}
			copy := child.Job
			children[i] = &copy
		}
		if allDone {
			var next engine.Job
			var events []engine.Event
			var err error
			if len(children) > 1 {
				next, events, err = engine.BindBranchChildren(root.Job, visit.BranchID, children)
			} else {
				next, events, err = engine.BindBranchChild(root.Job, visit.BranchID, *children[0])
			}
			if err != nil {
				next, events, err = engine.FailBranchChild(root.Job, visit.BranchID, "child cannot be bound: "+err.Error())
			}
			if err != nil {
				return err
			}
			root.Job = next
			saved, err := d.persist(ctx, d.jobs[id], root, events)
			if err == nil {
				d.jobs[id] = saved
				d.signal()
			}
			return err
		}
	}
	return nil
}
