package daemon

import (
	"context"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
)

// observeProvider records a turn's provider outcome. A non-ok outcome that
// adds a row gets exactly one supervision line (FAE-OBS-006, FAE-RT-009); a
// replayed observation adds neither.
func (d *Daemon) observeProvider(ctx context.Context, dispatch engine.Dispatch, o controlplane.ProviderObservation) error {
	recorded, err := d.store.ProviderObservationRecorded(ctx, o.ID)
	if err != nil {
		return err
	}
	if err = d.store.ObserveProvider(ctx, o); err != nil {
		return err
	}
	d.notifyEvents()
	if !recorded && o.Outcome != providerhealth.OK {
		d.logLine("provider", "provider observation outcome=%s provider=%s model=%s session=%q %s\n", o.Outcome, o.Provider, o.Model, o.ID, logTask(dispatch.TaskID, dispatch.Stage, dispatch.ID))
	}
	return nil
}
