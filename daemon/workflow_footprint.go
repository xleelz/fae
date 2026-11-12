package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/workflow"
)

func (d *Daemon) resolveFootprint(ctx context.Context, doc workflow.Document, max int) (workflow.Footprint, error) {
	return workflow.DeriveFootprint(doc, func(ref workflow.WorkflowRef) (workflow.Document, error) {
		_, raw, err := d.storedWorkflow(ctx, ref.ID, int64(ref.Revision))
		if err != nil {
			return workflow.Document{}, err
		}
		return d.parseWorkflow(raw)
	}, max)
}

func (d *Daemon) storedFootprint(ctx context.Context, id string, revision int64) (*workflow.Footprint, error) {
	raw, err := d.store.GetMarker(ctx, controlplane.WorkflowFootprintKey(id, revision))
	if err != nil {
		return nil, err
	}
	var f workflow.Footprint
	if err = json.Unmarshal(raw, &f); err != nil {
		return nil, err
	}
	if (f.SlotScope != "molecule" && f.SlotScope != "tree") || f.DerivedSessions < 0 || f.EffectiveSessions < f.DerivedSessions || f.EffectiveSessions > 1000000 {
		return nil, fmt.Errorf("invalid pinned workflow footprint")
	}
	return &f, nil
}
