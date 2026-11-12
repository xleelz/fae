package daemon

import (
	"context"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/provider"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// descendantOutputDirectories uses durable tree membership, including finished
// and detached descendants. Sibling output directories confer no exclusion on
// this task; ancestor identity, rather than task-id prefixes, defines descent.
func (d *Daemon) descendantOutputDirectories(ctx context.Context, task string) ([]string, error) {
	if d == nil || d.store == nil {
		return nil, fmt.Errorf("descendant output directory store unavailable")
	}
	rows, err := d.store.TreeJobs(ctx, task)
	if err != nil {
		return nil, err
	}
	return descendantOutputPaths(d.layout.Root, task, rows)
}
func descendantOutputPaths(root, task string, rows []controlplane.JobSnapshot) ([]string, error) {
	dirs := []string{}
	artifactRoot := filepath.Join(root, "artifacts")
	for _, row := range rows {
		var e envelope
		if err := decodeEnvelope(row.Payload, &e); err != nil {
			return nil, err
		}
		if e.Job.TaskID == task || !slices.Contains(e.Job.Ancestors, task) || e.Job.OutputDir == "" {
			continue
		}
		dir := e.Job.OutputDir
		if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || !strings.HasPrefix(dir, artifactRoot+string(filepath.Separator)) {
			return nil, fmt.Errorf("descendant output directory is outside instance artifacts: %s", e.Job.TaskID)
		}
		if err := provider.PhysicalTaskDirectory(dir); err != nil {
			return nil, fmt.Errorf("descendant output task %s: %w", e.Job.TaskID, err)
		}
		if !slices.Contains(dirs, dir) {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}
