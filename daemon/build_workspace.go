package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/xleelz/fae/workspace"
)

// pinBuildWorkspace is FAE-ARC-023 at bind time: the build-workspace identity
// detected for the bound workspace is pinned in the binding record on first
// bind (or first start of a legacy record) and re-verified on every later
// start; a changed identity (kind, root, marker, provider) refuses the start,
// while a member-preserving marker edit refreshes the pinned digest. An
// ambiguous detection keeps the pin and does not refuse the start; live
// execution refuses it (liveBuildWorkspace).
func pinBuildWorkspace(bindingPath, ws string) (*workspace.BuildWorkspace, error) {
	var stored struct {
		BuildWorkspace *workspace.BuildWorkspace `json:"build_workspace"`
	}
	if raw, err := os.ReadFile(bindingPath); err == nil {
		if err = json.Unmarshal(raw, &stored); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if stored.BuildWorkspace != nil {
		current, err := workspace.VerifyBuildWorkspace(ws, *stored.BuildWorkspace)
		if errors.Is(err, workspace.ErrAmbiguousBuildWorkspace) {
			return stored.BuildWorkspace, nil
		}
		return &current, err
	}
	detected, err := workspace.DetectBuildWorkspace(ws)
	if err != nil {
		return nil, nil
	}
	return &detected, nil
}

// liveBuildWorkspace selects the task worktree provider for live execution:
// a standalone workspace uses git worktrees; a package of a build workspace
// needs a pre-provisioned build-workspace binding (FAE-EXT-015), which Fae
// never creates, syncs or merges itself, so live execution refuses it, as it
// refuses an ambiguous or changed identity.
func (d *Daemon) liveBuildWorkspace() error {
	pinned, err := pinBuildWorkspace(d.layout.Binding, d.config.Workspace)
	if err != nil {
		return fmt.Errorf("FAE-ARC-023: %w", err)
	}
	if _, err = workspace.DetectBuildWorkspace(d.config.Workspace); err != nil || pinned == nil {
		return fmt.Errorf("FAE-ARC-023: live execution refused: %v", err)
	}
	if pinned.Provider != workspace.ProviderGitWorktree {
		return fmt.Errorf("FAE-ARC-023: live execution refused: the workspace is a package of the %s build workspace at %s, which needs a pre-provisioned %s (FAE-EXT-015); Fae never creates or syncs build workspaces", pinned.Kind, pinned.Root, pinned.Provider)
	}
	return nil
}
