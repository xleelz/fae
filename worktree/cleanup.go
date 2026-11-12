package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// CleanupPlan binds a removal to the observed branch tip. Archives are immutable
// per-tip refs, so subsequent cleanup attempts cannot overwrite an older backup.
type CleanupPlan struct {
	Run             string  `json:"run"`
	Task            string  `json:"task"`
	Binding         Binding `json:"binding"`
	Head            string  `json:"head"`
	Archive         string  `json:"archive"`
	WorktreePresent bool    `json:"worktree_present"`
	AlreadyRemoved  bool    `json:"already_removed"`
}

func (m *Manager) PlanCleanup(ctx context.Context, run, task string) (CleanupPlan, error) {
	p := CleanupPlan{Run: run, Task: task, Binding: m.Binding(run, task)}
	if run == "" || task == "" {
		return p, errors.New("run and task required")
	}
	if err := physicalAncestor(m.Root); err != nil {
		return p, err
	}
	if _, err := os.Stat(m.Root); os.IsNotExist(err) {
		return p, errors.New("owned worktree root missing")
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return p, err
	}
	defer unlock()
	return m.planCleanup(ctx, p)
}
func (m *Manager) planCleanup(ctx context.Context, p CleanupPlan) (CleanupPlan, error) {
	_, err := os.Lstat(p.Binding.Path)
	if err == nil {
		if err = m.verify(ctx, p.Binding); err != nil {
			return p, err
		}
		status, e := git(ctx, p.Binding.Path, "status", "--porcelain=v1", "--untracked-files=all", "--ignored")
		if e != nil {
			return p, e
		}
		if status != "" {
			return p, errors.New("dirty or ignored worktree files: cleanup refused")
		}
		p.WorktreePresent = true
	} else if !os.IsNotExist(err) {
		return p, err
	}
	if !p.WorktreePresent {
		registered, e := git(ctx, m.Repository, "worktree", "list", "--porcelain")
		if e != nil {
			return p, e
		}
		if strings.Contains("\n"+registered+"\n", "\nbranch refs/heads/"+p.Binding.Branch+"\n") {
			return p, errors.New("task branch registered at missing or foreign worktree")
		}
	}
	if _, e := git(ctx, m.Repository, "symbolic-ref", "-q", "refs/heads/"+p.Binding.Branch); e == nil {
		return p, errors.New("symbolic task branch refused")
	}
	head, err := git(ctx, m.Repository, "rev-parse", "--verify", "refs/heads/"+p.Binding.Branch+"^{commit}")
	if err != nil {
		// A missing branch is idempotent only with positive archive evidence.
		if p.WorktreePresent {
			return p, err
		}
		archives, e := git(ctx, m.Repository, "for-each-ref", "--format=%(refname)", "refs/fae/archive/"+Identity(p.Run, p.Task)+"/")
		if e != nil || archives == "" {
			return p, errors.New("missing task branch without archive evidence")
		}
		for _, ref := range strings.Split(archives, "\n") {
			if _, e := git(ctx, m.Repository, "symbolic-ref", "-q", ref); e == nil {
				return p, errors.New("symbolic archive evidence refused")
			}
			sha := ref[strings.LastIndex(ref, "/")+1:]
			actual, e := git(ctx, m.Repository, "rev-parse", "--verify", ref+"^{commit}")
			if e != nil || actual != sha {
				return p, errors.New("invalid archive evidence")
			}
		}
		p.AlreadyRemoved = true
		return p, nil
	}
	p.Head = head
	p.Archive = "refs/fae/archive/" + Identity(p.Run, p.Task) + "/" + head
	return p, nil
}

