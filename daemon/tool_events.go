package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"time"
)

func (d *Daemon) recordToolEvent(dispatch engine.Dispatch, event domain.ToolEvent) {
	raw, err := json.Marshal(map[string]any{"dispatch_id": dispatch.ID, "stage": dispatch.Stage, "branch_id": dispatch.BranchID, "tool": event.Tool, "paths": event.Paths, "allowed": event.Allowed, "at": event.At})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(d.ctx), 5*time.Second)
	defer cancel()
	if err = d.store.RecordTaskEvent(ctx, controlplane.Audit{Actor: "runtime", Action: "tool-call", Outcome: "observed"}, dispatch.TaskID, controlplane.Event{Kind: "tool-call", Payload: raw}); err == nil {
		d.notifyEvents()
	}
}
