package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/worktree"
)

type parallelBranchTracker struct {
	mu       sync.Mutex
	inFlight map[string]map[string]string             // taskID -> branchID -> dispatchID
	cancels  map[string]map[string]context.CancelFunc // taskID -> dispatchID -> cancel
}

var daemonParallelBranches sync.Map

func (d *Daemon) branchTracker() *parallelBranchTracker {
	v, _ := daemonParallelBranches.LoadOrStore(d, &parallelBranchTracker{
		inFlight: map[string]map[string]string{},
		cancels:  map[string]map[string]context.CancelFunc{},
	})
	return v.(*parallelBranchTracker)
}

func (d *Daemon) registerDispatchCancel(taskID, dispatchID string, cancel context.CancelFunc) {
	if d.inFlightCancel == nil {
		d.inFlightCancel = map[string]context.CancelFunc{}
	}
	t := d.branchTracker()
	t.mu.Lock()
	if t.cancels[taskID] == nil {
		t.cancels[taskID] = map[string]context.CancelFunc{}
	}
	t.cancels[taskID][dispatchID] = cancel
	t.mu.Unlock()
	d.inFlightCancel[dispatchID] = cancel
	d.inFlightCancel[taskID] = func() {
		d.cancelTaskInFlight(taskID)
	}
}

func (d *Daemon) cancelTaskInFlight(taskID string) {
	t := d.branchTracker()
	t.mu.Lock()
	var fns []context.CancelFunc
	for _, fn := range t.cancels[taskID] {
		if fn != nil {
			fns = append(fns, fn)
		}
	}
	t.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func (d *Daemon) clearDispatchCancel(taskID, dispatchID string) {
	if d.inFlightCancel != nil {
		delete(d.inFlightCancel, dispatchID)
	}
	t := d.branchTracker()
	t.mu.Lock()
	remaining := 0
	if m := t.cancels[taskID]; m != nil {
		delete(m, dispatchID)
		remaining = len(m)
		if remaining == 0 {
			delete(t.cancels, taskID)
		}
	}
	t.mu.Unlock()
	if remaining == 0 && d.inFlightCancel != nil {
		delete(d.inFlightCancel, taskID)
	}
}

func (d *Daemon) isDispatchActive(taskID, dispatchID string) bool {
	if dispatchID == "" {
		return false
	}
	t := d.branchTracker()
	t.mu.Lock()
	defer t.mu.Unlock()
	if m := t.cancels[taskID]; m != nil {
		if _, ok := m[dispatchID]; ok {
			return true
		}
	}
	for _, did := range t.inFlight[taskID] {
		if did == dispatchID {
			return true
		}
	}
	return false
}

func hasReadyParallelBranch(j engine.Job) bool {
	if j.State.Kernel() || j.OperatorStage != "" || j.Parked || j.Fan == nil || !j.Fan.Parallel {
		return false
	}
	if j.Fan.Baseline != nil {
		for _, b := range j.Fan.Branches {
			if b.Frame != nil && hasReadyParallelBranch(*b.Frame) {
				return true
			}
			if b.Status == engine.BranchPending || b.Status == engine.BranchRunning && b.Frame != nil && b.Frame.InFlight == nil && b.Frame.Child == nil {
				return true
			}
		}
		return false
	}
	for _, br := range j.Fan.Branches {
		if br.InFlight == nil && (br.Status == engine.BranchPending || br.Status == engine.BranchRunning) && br.Stage != j.Fan.Join {
			return true
		}
	}
	return false
}

func (d *Daemon) markBranchActive(taskID, branchID, dispatchID string) {
	if branchID == "" {
		return
	}
	t := d.branchTracker()
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inFlight[taskID] == nil {
		t.inFlight[taskID] = map[string]string{}
	}
	t.inFlight[taskID][branchID] = dispatchID
}

func (d *Daemon) clearBranchActive(taskID, branchID string) bool {
	t := d.branchTracker()
	t.mu.Lock()
	defer t.mu.Unlock()
	if m := t.inFlight[taskID]; m != nil {
		delete(m, branchID)
		if len(m) == 0 {
			delete(t.inFlight, taskID)
			return false
		}
		return true
	}
	return false
}

func (d *Daemon) hasActiveBranches(taskID string) bool {
	t := d.branchTracker()
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.inFlight[taskID]) > 0
}

