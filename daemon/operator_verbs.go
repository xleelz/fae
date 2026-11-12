package daemon

import (
	"context"
	"fmt"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

// operatorRead serves the operator read verbs added for §11.4–11.5 from the plane.
func (d *Daemon) operatorRead(ctx context.Context, r api.Request) (any, bool, error) {
	switch r.Method {
	case "Inbox":
		v, err := d.inbox(ctx, r.Params)
		return v, true, err
	case "AnalyzeSession":
		v, err := d.analyzeSession(ctx, r.Params)
		return v, true, err
	case "QuarantineInspect":
		v, err := d.quarantineInspect(ctx, r.Params)
		return v, true, err
	}
	return nil, false, nil
}

// operatorVerb dispatches the audited operator mutations added for §10–11.
func (d *Daemon) operatorVerb(ctx context.Context, r api.Request, audit controlplane.Audit) (any, error) {
	switch r.Method {
	case "AttachDesignDoc":
		return d.attachDesignDoc(ctx, r.Params, audit)
	case "Reclaim":
		return d.reclaim(ctx, r.Params, audit)
	case "Migrate":
		return d.migrate(ctx, r.Params, audit)
	case "Decide":
		return d.decide(ctx, r.Params, audit)
	case "QuarantineRelease":
		return d.quarantineRelease(ctx, r.Params, audit)
	case "QuarantineRetire":
		return d.quarantineRetire(ctx, r.Params, audit)
	}
	return nil, fmt.Errorf("unknown RPC method %q", r.Method)
}
