package daemon

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/provider"
	"github.com/xleelz/fae/workflow"
)

func (d *Daemon) prepareTaskDirectories(ctx context.Context, p AdmitParams, job *engine.Job) error {
	if !workflow.DeclaredPathSafe(job.TaskID) {
		return fmt.Errorf("unsafe task directory identity")
	}
	if len(p.Writes) > 0 {
		authority := false
		for _, stage := range job.Document.Stages {
			if workflow.ProducesChanges(job.Document, stage) {
				authority = true
			}
		}
		if !authority {
			return fmt.Errorf("task writes cannot grant authority to a read-only workflow")
		}
	}
	root := filepath.Join(d.layout.Root, "artifacts")
	out := filepath.Join(root, filepath.FromSlash(job.TaskID), "out")
	parent := p.ParentTask
	if parent == "" && job.Parent != nil {
		parent = job.Parent.Task
	}
	if parent != "" {
		snapshot, found, err := d.storedJob(ctx, parent)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("parent output directory: task %s not found", parent)
		}
		var e envelope
		if err := decodeEnvelope(snapshot.Payload, &e); err != nil {
			return err
		}
		parentOut := e.Job.OutputDir
		if parentOut == "" {
			parentOut = filepath.Join(root, filepath.FromSlash(parent), "out")
		}
		out = filepath.Join(parentOut, filepath.FromSlash(job.TaskID))
	}
	if !strings.HasPrefix(out, root+string(filepath.Separator)) {
		return fmt.Errorf("task output directory escaped artifact root")
	}
	for name, role := range job.Document.Roles {
		if p.Writes != nil && !workflow.GlobScopeNarrows(p.Writes, role.Writes) {
			return fmt.Errorf("task writes widen role %s scope", name)
		}
	}
	if err := provider.EnsurePhysicalTaskDirectory(out); err != nil {
		return err
	}
	if err := provider.PhysicalTaskDirectory(out); err != nil {
		return err
	}
	job.OutputDir = out
	if job.Document.Staging {
		stage := filepath.Join(d.layout.Root, "staging", filepath.FromSlash(job.TaskID))
		if err := provider.EnsurePhysicalTaskDirectory(stage); err != nil {
			return err
		}
		if err := provider.PhysicalTaskDirectory(stage); err != nil {
			return err
		}
		job.StagingDir = stage
		marker := filepath.Join(stage, ".fae-staging-owner")
		identity := d.layout.Root + "\n" + job.TaskID + "\n"
		if prior, e := os.ReadFile(marker); e == nil {
			if string(prior) != identity {
				return fmt.Errorf("foreign task staging ownership")
			}
		} else if !os.IsNotExist(e) {
			return e
		} else if e = os.WriteFile(marker, []byte(identity), 0600); e != nil {
			return e
		}
	}
	return nil
}

// cleanupTaskDirectories removes only daemon-created staging bearing this exact
// task's ownership marker. WF111 explicitly requires removal at terminal state;
// outputs are retained. A symlink or unexpected ownership refuses cleanup.
func (d *Daemon) cleanupTaskDirectories(job engine.Job, admission ...AdmitParams) {
	d.cleanupTaskScratch(job, admission...)
	if !job.State.Terminal() || job.StagingDir == "" {
		return
	}
	expected := filepath.Join(d.layout.Root, "staging", filepath.FromSlash(job.TaskID))
	cleanup := func() error {
		if expected != job.StagingDir {
			return fmt.Errorf("foreign staging path")
		}
		if err := provider.PhysicalTaskDirectory(expected); err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		marker := filepath.Join(expected, ".fae-staging-owner")
		raw, err := os.ReadFile(marker)
		if err != nil {
			return err
		}
		if string(raw) != d.layout.Root+"\n"+job.TaskID+"\n" {
			return fmt.Errorf("foreign staging ownership")
		}
		var paths []string
		err = filepath.WalkDir(expected, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("symlink in owned staging refuses cleanup")
			}
			if path != expected && path != marker {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return err
		}
		slices.SortFunc(paths, func(a, b string) int { return len(b) - len(a) })
		for _, path := range paths {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		if err := os.Remove(marker); err != nil {
			return err
		}
		return os.Remove(expected)
	}
	if err := cleanup(); err != nil {
		d.taskLog("task staging cleanup refused task=%s: %v\n", job.TaskID, err)
	}
}