// dispatchParallelBranches prepares and launches any ready sibling branches
// when a task is inside a Parallel: true FanOut (FAE-WF-096). Caller holds d.mu.
func (d *Daemon) dispatchParallelBranches(id string, snapshot controlplane.JobSnapshot, e envelope, firstDispatch *engine.Dispatch, options engine.Options) (controlplane.JobSnapshot, envelope, []*engine.Dispatch, error) {
	options.ValidateChangeRequest = d.validateChangeRequest
	incoming, requestErr := d.changeRequestsForTask(d.ctx, e.Job.TaskID)
	if requestErr != nil {
		return snapshot, e, nil, requestErr
	}
	e.Job.IncomingRequests = incoming
	var extraDispatches []*engine.Dispatch
	if firstDispatch != nil && firstDispatch.BranchID != "" {
		d.markBranchActive(id, firstDispatch.BranchID, firstDispatch.ID)
	}
	if e.Job.Fan == nil || !e.Job.Fan.Parallel {
		return snapshot, e, nil, nil
	}
	if e.Job.Fan.Baseline != nil {
		if err := d.prepareRunStats(d.ctx, &e.Job); err != nil {
			return snapshot, e, nil, err
		}
		next, dispatches, events, err := engine.PrepareParallelFanOut(e.Job, options)
		if err != nil {
			return snapshot, e, nil, err
		}
		e.Job = next
		var ready []*engine.Dispatch
		for _, disp := range dispatches {
			if (firstDispatch == nil || disp.ID != firstDispatch.ID) && !d.isDispatchActive(id, disp.ID) {
				ready = append(ready, disp)
				d.markBranchActive(id, disp.BranchID, disp.ID)
			}
		}
		if len(events) > 0 || len(ready) > 0 {
			saved, err := d.openCheckpoint(func() (controlplane.JobSnapshot, error) { return d.persist(d.ctx, snapshot, e, events) })
			if err != nil {
				return snapshot, e, nil, err
			}
			snapshot = saved
			d.jobs[id] = saved
		}
		return snapshot, e, ready, nil
	}
	var allEvents []engine.Event
	curJob := e.Job
	for i := range curJob.Fan.Branches {
		br := curJob.Fan.Branches[i]
		if firstDispatch != nil && br.ID == firstDispatch.BranchID {
			continue
		}
		if br.InFlight != nil || (br.Status != engine.BranchPending && br.Status != engine.BranchRunning) {
			continue
		}
		if err := d.prepareRunStats(d.ctx, &curJob); err != nil {
			return snapshot, e, nil, err
		}
		nextJ, disp, evs, err := engine.PrepareBranchWith(curJob, br.ID, options)
		if err != nil {
			break
		}
		curJob = nextJ
		allEvents = append(allEvents, evs...)
		if disp != nil && !d.isDispatchActive(id, disp.ID) {
			dCopy := *disp
			extraDispatches = append(extraDispatches, &dCopy)
			d.markBranchActive(id, br.ID, dCopy.ID)
		}
		if curJob.Fan == nil {
			break
		}
	}
	if len(allEvents) > 0 || len(extraDispatches) > 0 {
		e.Job = curJob
		saved, err := d.openCheckpoint(func() (controlplane.JobSnapshot, error) {
			return d.persist(d.ctx, snapshot, e, allEvents)
		})
		if err != nil {
			return snapshot, e, nil, err
		}
		snapshot = saved
		d.jobs[id] = snapshot
	}
	return snapshot, e, extraDispatches, nil
}

