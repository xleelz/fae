package daemon

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

// WF111 authorizes deletion only of declared scratch in this task's verified,
// daemon-owned worktrees. Files are removed individually; links are refused.
func (d *Daemon) cleanupTaskScratch(job engine.Job, admission ...AdmitParams) {
	if d.live == nil || d.live.manager == nil || !job.State.Terminal() {
		return
	}
	patterns := taskScratch(job.Document)
	if len(patterns) == 0 {
		return
	}
	// persist already holds the writer mutex: use its durable admission,
	// never call taskEnvelope/lockedStoredJob from this lifecycle hook.
	if len(admission) == 0 || admission[0].RunID == "" {
		return
	}
	root := d.live.manager.Binding(admission[0].RunID, job.TaskID)
	if err := d.cleanupScratchBinding(context.Background(), root, job, patterns); err != nil {
		d.taskLog("task scratch cleanup refused task=%s: %v\n", job.TaskID, err)
	}
}

func removeOwnedScratch(root string, patterns []string) error {
	if err := provider.PhysicalTaskDirectory(root); err != nil {
		return err
	}
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if entry.IsDir() {
			if rel == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !workflow.MatchesAnyGlob(rel, patterns) {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("scratch symlink refuses cleanup: %s", rel)
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return err
	}
	slices.Sort(paths)
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (d *Daemon) cleanupScratchBinding(ctx context.Context, parent worktree.Binding, job engine.Job, patterns []string) error {
	if _, err := os.Lstat(parent.Path); err == nil {
		if d.live.broker == nil {
			return fmt.Errorf("scratch cleanup requires owned broker")
		}
		if _, err = d.live.broker.Tree(ctx, parent); err != nil {
			return err
		}
		if err = removeOwnedScratch(parent.Path, patterns); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, fan := range []*engine.FanState{job.Fan, job.LastFan} {
		if fan == nil {
			continue
		}
		for _, branch := range fan.Branches {
			path := branch.ID
			if branch.Frame != nil && branch.Frame.BranchContext != nil {
				path = branch.Frame.BranchContext.Path
			}
			binding := d.live.manager.Binding(parent.Branch+"/fan/"+fanWorktreeIdentity(fan), path)
			if branch.Frame != nil {
				if err := d.cleanupScratchBinding(ctx, binding, *branch.Frame, patterns); err != nil {
					return err
				}
			} else if _, err := os.Lstat(binding.Path); err == nil {
				if d.live.broker == nil {
					return fmt.Errorf("scratch cleanup requires owned broker")
				}
				if _, err = d.live.broker.Tree(ctx, binding); err != nil {
					return err
				}
				if err = removeOwnedScratch(binding.Path, patterns); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
