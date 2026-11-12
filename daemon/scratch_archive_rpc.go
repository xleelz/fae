package daemon

import (
	"context"
	"errors"

	"github.com/xleelz/fae/api"
)

// ScratchArchiveRPCParams is FAE-CLI-001's daemon route for redundant scratch
// archive cleanup: the live instance performs it for a removed scratch
// instance under the same home. Apply needs a fresh authorizing message.
type ScratchArchiveRPCParams struct {
	ScratchInstance    string `json:"scratch_instance"`
	Source             string `json:"source"`
	Survivor           string `json:"survivor"`
	Apply              bool   `json:"apply,omitempty"`
	PlanDigest         string `json:"plan_digest,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

func (d *Daemon) scratchArchiveRPC(ctx context.Context, r api.Request) (any, error) {
	principal, ok := api.AuthenticatedPrincipal(ctx)
	if !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("scratch archive cleanup requires instance operator")
	}
	var p ScratchArchiveRPCParams
	if err := decode(r.Params, &p); err != nil {
		return nil, err
	}
	if p.ScratchInstance == "" || p.ScratchInstance == d.config.Instance {
		return nil, errors.New("scratch archive cleanup names a removed scratch instance, not the serving instance")
	}
	if p.Apply {
		if err := d.store.AuthorizeReadOnlyEffect(ctx, p.AuthorizingMessage, principal.Actor, "ScratchArchiveCleanup", "runtime", actionClasses()); err != nil {
			return nil, err
		}
	}
	return CleanupScratchArchive(ctx, ScratchArchiveCleanupParams{Home: d.config.Root, Instance: p.ScratchInstance, Source: p.Source, Survivor: p.Survivor, Apply: p.Apply, PlanDigest: p.PlanDigest})
}