// ArchiveCleanup preflights and archives one plan without deleting anything.
// Batch callers archive all plans first, so an archive failure deletes nothing.
func (m *Manager) ArchiveCleanup(ctx context.Context, p CleanupPlan, verify func(context.Context) error) error {
	if verify == nil || p.Binding != m.Binding(p.Run, p.Task) {
		return errors.New("owned plan and verifier required")
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	now, err := m.planCleanup(ctx, CleanupPlan{Run: p.Run, Task: p.Task, Binding: p.Binding})
	if err != nil {
		return err
	}
	if now != p {
		return errors.New("cleanup plan changed")
	}
	if err = verify(ctx); err != nil {
		return err
	}
	if p.AlreadyRemoved {
		return nil
	}
	return m.archiveCleanup(ctx, p, verify)
}
func (m *Manager) archiveCleanup(ctx context.Context, p CleanupPlan, verify func(context.Context) error) error {
	var err error
	if _, e := git(ctx, m.Repository, "symbolic-ref", "-q", p.Archive); e == nil {
		return errors.New("symbolic archive refused")
	}
	existing, e := git(ctx, m.Repository, "rev-parse", "--verify", p.Archive)
	if e != nil {
		_, err = git(ctx, m.Repository, "update-ref", "--no-deref", p.Archive, p.Head, strings.Repeat("0", len(p.Head)))
		if err != nil {
			return fmt.Errorf("archive failed: %w", err)
		}
	} else if existing != p.Head {
		return errors.New("archive ref does not match branch")
	}
	archived, err := git(ctx, m.Repository, "rev-parse", "--verify", p.Archive+"^{commit}")
	backupErr := verify(ctx)
	if err != nil || !CanCleanupArchived(archived == p.Head, backupErr == nil) {
		return errors.New("archive verification failed")
	}
	if err = verify(ctx); err != nil {
		return err
	}
	return nil
}

// Cleanup archives before removing the clean owned worktree and CAS-deletes the
// branch in a transaction that also verifies the archive. Plane authority and
// backup verification are supplied by the caller and repeated under the lock.
func (m *Manager) Cleanup(ctx context.Context, p CleanupPlan, verify func(context.Context) error) error {
	if verify == nil {
		return errors.New("backup and retirement verifier required")
	}
	if p.Binding != m.Binding(p.Run, p.Task) {
		return errors.New("foreign cleanup binding")
	}
	unlock, err := m.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	now, err := m.planCleanup(ctx, CleanupPlan{Run: p.Run, Task: p.Task, Binding: p.Binding})
	if err != nil {
		return err
	}
	if now != p {
		return errors.New("cleanup plan changed; replan required")
	}
	if err = verify(ctx); err != nil {
		return err
	}
	if p.AlreadyRemoved {
		return nil
	}
	if err = m.archiveCleanup(ctx, p, verify); err != nil {
		return err
	}
	current, err := m.planCleanup(ctx, CleanupPlan{Run: p.Run, Task: p.Task, Binding: p.Binding})
	if err != nil || current != p {
		return errors.New("source changed after archive; nothing removed")
	}
	if p.WorktreePresent {
		if _, err = git(ctx, m.Repository, "worktree", "remove", p.Binding.Path); err != nil {
			return err
		}
	}
	// update-ref's transaction protects against a branch/archive change between
	// validation and deletion. No force branch deletion and no filesystem recursion.
	cmd := exec.CommandContext(ctx, "git", "-C", m.Repository, "update-ref", "--stdin")
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader("start\noption no-deref\nverify " + p.Archive + " " + p.Head + "\ndelete refs/heads/" + p.Binding.Branch + " " + p.Head + "\nprepare\ncommit\n")
	if out, e := cmd.CombinedOutput(); e != nil {
		return fmt.Errorf("archive/branch changed; branch retained: %w: %s", e, out)
	}
	return nil
}

// Integrated reports whether commit sha is reachable from ref in the managed
// repository (git merge-base --is-ancestor): whether a Done task's landing
// is in the integration ref, so its own worktree and branch hold nothing the
// ref lacks once an archive keeps the branch tip.
func (m *Manager) Integrated(ctx context.Context, sha, ref string) (bool, error) {
	if sha == "" || strings.HasPrefix(sha, "-") || !strings.HasPrefix(ref, "refs/") {
		return false, errors.New("commit and full ref required")
	}
	if _, err := git(ctx, m.Repository, "rev-parse", "--verify", "--quiet", ref+"^{commit}"); err != nil {
		return false, fmt.Errorf("integration ref %s: %w", ref, err)
	}
	cmd := exec.CommandContext(ctx, "git", "-C", m.Repository, "merge-base", "--is-ancestor", sha, ref)
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("git merge-base: %w: %s", err, out)
	}
	return true, nil
}
