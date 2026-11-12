package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/controlplane"
)

type WorkflowLimitParams struct {
	Max                int    `json:"max"`
	AuthorizingMessage string `json:"authorizing_message"`
}

func (d *Daemon) workflowLimitSet(ctx context.Context, raw json.RawMessage, a controlplane.Audit) (any, error) {
	var p WorkflowLimitParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Max != -1 && (p.Max < 1 || p.Max > 1000000) {
		return nil, errors.New("workflow limit must be -1 or 1..1000000")
	}
	return d.setWorkflowRequest(ctx, p.Max, a)
}
