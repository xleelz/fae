package main

import (
	"context"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/engine"
	"io"
	"strings"
	"time"
)

var taskControlMethods = map[string]string{"retire": "Retire", "escalate": "Escalate", "block": "Block", "cancel": "Cancel", "defer": "Defer", "undefer": "Undefer", "reprioritize": "Reprioritize", "set-attempt-limit": "SetAttemptLimit", "restart": "RestartWithInformation", "resume-with-context": "ResumeWithContext", "resume-at-stage": "ResumeAtStage"}

func taskControlCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, opts := flags(verb, c)
	p := daemon.TaskControlParams{}
	f.StringVar(&p.ID, "task", "", "task ID")
	f.StringVar(&p.Reason, "reason", "", "operator reason")
	f.StringVar(&p.Information, "information", "", "append-only restart information")
	f.StringVar(&p.Context, "context", "", "context for later briefs")
	f.IntVar(&p.Priority, "priority", 0, "future scheduling priority")
	f.IntVar(&p.AttemptLimit, "limit", 0, "per-task attempt limit")
	informationFile := f.String("information-file", "", "file holding the restart information (excludes --information)")
	contextFile := f.String("context-file", "", "file holding the resume context (excludes --context)")
	until := f.String("until", "", "RFC3339 defer deadline; omitted means explicit undefer")
	undefer := f.Bool("undefer", false, "remove Pending deferral")
	if err := parse(f, args); err != nil {
		return err
	}
	if p.ID == "" {
		return usage("%s requires --task", verb)
	}
	// FAE-CLI-043: the information and the context may each come from a file.
	for _, form := range []struct {
		text *string
		file string
		name string
	}{{&p.Information, *informationFile, "information"}, {&p.Context, *contextFile, "context"}} {
		if form.file == "" {
			continue
		}
		if *form.text != "" {
			return usage("--%s and --%s-file are exclusive", form.name, form.name)
		}
		text, err := inputText("", form.file)
		if err != nil {
			return err
		}
		*form.text = text
	}
	if err := requireMessage(verb, opts); err != nil {
		return err
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

func notifyCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, opts := flags("notify", c)
	p := daemon.TaskNotifyParams{}
	f.StringVar(&p.Task, "task", "", "target task ID")
	f.StringVar(&p.FromTask, "from-task", "", "source task ID (optional)")
	f.StringVar(&p.Kind, "kind", "", "notification kind: breaking-change, cross-area-need, or note")
	f.StringVar(&p.Delivery, "delivery", "next-turn", "delivery mode: next-turn or in-flight")
	f.StringVar(&p.Body, "body", "", "notification body text")
	f.StringVar(&p.Actor, "actor", "", "actor identity")
	f.StringVar(&p.Reason, "reason", "", "notification reason")
	if err := parse(f, args); err != nil {
		return err
	}
	if strings.TrimSpace(p.Task) == "" {
		return usage("notify requires --task")
	}
	if strings.TrimSpace(p.Kind) == "" {
		return usage("notify requires --kind")
	}
	if strings.TrimSpace(p.Body) == "" {
		return usage("notify requires --body")
	}
	p.AuthorizingMessage = opts.message
	return rpc(ctx, *opts, "Notify", "", p, out)
}

func followUpCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, opts := flags("follow-up", c)
	p := daemon.TaskFollowUpParams{}
	var wallSeconds, maxTurns int
	var key string
	f.StringVar(&p.Task, "task", "", "Done predecessor task ID")
	f.StringVar(&p.ID, "id", "", "optional new task ID")
	f.StringVar(&p.Text, "text", "", "follow-up objective / prompt")
	f.IntVar(&wallSeconds, "wall-seconds", 0, "turn budget wall clock seconds")
	f.IntVar(&maxTurns, "max-turns", 0, "turn budget maximum turns")
	f.StringVar(&p.Actor, "actor", "", "actor identity")
	f.StringVar(&p.Reason, "reason", "", "follow-up reason")
	f.StringVar(&key, "idempotence-key", "", "caller-supplied idempotence key")
	if err := parse(f, args); err != nil {
		return err
	}
	if strings.TrimSpace(p.Task) == "" {
		return usage("follow-up requires --task")
	}
	if strings.TrimSpace(p.Text) == "" {
		return usage("follow-up requires --text")
	}
	if wallSeconds < 0 || maxTurns < 0 {
		return usage("follow-up budget values must not be negative")
	}
	if wallSeconds > 0 || maxTurns > 0 {
		p.Budget = &engine.TurnBudgetSpec{
			WallSeconds: wallSeconds,
			MaxTurns:    maxTurns,
		}
	}
	p.AuthorizingMessage = opts.message
	return rpc(ctx, *opts, "FollowUp", key, p, out)
}
