package main

import (
	"context"
	"github.com/xleelz/fae/daemon"
	"io"
)

func changeRequestCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("change-request requires list or disposition")
	}
	action := args[0]
	f, opts := flags("change-request "+action, c)
	task := f.String("task", "", "target ancestor task")
	id := f.String("id", "", "change request ID")
	state := f.String("state", "", "accepted or declined")
	reason := f.String("reason", "", "operator disposition reason")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	if *task == "" {
		return usage("change-request requires --task")
	}
	switch action {
	case "list":
		return rpc(ctx, *opts, "ChangeRequests", "", daemon.ChangeRequestsParams{Task: *task}, out)
	case "disposition":
		if *id == "" || (*state != "accepted" && *state != "declined") || *reason == "" {
			return usage("change-request disposition requires --id, --state accepted|declined and --reason")
		}
		if err := requireMessage("change-request disposition", opts); err != nil {
			return err
		}
		return rpc(ctx, *opts, "ChangeRequestDisposition", "", daemon.ChangeRequestDispositionParams{Task: *task, ID: *id, State: *state, Reason: *reason, AuthorizingMessage: opts.message}, out)
	default:
		return usage("change-request requires list or disposition")
	}
}
