package daemon

import (
	"context"
	"errors"

	"github.com/xleelz/fae/controller"
)

// fastForwardDeployed moves the bound workspace's integration reference to the
// deployed tip only when the current reference is its ancestor (FAE-VCS-011).
// A diverged reference is left unchanged and the rejection is recorded as an
// alerting controller action.
func fastForwardDeployed(ctx context.Context, workspace string, m *controller.Marker, record func(string, *controller.Marker, bool) error) error {
	e := controller.FastForward(ctx, workspace, m.IntegrationRef, m.DeployedTip)
	if e == nil {
		return nil
	}
	m.State = "failed"
	m.NextAction = "restore previous binary; reconcile divergent integration reference"
	return errors.Join(e, record("controller-maintenance-deployment-rejected", m, true))
}
