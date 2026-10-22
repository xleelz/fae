package daemon

import (
	"context"
	"errors"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/engine"
)

func (d *Daemon) validateRetirement(ctx context.Context, e envelope, reason string) error {
	class, reference, err := engine.ParseRetirement(reason)
	if err != nil {
		return err
	}
	switch class {
	case "superseded-by":
		if _, ok := d.jobs[reference]; !ok || reference == e.Job.TaskID {
			return errors.New("successor must identify another existing task")
		}
	case "contained":
		// A branch ancestor is not evidence: accept only this task's own broker
		// receipt, whose provenance the engine validates, and resolve the exact SHA.
		if err = engine.Validate(e.Job); err != nil {
			return err
		}
		resolved, resolveErr := controller.Git(ctx, d.config.Workspace, "rev-parse", "--verify", reference+"^{commit}")
		own := e.Job.LatestCommitHead()
		landed := ownLandingSHA(e)
		if !CanRetireContained(resolveErr == nil && resolved == reference, e.Job.ExecutionKind == "live" && ((own != "" && own == reference) || (landed != "" && landed == reference))) {
			return errors.New("contained requires a resolving exact SHA from this task's own commit evidence")
		}
	}
	return nil
}
