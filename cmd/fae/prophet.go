package main

import (
	"context"
	"github.com/xleelz/fae/daemon"
	"io"
)

func prophetCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("prophet requires session|chat|global|restart|resume")
	}
	sub := args[0]
	f, opts := flags("prophet "+sub, c)
	p := daemon.ProphetParams{}
	f.StringVar(&p.Task, "task", "", "task ID (omitted for global)")
	f.StringVar(&p.ActionTask, "action-task", "", "explicit global direction target")
	f.StringVar(&p.Text, "text", "", "operator message")
	f.Int64Var(&p.After, "after", 0, "chat cursor")
	f.IntVar(&p.Limit, "limit", 50, "page size")
	direct := f.String("direct", "", "explicit permitted action: RestartWithInformation or ResumeWithContext")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	method := "ProphetSession"
	switch sub {
	case "session":
		if p.Text != "" {
			return usage("session does not send text")
		}
	case "chat":
		if p.Task == "" {
			return usage("chat requires --task")
		}
		method = "ProphetChat"
	case "global":
		if p.Task != "" {
			return usage("global does not accept --task")
		}
		if p.Text != "" {
			method = "ProphetChat"
		}
	default:
		return usage("unknown Prophet command")
	}
	if *direct != "" {
		p.Direct = []string{*direct}
	}
	return rpc(ctx, *opts, method, "", p, out)
}
