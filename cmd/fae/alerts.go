package main

import (
	"context"
	"io"

	"github.com/xleelz/fae/daemon"
)

// observabilityCommand: fae alerts [list] [--after N] [--limit N]
// [--unacknowledged] | fae alerts ack --id N --authorizing-message ID |
// fae metrics [--window D] | fae host-metrics. Each is one RPC.
func observabilityCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	if verb != "alerts" {
		return metricsCommand(ctx, verb, args, c, out)
	}
	sub := "list"
	if len(args) > 0 && (args[0] == "list" || args[0] == "ack") {
		sub, args = args[0], args[1:]
	}
	f, opts := flags("alerts "+sub, c)
	if sub == "ack" {
		p := daemon.AlertAcknowledgeParams{}
		f.Int64Var(&p.ID, "id", 0, "alert ID")
		if err := parse(f, args); err != nil {
			return err
		}
		if p.ID <= 0 {
			return usage("alerts ack requires --id")
		}
		if err := requireMessage("alerts ack", opts); err != nil {
			return err
		}
		p.AuthorizingMessage = opts.message
		return rpc(ctx, *opts, "AlertAcknowledge", "", p, out)
	}
	p := daemon.AlertsParams{}
	f.Int64Var(&p.After, "after", 0, "resume after alert ID")
	f.IntVar(&p.Limit, "limit", 100, "page size 1..500")
	f.BoolVar(&p.Unacknowledged, "unacknowledged", false, "only alerts not yet acknowledged")
	if err := parse(f, args); err != nil {
		return err
	}
	if p.After < 0 || p.Limit < 1 || p.Limit > 500 {
		return usage("alerts cursor must be nonnegative and limit 1..500")
	}
	return rpc(ctx, *opts, "Alerts", "", p, out)
}
