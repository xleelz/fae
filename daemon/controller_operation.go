package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/api"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
)

// ControllerOperation authorizes a complete lifecycle operation before handing
// it to the independently authenticated launcher. The receipt survives this
// daemon's shutdown; each phase still uses the validated, audited Controller RPC.
type ControllerOperationParams struct {
	AuthorizingMessage string                      `json:"authorizing_message"`
	Operation          controller.OperationRequest `json:"operation"`
}

func (d *Daemon) controllerOperation(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p ControllerOperationParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Operation.Status || p.Operation.ID != "" {
		return nil, errors.New("submit a full controller operation; read its receipt from the launcher")
	}
	if p.Operation.Options.Message != p.AuthorizingMessage {
		return nil, errors.New("controller operation citation mismatch")
	}
	if err := controller.ValidateOperation(p.Operation, p.Operation.Options.Launcher); err != nil {
		return nil, err
	}
	if err := d.store.RecordAuthorityDecision(ctx, audit, "controller-operation", p.Operation.Action, p.Operation.Options.Note, raw); err != nil {
		return nil, err
	}
	credential, err := api.RepositoryFreeCredential(d.layout.Credential)
	if err != nil {
		return nil, err
	}
	p.Operation.Authorization = controller.SignOperation(p.Operation, credential)
	return controller.OperationRPC(ctx, p.Operation.Options.Launcher, p.Operation)
}