// matchParallelBranchDispatch finds the branch ID for a completion dispatchID
// when a task is in a Parallel: true FanOut.
func matchParallelBranchDispatch(j engine.Job, dispatchID string) (string, *engine.Dispatch, bool) {
	if j.Fan != nil && j.Fan.Baseline != nil {
		for _, disp := range engine.BranchDispatches(j) {
			if disp.ID == dispatchID {
				return disp.BranchID, disp, true
			}
		}
		return "", nil, false
	}
	if j.Fan == nil {
		return "", nil, false
	}
	for i := range j.Fan.Branches {
		b := &j.Fan.Branches[i]
		if b.InFlight != nil && b.InFlight.ID == dispatchID {
			return b.ID, b.InFlight, true
		}
	}
	if j.InFlight != nil && j.InFlight.ID == dispatchID && j.InFlight.BranchID != "" {
		return j.InFlight.BranchID, j.InFlight, true
	}
	return "", nil, false
}

// integrateFanBranchesAtJoin merges surviving change-producing branch commits in
// deterministic branch order (0..N-1) into the parent task worktree when a
// FanOut reaches FanIn (FAE-WF-096).
func (d *Daemon) integrateFanBranchesAtJoin(ctx context.Context, taskID, runID string, beforeFan *engine.FanState, afterJob *engine.Job) error {
	if d == nil || d.live == nil || d.live.broker == nil || afterJob == nil {
		return nil
	}
	parent, err := d.live.manager.Ensure(ctx, runID, taskID, afterJob.Base)
	if err != nil {
		return err
	}
	return d.integrateFanFrame(ctx, taskID, parent, afterJob)
}

// Each completed inner fan is integrated into its enclosing branch before the
// next turn can resolve that branch's workspace. The marker is checkpointed.
func (d *Daemon) integrateFanFrame(ctx context.Context, taskID string, parent worktree.Binding, job *engine.Job) error {
	for _, f := range []*engine.FanState{job.Fan, job.LastFan} {
		if f == nil {
			continue
		}
		for i := range f.Branches {
			branch := &f.Branches[i]
			if branch.Frame == nil || len(branch.WriteScope) == 0 || !pendingFanIntegration(*branch.Frame) {
				continue
			}
			path := branch.ID
			if branch.Frame.BranchContext != nil {
				path = branch.Frame.BranchContext.Path
			}
			bw, err := d.live.broker.ForkBranchWorktree(ctx, parent, fanWorktreeIdentity(f), path, branch.WriteScope)
			if err != nil {
				return err
			}
			if err = d.integrateFanFrame(ctx, taskID, bw.Binding, branch.Frame); err != nil {
				return err
			}
			committed, err := d.live.broker.CommitBranchWorktree(ctx, bw, nil, "fae nested branch checkpoint")
			if err != nil {
				return err
			}
			branch.Tree, branch.CommitSHA, branch.BranchRef = committed.Tree, committed.CommitSHA, committed.Ref
			branch.BaseTree = bw.BaseTree
			branch.Frame.Tree = committed.Tree
		}
	}
	f := job.LastFan
	if f == nil || f.IntegratedTree != "" {
		return nil
	}
	var branches []effects.BranchWorktree
	for _, branch := range f.Branches {
		if branch.Status != engine.BranchPassed || branch.CommitSHA == "" {
			continue
		}
		branches = append(branches, effects.BranchWorktree{BranchID: branch.ID, BaseTree: branch.BaseTree, CommitSHA: branch.CommitSHA, Tree: branch.Tree, WriteScope: branch.WriteScope})
	}
	if len(branches) == 0 {
		f.IntegratedTree = job.Tree
		return nil
	}
	tree, err := d.live.broker.IntegrateBranchWorktrees(ctx, parent, branches)
	if err != nil {
		return err
	}
	job.Tree, f.IntegratedTree = tree, tree
	return nil
}

