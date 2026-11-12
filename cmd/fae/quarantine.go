package main

import (
	"context"
	"io"
	"strings"

	"github.com/xleelz/fae/daemon"
)

// quarantineCommand: fae quarantine [inspect] [--task ID] | release --task ID
// --authorizing-message ID | retire --task ID --reason TEXT
// --authorizing-message ID (FAE-ROB-042). Each is one RPC.
func quarantineCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	sub := "inspect"
	if len(args) > 0 && (args[0] == "inspect" || args[0] == "release" || args[0] == "retire") {
		sub, args = args[0], args[1:]
	}
	f, opts := flags(verb+" "+sub, c)
	p := daemon.QuarantineParams{}
	f.StringVar(&p.ID, "task", "", "quarantined task ID")
	if sub == "retire" {
		f.StringVar(&p.Reason, "reason", "", "why the quarantined row leaves the drive for good")
	}
	if err := parse(f, args); err != nil {
		return err
	}
	if sub == "inspect" {
		return rpc(ctx, *opts, "QuarantineInspect", "", p, out)
	}
	if p.ID == "" || sub == "retire" && strings.TrimSpace(p.Reason) == "" {
		return usage("quarantine release requires --task; quarantine retire requires --task and --reason")
	}
	if err := requireMessage(verb+" "+sub, opts); err != nil {
		return err
	}
	p.AuthorizingMessage = opts.message
	method := "QuarantineRelease"
	if sub == "retire" {
		method = "QuarantineRetire"
	}
	return rpc(ctx, *opts, method, "", p, out)
}
