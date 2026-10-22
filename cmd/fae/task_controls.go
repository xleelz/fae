package main

import (
	"context"
	"github.com/xleelz/fae/daemon"
	"io"
	"time"
)

var taskControlMethods = map[string]string{"retire": "Retire", "escalate": "Escalate", "block": "Block", "cancel": "Cancel", "defer": "Defer", "undefer": "Undefer", "reprioritize": "Reprioritize", "set-attempt-limit": "SetAttemptLimit", "restart": "RestartWithInformation", "resume-with-context": "ResumeWithContext", "resume": "Resume"}

func taskControlCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, opts := flags(verb, c)
	p := daemon.TaskControlParams{}
	f.StringVar(&p.ID, "task", "", "task ID")
	f.StringVar(&p.Reason, "reason", "", "operator reason")
	f.StringVar(&p.Information, "information", "", "append-only restart information")
	f.StringVar(&p.Context, "context", "", "context for later briefs")
	f.IntVar(&p.Priority, "priority", 0, "future scheduling priority")
	f.IntVar(&p.AttemptLimit, "limit", 0, "per-task attempt limit")
	until := f.String("until", "", "RFC3339 defer deadline; omitted means explicit undefer")
	undefer := f.Bool("undefer", false, "remove Pending deferral")
	if err := parse(f, args); err != nil {
		return err
	}
	if p.ID == "" || opts.message == "" {
		return usage("%s requires --task and --authorizing-message", verb)
	}
	if *until != "" {
		var err error
		p.Until, err = time.Parse(time.RFC3339, *until)
		if err != nil {
			return usage("invalid --until: %v", err)
		}
	}
	method := taskControlMethods[verb]
	if *undefer {
		if verb != "defer" || *until != "" {
			return usage("--undefer requires defer without --until")
		}
		method = "Undefer"
	}
	p.AuthorizingMessage = opts.message
	return rpc(ctx, *opts, method, "", p, out)
}
