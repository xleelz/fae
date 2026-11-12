package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/engine"
)

// prepareRunStats is called on the candidate checkpoint before Prepare. No
// runtime credential enters the resulting immutable evidence artifact.
func (d *Daemon) prepareRunStats(ctx context.Context, j *engine.Job) error {
	statsNeeded := false
	for _, stage := range j.Document.Stages {
		statsNeeded = statsNeeded || stage.RunStats != ""
	}
	if !statsNeeded && j.Document.Recursion == nil && j.RootMaxDescendants == 0 {
		return nil
	}
	page, err := d.treeSnapshot(ctx, j.TaskID, statsNeeded)
	if err != nil {
		return err
	}
	j.TreeDescendants = page.Descendants
	if !statsNeeded {
		j.RunStats = nil
		return nil
	}

	treeRaw, err := json.Marshal(page)
	if err != nil {
		return err
	}
	var tree map[string]any
	if err = json.Unmarshal(treeRaw, &tree); err != nil {
		return err
	}
	for _, n := range page.Nodes {
		if n.ID == j.TaskID {
			page.Nodes = []TreeNode{n}
			page.Stats = n.Stats
			page.Tasks = 1
			page.Descendants = 0
			break
		}
	}
	taskRaw, err := json.Marshal(page)
	if err != nil {
		return err
	}
	var task map[string]any
	if err = json.Unmarshal(taskRaw, &task); err != nil {
		return err
	}
	j.RunStats = map[string]any{"tree": tree, "task": task}
	return nil
}