func pendingFanIntegration(job engine.Job) bool {
	if job.LastFan != nil && job.LastFan.IntegratedTree == "" {
		return true
	}
	if job.Fan != nil {
		for _, b := range job.Fan.Branches {
			if b.Frame != nil && pendingFanIntegration(*b.Frame) {
				return true
			}
		}
	}
	return false
}

func fanWorktreeIdentity(f *engine.FanState) string {
	return fmt.Sprintf("%s/visit-%d", f.Stage, f.Visit)
}

// branchWorktree follows the checkpointed lexical fan frames, so nested leaves
// inherit earlier enclosing-branch writes without receiving sibling writes.
func branchWorktree(ctx context.Context, broker *effects.Broker, parent worktree.Binding, job engine.Job, path string) (effects.BranchWorktree, error) {
	if job.Fan == nil {
		return effects.BranchWorktree{}, fmt.Errorf("missing fan for branch %s", path)
	}
	for _, branch := range job.Fan.Branches {
		candidate := branch.ID
		if branch.Frame != nil && branch.Frame.BranchContext != nil {
			candidate = branch.Frame.BranchContext.Path
		}
		if path != candidate && !strings.HasPrefix(path, candidate+"/") {
			continue
		}
		bw, err := broker.ForkBranchWorktree(ctx, parent, fanWorktreeIdentity(job.Fan), candidate, branch.WriteScope)
		if err != nil {
			return bw, err
		}
		if path == candidate {
			return bw, nil
		}
		if branch.Frame == nil {
			return bw, fmt.Errorf("missing enclosing branch frame %s", candidate)
		}
		return branchWorktree(ctx, broker, bw.Binding, *branch.Frame, path)
	}
	return effects.BranchWorktree{}, fmt.Errorf("unknown branch %s", path)
}

// ExecuteBranchWorktree runs a change-producing FanOut branch inside its own
// forked git worktree, enforcing its branch WriteScope and committing its
// changes onto its branch-local ref before FanIn integration (FAE-WF-096).
func ExecuteBranchWorktree(ctx context.Context, broker *effects.Broker, parent worktree.Binding, d engine.Dispatch, fn func(branchBinding worktree.Binding) error) (effects.BranchWorktree, error) {
	bw, err := broker.ForkBranchWorktree(ctx, parent, d.FanStage, d.BranchID, d.WriteScope)
	if err != nil {
		return effects.BranchWorktree{}, err
	}
	defer func() {
		_ = broker.CleanupBranchWorktree(context.Background(), bw)
	}()
	if err := fn(bw.Binding); err != nil {
		return bw, err
	}
	msg := fmt.Sprintf("fae fan-branch %s/%s (%s)", d.FanStage, d.BranchID, d.TaskID)
	return broker.CommitBranchWorktree(ctx, bw, d.Scope, msg)
}

