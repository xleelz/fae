package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

// refusalEventKind is the instance event of a refused mutating request.
const refusalEventKind = "rpc-refused"

// pairedHandle is the RPC handler: every refusal of a mutating request (an
// action the caller is not permitted, a maintenance or shutdown refusal, a
// validation or authority failure) produces exactly one event and then exactly
// one supervision line (FAE-OBS-006). The line is written only after the event
// committed, so no line exists without its row; reads answer without either.
// Unauthenticated calls never reach a handler and have no side effect.
func (d *Daemon) pairedHandle(ctx context.Context, r api.Request) (any, error) {
	result, err := d.handle(ctx, r)
	if err == nil || errors.Is(err, context.Canceled) {
		return result, err
	}
	if _, permission, _, authErr := authorizePrincipal(ctx, r); authErr == nil && permission.Read {
		return result, err
	}
	d.pairRefusal(r.Method, err)
	return result, err
}

func (d *Daemon) pairRefusal(method string, refusal error) {
	if d.store == nil {
		return
	}
	code := "refused"
	var rpc *api.RPCError
	if errors.As(refusal, &rpc) && rpc.Code != "" {
		code = rpc.Code
	}
	reason := refusal.Error()
	if len(reason) > 512 {
		reason = reason[:512]
	}
	body, _ := json.Marshal(map[string]string{"method": method, "code": code, "reason": reason})
	write, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.store.ControllerAction(write, controlplane.Audit{Actor: "daemon", Action: refusalEventKind, Outcome: "refused"}, body, false); err != nil {
		return
	}
	d.notifyEvents()
	d.logLine("rpc", "refused method=%s code=%s reason=%q\n", method, code, reason)
}
