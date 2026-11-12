package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// Each branch carries an isolated execution view. Its descendants never receive
// sibling evidence; global dispatch sequence and task attempts are synchronized
// before preparation. The views are checkpointed, not goroutine-local state.
type BranchContext struct {
	Siblings    []any          `json:"siblings,omitempty"`
	VisitGroups map[string]int `json:"visit_groups,omitempty"`
	IndexPath   []int          `json:"index_path,omitempty"`
	Path        string         `json:"path"`
	Fan         string         `json:"fan"`
	Item        any            `json:"item,omitempty"`
	WriteScope  []string       `json:"write_scope,omitempty"`
}

func nestedRegion(d workflow.Document, fan string) bool {
	for stage := range workflow.BranchOf(d, fan) {
		p := d.Stages[stage].Primitive
		if p == workflow.FanOut || p == workflow.SubWorkflow || p == workflow.HumanDecision {
			return true
		}
	}
	return false
}

func copyCounters(in map[string]int) map[string]int {
	out := map[string]int{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mergeBranchView(root *Job, view Job, events []Event, before ...Job) {
	if root.BranchVisits == nil {
		root.BranchVisits = map[string]map[string]int{}
	}
	for key, counts := range view.BranchVisits {
		if root.BranchVisits[key] == nil {
			root.BranchVisits[key] = map[string]int{}
		}
		for stage, count := range counts {
			root.BranchVisits[key][stage] = max(root.BranchVisits[key][stage], count)
		}
	}
	if view.BranchContext != nil {
		if root.BranchVisits == nil {
			root.BranchVisits = map[string]map[string]int{}
		}
		key := branchCounterKey(view.BranchContext)
		root.BranchVisits[key] = copyCounters(view.Counts)
	}
	root.Sequence = max(root.Sequence, view.Sequence)
	if len(before) > 0 {
		old := before[0]
		root.Attempts = max(0, root.Attempts+view.Attempts-old.Attempts)
		if view.Attempts > old.Attempts {
			root.NewAttempt = false
		}
		root.TestRuns = max(0, root.TestRuns+view.TestRuns-old.TestRuns)
		for k, v := range view.Counts {
			root.Counts[k] = max(0, root.Counts[k]+v-old.Counts[k])
		}
	} else {
		root.Attempts = max(root.Attempts, view.Attempts)
		root.TestRuns = max(root.TestRuns, view.TestRuns)
		for k, v := range view.Counts {
			root.Counts[k] = max(root.Counts[k], v)
		}
	}
	for _, stat := range view.TurnStats {
		exists := false
		for _, old := range root.TurnStats {
			if old.Dispatch == stat.Dispatch && old.Session == stat.Session && old.Attempt == stat.Attempt {
				exists = true
			}
		}
		if !exists {
			root.TurnStats = append(root.TurnStats, stat)
		}
	}
	seen := map[string]bool{}
	for _, a := range root.Evidence {
		seen[a.Stage+"\x00"+a.Session+"\x00"+a.ID] = true
	}
	for _, a := range view.Evidence {
		key := a.Stage + "\x00" + a.Session + "\x00" + a.ID
		if !seen[key] {
			root.Evidence = append(root.Evidence, a)
			seen[key] = true
		}
	}
	known := map[string]bool{}
	for _, p := range root.Provenance {
		known[p.DispatchID+"\x00"+p.ArtifactID+"\x00"+p.Session] = true
	}
	for _, p := range view.Provenance {
		key := p.DispatchID + "\x00" + p.ArtifactID + "\x00" + p.Session
		if !known[key] {
			root.Provenance = append(root.Provenance, p)
			known[key] = true
		}
	}
	for i, e := range events {
		if e.Kind == "transition" || e.Kind == "fan-cursor" {
			e.Kind = "branch-transition"
		}
		if e.BranchID == "" && view.BranchContext != nil {
			e.BranchID = view.BranchContext.Path
		}
		events[i] = e
		root.History = append(root.History, e)
	}
	for _, r := range view.AncestorRequests {
		found := false
		for _, prior := range root.AncestorRequests {
			if prior.ID == r.ID {
				found = true
			}
		}
		if !found {
			root.AncestorRequests = append(root.AncestorRequests, r)
		}
	}
	for _, r := range view.GateRecords {
		found := false
		for _, prior := range root.GateRecords {
			if prior.ID == r.ID {
				found = true
			}
		}
		if !found {
			root.GateRecords = append(root.GateRecords, r)
		}
	}
}

func branchPath(prefix, id string) string {
	if prefix == "" {
		return id
	}
	return prefix + "/" + id
}

func prepareNestedFan(input Job, o Options, limit int, only string) (Job, []*Dispatch, []Event, error) {
	if input.State.Kernel() || input.OperatorStage != "" || input.Parked || input.Deferred {
		return input, nil, nil, nil
	}
	j := clone(input)
	f := j.Fan
	if f == nil || f.Baseline == nil {
		return j, nil, nil, errors.New("missing nested fan checkpoint")
	}
	var dispatches []*Dispatch
	var events []Event
	prefix := ""
	if j.BranchContext != nil {
		prefix = j.BranchContext.Path
	}
	for i := range f.Branches {
		b := &f.Branches[i]
		path := branchPath(prefix, b.ID)
		if only != "" && only != b.ID && only != path && !strings.HasPrefix(only, path+"/") {
			continue
		}
		if b.Status == BranchPassed || b.Status == BranchFailed || b.Status == BranchSkipped || b.Status == BranchCancelled {
			continue
		}
		if j.BranchContext != nil && j.BranchContext.WriteScope != nil {
			for _, g := range b.WriteScope {
				narrowed := false
				for _, outer := range j.BranchContext.WriteScope {
					if workflow.GlobScopeNarrows([]string{g}, []string{outer}) {
						narrowed = true
					}
				}
				if !narrowed {
					return input, nil, nil, fmt.Errorf("nested branch %s scope %q widens enclosing branch", path, g)
				}
			}
		}
		if b.Frame == nil {
			view := clone(*f.Baseline)
			if err := enter(&view, domain.State(b.Start)); err != nil {
				return input, nil, nil, err
			}
			view.Fan = nil
			view.InFlight = nil
			view.Parked = false
			view.Reason = ""
			view.Child = nil
			indices := []int{}
			if j.BranchContext != nil {
				indices = append(indices, j.BranchContext.IndexPath...)
			}
			indices = append(indices, i)
			groups := map[string]int{}
			if j.BranchContext != nil {
				for k, v := range j.BranchContext.VisitGroups {
					groups[k] = v
				}
			}
			groups[prefix+"/"+f.Stage] = f.Visit
			siblings := []any{}
			for otherIndex, other := range f.Branches {
				if otherIndex != i {
					siblings = append(siblings, other.Item)
				}
			}
			view.BranchContext = &BranchContext{Siblings: siblings, VisitGroups: groups, IndexPath: indices, Path: path, Fan: f.Stage, Item: b.Item, WriteScope: b.WriteScope}
			prior := j.BranchVisits[branchCounterKey(view.BranchContext)]
			view.NewAttempt = j.NewAttempt
			view.BranchAttemptPaid = false
			view.Failures = map[string]int{}
			view.InterruptedSessionID = ""
			view.CutOffByQuota = false
			view.PriorRejections = map[string][]string{}
			view.TurnCheckpoints = map[string]TurnCheckpoint{}
			for stage := range workflow.BranchOf(j.Document, f.Stage) {
				delete(view.HandlerAliases, stage)
			}
			for stage := range workflow.BranchOf(j.Document, f.Stage) {
				view.Counts[stage] = prior[stage]
			}
			var entered []Event
			event(&view, Event{Kind: "transition", Stage: f.Stage, From: domain.State(f.Stage), To: domain.State(b.Start), Cause: "fan-branch " + b.ID, BranchID: path}, &entered)
			mergeBranchView(&j, view, entered)
			b.Frame = &view
			b.Status = BranchRunning
			b.Stage = b.Start
		}
		view := *b.Frame
		if view.InFlight != nil && view.Fan == nil {
			continue
		}
		for step := 0; step < 1024; step++ {
			view.Sequence = j.Sequence
			view.Attempts = j.Attempts
			view.TestRuns = j.TestRuns
			view.TreeDescendants = j.TreeDescendants
			view.RootMaxDescendants = j.RootMaxDescendants
			view.RunStats = j.RunStats
			view.IncomingRequests = j.IncomingRequests
			view.OutputDir, view.StagingDir = j.OutputDir, j.StagingDir
			if string(view.State) == f.Join {
				finishNestedBranch(&j, i, &view, &events)
				b.Frame = &view
				break
			}
			if view.State == domain.Escalated || view.State.Terminal() {
				if view.State == domain.Escalated {
					b.Frame = &view
					b.Status = BranchFailed
					b.Error = view.Reason
					b.Stage = f.Join
					escalateBranch(&j, view, &events)
					return j, nil, events, nil
				}
				b.Status = BranchFailed
				b.Error = view.Reason
				b.Stage = f.Join
				b.Frame = &view
				break
			}
			if view.Child != nil || view.Parked && view.Reason != ParkRoutesUnavailable && view.Reason != "infrastructure-deferred" {
				b.Frame = &view
				break
			}
			previous := clone(view)
			var next Job
			var ds []*Dispatch
			var ev []Event
			var err error
			if view.Fan != nil && view.Fan.Baseline != nil {
				next, ds, ev, err = prepareNestedFan(view, o, limit-len(dispatches), "")
			} else {
				var d *Dispatch
				next, d, ev, err = PrepareWith(view, o)
				if d != nil {
					index := i
					d.BranchIndex = &index
					d.BranchID = path
					d.FanStage = f.Stage
					d.BranchItem = b.Item
					d.WriteScope = append([]string(nil), b.WriteScope...)
					next.InFlight = d
					ds = []*Dispatch{d}
				}
			}
			if err != nil {
				return input, nil, nil, err
			}
			mergeBranchView(&j, next, ev, previous)
			events = append(events, ev...)
			view = next
			b = &j.Fan.Branches[i]
			b.Frame = &view
			b.Stage = string(view.State)
			for _, d := range ds {
				dispatches = append(dispatches, d)
				if b.InFlight == nil {
					b.InFlight = d
				}
			}
			if len(ds) > 0 || len(ev) == 0 {
				break
			}
		}
		if len(dispatches) >= limit || !f.Parallel && b.Status == BranchRunning {
			break
		}
	}
	refreshNestedDispatch(&j)
	if err := settleNestedFan(&j, &events); err != nil {
		return input, nil, nil, err
	}
	// A failure discovered while preparing can settle an enclosing join and
	// cancel work selected earlier in this call. Never return cancelled turns.
	live := map[string]bool{}
	for _, dispatch := range BranchDispatches(j) {
		live[dispatch.ID] = true
	}
	surviving := dispatches[:0]
	for _, dispatch := range dispatches {
		if live[dispatch.ID] {
			surviving = append(surviving, dispatch)
		}
	}
	return j, surviving, events, nil
}

func refreshNestedDispatch(j *Job) {
	j.InFlight = nil
	for _, d := range BranchDispatches(*j) {
		j.InFlight = d
		enterFanCursor(j, j.Fan.Stage)
		return
	}
	if j.Fan != nil {
		enterFanCursor(j, j.Fan.Stage)
	}
}

func BranchDispatches(j Job) []*Dispatch {
	out := []*Dispatch{}
	if j.Fan == nil {
		if j.InFlight != nil {
			out = append(out, j.InFlight)
		}
		return out
	}
	for _, b := range j.Fan.Branches {
		if b.Status == BranchCancelled || b.Status == BranchSkipped || b.Status == BranchFailed || b.Status == BranchPassed {
			continue
		}
		if b.Frame != nil {
			out = append(out, BranchDispatches(*b.Frame)...)
		} else if b.InFlight != nil {
			out = append(out, b.InFlight)
		}
	}
	return out
}

func completeNestedFan(input Job, branch, id string, r Result) (Job, []Event, error) {
	j := clone(input)
	var events []Event
	found := false
	for i := range j.Fan.Branches {
		b := &j.Fan.Branches[i]
		if b.Frame == nil {
			continue
		}
		view := *b.Frame
		if view.Fan != nil {
			for _, d := range BranchDispatches(view) {
				if d.ID == id {
					next, ev, err := completeNestedFan(view, branch, id, r)
					if err != nil {
						return input, nil, err
					}
					mergeBranchView(&j, next, ev, view)
					events = append(events, ev...)
					b = &j.Fan.Branches[i]
					b.Frame = &next
					b.Stage = string(next.State)
					found = true
					break
				}
			}
		} else if view.InFlight != nil && view.InFlight.ID == id {
			before := len(view.History)
			next, _, err := complete(view, id, r)
			if err != nil {
				return input, nil, err
			}
			ev := append([]Event(nil), next.History[before:]...)
			mergeBranchView(&j, next, ev, view)
			events = append(events, ev...)
			b = &j.Fan.Branches[i]
			b.Frame = &next
			b.Stage = string(next.State)
			b.InFlight = nil
			if r.NewTree != "" {
				b.Tree = r.NewTree
			}
			if r.BranchBaseTree != "" {
				b.BaseTree = r.BranchBaseTree
			}
			if r.BranchCommit != "" {
				b.CommitSHA = r.BranchCommit
			}
			if r.BranchRef != "" {
				b.BranchRef = r.BranchRef
			}
			found = true
		}
		if found {
			b = &j.Fan.Branches[i]
			view = *b.Frame
			if string(view.State) == j.Fan.Join {
				finishNestedBranch(&j, i, &view, &events)
				b.Frame = &view
			} else if view.State == domain.Escalated || view.State.Terminal() {
				if view.State == domain.Escalated {
					b.Status = BranchFailed
					b.Error = view.Reason
					b.Stage = j.Fan.Join
					escalateBranch(&j, view, &events)
					return j, events, nil
				}
				b.Status = BranchFailed
				b.Error = view.Reason
				b.Stage = j.Fan.Join
			}
			break
		}
	}
	if !found {
		return input, nil, fmt.Errorf("stale nested branch completion %s", id)
	}
	refreshNestedDispatch(&j)
	if err := settleNestedFan(&j, &events); err != nil {
		return input, nil, err
	}
	return j, events, nil
}

func finishNestedBranch(j *Job, i int, view *Job, events *[]Event) {
	b := &j.Fan.Branches[i]
	if b.Status == BranchPassed || b.Status == BranchFailed {
		return
	}
	b.Status, b.Stage, b.InFlight = BranchPassed, j.Fan.Join, nil
	arrivedFrom := ""
	for n := len(view.History) - 1; n >= 0; n-- {
		ev := view.History[n]
		if ev.Kind != "transition" || string(ev.To) != j.Fan.Join {
			continue
		}
		arrivedFrom = string(ev.From)
		if strings.HasPrefix(ev.Cause, "failure ") {
			view.EscalatedStage = string(ev.From)
			b.Status = BranchFailed
			b.Error = "branch took failure route from " + string(ev.From)
			for x := len(view.Evidence) - 1; x >= 0; x-- {
				a := view.Evidence[x]
				if a.Stage == "failure:"+string(ev.From) {
					if detail, ok := a.Fields["error"].(string); ok {
						b.Error = detail
						if reason, ok := a.Fields["reason"].(string); ok {
							b.FailureReason = reason
						}
					}
					break
				}
			}
		}
		break
	}
	if b.Pass != nil {
		g := record(j, workflow.RecordBranch, j.Fan.Join, b.ID, 0, *b.Pass, current(*view), "branch-verdict", events)
		if !g.Passed {
			b.Status = BranchFailed
			b.Error = "branch pass expression was not true"
			b.FailureReason = "gate"
			view.EscalatedStage = arrivedFrom
		}
	}
	event(j, Event{Kind: "branch-joined", Stage: j.Fan.Join, Cause: b.ID + " " + b.Status}, events)
}

func settleNestedFan(j *Job, events *[]Event) error {
	f := j.Fan
	if f == nil {
		return nil
	}
	passed, failed := 0, 0
	for _, b := range f.Branches {
		if b.Status == BranchPassed {
			passed++
		} else if b.Status == BranchFailed {
			failed++
		}
	}
	join := j.Document.Stages[f.Join]
	_, decided := workflow.JoinVerdict(join.Rule, join.Quorum, passed, failed, len(f.Branches))
	if !decided {
		return nil
	}
	// fanStep records the ordinary typed join and cancellations. Branch-local
	// views have already contributed their evidence in deterministic slots.
	from := j.State
	enterFanCursor(j, f.Join)
	event(j, Event{Kind: "fan-cursor", Stage: f.Stage, From: from, To: j.State, Attempt: j.stagedAttempt(), Cause: "settled fan checkpoint"}, events)
	j.InFlight = nil
	return fanStep(j, join, events)
}

// A fan checkpoint has several simultaneously live cursors. Projecting the
// enclosing job onto one of them is not a workflow route, and cannot select a
// stage that is absent from this checkpoint. Durable assignments still pass
// through enter, which also clears turn-local retry state.
func enterFanCursor(j *Job, stage string) {
	if j.Fan == nil {
		return
	}
	allowed := stage == j.Fan.Stage || stage == j.Fan.Join
	for _, d := range BranchDispatches(*j) {
		allowed = allowed || d.Stage == stage
	}
	if !allowed {
		return
	}
	j.fanCursorAuthorization = stage
	_ = enter(j, domain.State(stage))
}

type ChildVisit struct {
	BranchID string
	Ref      ChildRef
}

func BranchJob(j Job, path string) (Job, bool) {
	if j.BranchContext != nil && j.BranchContext.Path == path {
		return j, true
	}
	if j.Fan != nil {
		for _, b := range j.Fan.Branches {
			if b.Frame != nil {
				if view, ok := BranchJob(*b.Frame, path); ok {
					return view, true
				}
			}
		}
	}
	return Job{}, false
}

func PendingSubWorkflowVisits(j Job) []ChildVisit {
	out := []ChildVisit{}
	if ref, ok := PendingChild(j); ok {
		path := ""
		if j.BranchContext != nil {
			path = j.BranchContext.Path
		}
		out = append(out, ChildVisit{path, ref})
	}
	if j.Fan != nil {
		for _, b := range j.Fan.Branches {
			if b.Frame != nil {
				out = append(out, PendingSubWorkflowVisits(*b.Frame)...)
			}
		}
	}
	return out
}

func updateBranchChild(input Job, path string, child *Job, children []*Job, reason string) (Job, []Event, error) {
	j := clone(input)
	if j.Fan == nil {
		return input, nil, errors.New("no nested fan child")
	}
	var events []Event
	for i := range j.Fan.Branches {
		b := &j.Fan.Branches[i]
		if b.Frame == nil {
			continue
		}
		view := *b.Frame
		if view.BranchContext != nil && view.BranchContext.Path == path && view.Child != nil {
			var next Job
			var ev []Event
			var err error
			if children != nil {
				previous := len(view.History)
				err = view.BindChildren(children)
				next, ev = view, view.History[previous:]
			} else if child != nil {
				next, ev, err = BindChild(view, *child)
			} else {
				next, ev, err = ChildFailed(view, reason)
			}
			if err != nil {
				return input, nil, err
			}
			mergeBranchView(&j, next, ev, view)
			events = append(events, ev...)
			b = &j.Fan.Branches[i]
			b.Frame = &next
			b.Stage = string(next.State)
			if next.State == domain.Escalated {
				b.Status = BranchFailed
				b.Error = next.Reason
			} else if string(next.State) == j.Fan.Join {
				finishNestedBranch(&j, i, &next, &events)
			}
			refreshNestedDispatch(&j)
			if err = settleNestedFan(&j, &events); err != nil {
				return input, nil, err
			}
			return j, events, nil
		}
		if view.Fan != nil {
			next, ev, err := updateBranchChild(view, path, child, children, reason)
			if err == nil {
				mergeBranchView(&j, next, ev, view)
				b = &j.Fan.Branches[i]
				b.Frame = &next
				events = append(events, ev...)
				refreshNestedDispatch(&j)
				return j, events, nil
			}
		}
	}
	return input, nil, errors.New("unknown branch child")
}
func BindBranchChild(input Job, path string, child Job) (Job, []Event, error) {
	return updateBranchChild(input, path, &child, nil, "")
}
func FailBranchChild(input Job, path, reason string) (Job, []Event, error) {
	return updateBranchChild(input, path, nil, nil, reason)
}

func BindBranchChildren(input Job, path string, children []*Job) (Job, []Event, error) {
	return updateBranchChild(input, path, nil, children, "")
}

func branchCounterKey(context *BranchContext) string { return branchInstanceKey(context) }
func branchInstanceKey(context *BranchContext) string {
	if context == nil {
		return ""
	}
	raw, _ := json.Marshal(context.VisitGroups)
	sum := sha256.Sum256(raw)
	return context.Fan + "/" + context.Path + "/" + hex.EncodeToString(sum[:12])
}

func escalateBranch(j *Job, failed Job, events *[]Event) {
	for _, d := range BranchDispatches(*j) {
		event(j, Event{Kind: "branch-cancelled", Stage: d.Stage, DispatchID: d.ID, BranchID: d.BranchID, Cause: "branch escalation"}, events)
	}
	var cancel func(*Job)
	cancel = func(view *Job) {
		view.InFlight = nil
		if view.Fan == nil {
			return
		}
		for i := range view.Fan.Branches {
			b := &view.Fan.Branches[i]
			if b.Status == BranchRunning || b.Status == BranchPending {
				b.Status = BranchCancelled
			}
			b.InFlight = nil
			if b.Frame != nil {
				cancel(b.Frame)
			}
		}
	}
	cancel(j)
	kind := failed.EscalationKind
	if kind == "" {
		kind = domain.HumanDecisionRequired
	}
	detail := failed.EscalationDetail
	if detail == "" {
		detail = failed.Reason
	}
	stop(j, kind, detail, events)
	j.EscalatedStage = failed.EscalatedStage
}