// DriveParallelFanOut drives an active (or starting) Parallel: true FanOut
// stage by preparing all ready branches and executing their dispatches
// concurrently in goroutines until FanIn settles (FAE-WF-096).
func DriveParallelFanOut(ctx context.Context, input engine.Job, exec engine.Executor, options engine.Options) (engine.Job, []engine.Event, error) {
	j := input
	var allEvents []engine.Event
	for step := 0; step < 512; step++ {
		if j.State.Terminal() || j.State == "Escalated" || j.Parked {
			return j, allEvents, nil
		}
		s, ok := j.Document.Stages[string(j.State)]
		if j.Fan == nil && (!ok || s.Primitive != "FanOut") {
			nextJ, d, evs, err := engine.PrepareWith(j, options)
			if err != nil {
				return j, allEvents, err
			}
			allEvents = append(allEvents, evs...)
			j = nextJ
			if d != nil {
				res, err := exec.Execute(ctx, *d)
				if err != nil && res.Detail == "" {
					res = engine.Result{DispatchID: d.ID, TaskID: j.TaskID, Tree: d.Tree, Class: "execution-error", Session: d.Sessions[0].ID, ProducedAt: time.Now().UTC(), Detail: err.Error()}
				}
				j, evs, err = engine.Complete(j, d.ID, res)
				if err != nil {
					return j, allEvents, err
				}
				allEvents = append(allEvents, evs...)
			}
			continue
		}
		if j.Fan == nil && ok && s.Primitive == "FanOut" {
			nextJ, d, evs, err := engine.PrepareWith(j, options)
			if err != nil {
				return j, allEvents, err
			}
			allEvents = append(allEvents, evs...)
			j = nextJ
			if j.Fan == nil {
				// Empty route taken.
				continue
			}
			if d != nil && j.Fan != nil && len(j.Fan.Branches) > 0 {
				b := &j.Fan.Branches[0]
				d.BranchID = b.ID
				d.FanStage = j.Fan.Stage
				d.BranchItem = b.Item
				d.WriteScope = append([]string(nil), b.WriteScope...)
				b.InFlight = d
				b.Stage = d.Stage
			}
		}
		if j.Fan == nil {
			continue
		}
		// Prepare all branches that can run concurrently.
		var dispatches []engine.Dispatch
		if j.Fan.Baseline != nil {
			next, ready, evs, err := engine.PrepareParallelFanOut(j, options)
			if err != nil {
				return j, allEvents, err
			}
			j = next
			allEvents = append(allEvents, evs...)
			for _, d := range ready {
				dispatches = append(dispatches, *d)
			}
		} else {
			for i := range j.Fan.Branches {
				br := &j.Fan.Branches[i]
				if br.InFlight != nil {
					dispatches = append(dispatches, *br.InFlight)
					continue
				}
				if br.Status != engine.BranchPending && br.Status != engine.BranchRunning {
					continue
				}
				if !j.Fan.Parallel && len(dispatches) > 0 {
					break
				}
				nextJ, d, evs, err := engine.PrepareBranchWith(j, br.ID, options)
				if err != nil {
					return j, allEvents, err
				}
				allEvents = append(allEvents, evs...)
				j = nextJ
				if j.Fan == nil {
					break
				}
				if d != nil {
					dispatches = append(dispatches, *d)
				}
			}
		}
		if j.Fan == nil || len(dispatches) == 0 {
			continue
		}
		type branchOut struct {
			branchID string
			dispatch engine.Dispatch
			result   engine.Result
			err      error
		}
		outs := make(chan branchOut, len(dispatches))
		turnContext, cancelTurns := context.WithCancel(ctx)
		for _, d := range dispatches {
			go func(disp engine.Dispatch) {
				res, err := exec.Execute(turnContext, disp)
				if err != nil && res.Detail == "" {
					res = engine.Result{DispatchID: disp.ID, TaskID: disp.TaskID, Tree: disp.Tree, Class: "execution-error", Session: disp.Sessions[0].ID, ProducedAt: time.Now().UTC(), Detail: err.Error()}
				}
				outs <- branchOut{branchID: disp.BranchID, dispatch: disp, result: res, err: err}
			}(d)
		}
		for remaining := len(dispatches); remaining > 0; remaining-- {
			out := <-outs
			if j.Fan == nil {
				continue
			}
			branchID := out.branchID
			if branchID == "" && len(j.Fan.Branches) > 0 {
				branchID = j.Fan.Branches[0].ID
			}
			nextJ, evs, err := engine.CompleteBranch(j, branchID, out.dispatch.ID, out.result)
			if err != nil {
				cancelTurns()
				return j, allEvents, err
			}
			allEvents = append(allEvents, evs...)
			j = nextJ
			if j.Fan == nil {
				cancelTurns()
			}
		}
		cancelTurns()
	}
	return j, allEvents, nil
}
