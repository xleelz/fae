package daemon

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/worktree"
)

func staleResidue(detail string) error {
	return errors.New(string(domain.StaleBranchResidue) + ": stale task branch residue: " + detail)
}

// integrationTip reads the integration target's current tip in the bound
// repository; an instance whose target does not exist yet forks from its
// pinned base.
func (x *liveExecutor) integrationTip(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--verify", "--quiet", x.config.IntegrationRef+"^{commit}")
	cmd.Dir = x.manager.Repository
	cmd.Env = cleanGitEnv()
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && len(out) == 0 {
		return x.config.Base, nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// forkTask gives a task its worktree at base. An existing pristine worktree
// whose HEAD is the task's recorded base, or an ancestor of base on the
// integration line, moves to base; any other content is StaleBranchResidue
// (FAE-VCS-003). It returns the measured tree.
func (x *liveExecutor) forkTask(ctx context.Context, run, task, base, recorded string) (worktree.Binding, string, error) {
	w, err := x.manager.Ensure(ctx, run, task, base)
	if err != nil {
		return w, "", err
	}
	head, err := effectGit(ctx, w, "rev-parse", "HEAD")
	if err != nil {
		return w, "", err
	}
	if head != base {
		if _, notAncestor := effectGit(ctx, w, "merge-base", "--is-ancestor", head, base); head != recorded && notAncestor != nil {
			return w, "", staleResidue("HEAD differs from the task base")
		}
		if _, err = x.manager.Refork(ctx, run, task, head, base); err != nil {
			return w, "", staleResidue(err.Error())
		}
	}
	tree, err := x.broker.Tree(ctx, w)
	if err != nil {
		return w, "", err
	}
	want, err := effectGit(ctx, w, "rev-parse", base+"^{tree}")
	if err != nil {
		return w, "", err
	}
	if tree != want {
		return w, "", staleResidue("task worktree is not pristine")
	}
	return w, tree, nil
}

// releaseFork moves a released task's pristine worktree from base to the
// current integration tip and returns the tip and the measured tree; a tip
// equal to base leaves it untouched. It holds no daemon lock.
func (x *liveExecutor) releaseFork(ctx context.Context, run, task, base string) (string, string, error) {
	tip, err := x.integrationTip(ctx)
	if err != nil || tip == base {
		return base, "", err
	}
	_, tree, err := x.forkTask(ctx, run, task, tip, base)
	return tip, tree, err
}

// forkAtRelease runs under d.mu when a Pending task is released: its pristine
// worktree moves to the current integration tip, so a dependent starts from
// its satisfied prerequisites' output (FAE-VCS-008). A carry-forward
// re-admission re-forks too: its carried pre-code evidence is informational
// on any tree (engine.CarryForward). The exact base is durable in the
// checkpoint before any dispatch. A worktree that is not pristine keeps its
// base; the dispatch-time residue check refuses it.
//
// Like a drive checkpoint, its git commands and plane write run without the
// writer lock (FAE-ROB-021): admissions proceed, and every other writer waits
// for the open checkpoint. The task is read again under the lock before the
// fork is recorded; changed reports a task that changed meanwhile, which the
// next round decides afresh.
func (d *Daemon) forkAtRelease(s controlplane.JobSnapshot, e envelope) (_ controlplane.JobSnapshot, _ envelope, changed bool, _ error) {
	j, live := e.Job, d.live
	if j.Parent != nil || live == nil || j.ExecutionKind != "live" || j.State != domain.Pending || j.Sequence > 0 || j.InFlight != nil || j.Base == "" {
		return s, e, false, nil
	}
	d.checkpointing = true
	defer d.closeCheckpoint()
	var tip, tree string
	var err error
	func() {
		d.mu.Unlock()
		defer d.mu.Lock()
		ctx, cancel := context.WithTimeout(d.ctx, 30*time.Second)
		defer cancel()
		tip, tree, err = live.releaseFork(ctx, e.Admission.RunID, j.TaskID, j.Base)
	}()
	if now, ok := d.jobs[s.ID]; !ok || now.Version != s.Version || d.ctx.Err() != nil {
		return s, e, true, nil
	}
	if err != nil {
		d.taskLog("task %s keeps admission base %s: %v\n", j.TaskID, j.Base, err)
		return s, e, false, nil
	}
	if tip == j.Base {
		return s, e, false, nil
	}
	forked := e
	forked.Job = engine.Fork(j, tip, tree)
	saved, err := d.persist(d.ctx, s, forked, forked.Job.History[len(j.History):])
	if err != nil {
		return s, e, false, err
	}
	d.jobs[s.ID] = saved
	return saved, forked, false, nil
}

// childVisitBase creates an immutable commit object of the parent's recorded
// visit tree without changing its HEAD, refs, index or files. The child forks
// from that object, so uncommitted parent outputs are present too (WF-117).
func (x *liveExecutor) childVisitBase(ctx context.Context, parent engine.Job) (string, error) {
	args := []string{"-c", "core.hooksPath=/dev/null", "commit-tree", parent.Tree}
	if head := parent.ExpectedHead(); head != "" {
		args = append(args, "-p", head)
	}
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = x.manager.Repository
	cmd.Env = append(cleanGitEnv(), "GIT_AUTHOR_NAME=Fae", "GIT_AUTHOR_EMAIL=fae@localhost", "GIT_COMMITTER_NAME=Fae", "GIT_COMMITTER_EMAIL=fae@localhost")
	cmd.Stdin = strings.NewReader("FAE child visit snapshot for " + parent.TaskID + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
