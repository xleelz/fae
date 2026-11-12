package main

import (
	"context"
	"io"

	"github.com/xleelz/fae/daemon"
)

// operatorCommands are the §10–11 operator, maintenance and analysis verbs.
var operatorCommands = map[string]func(context.Context, string, []string, common, io.Writer) error{
	"message": messageCommand, "reply": messageCommand, "inbox": messageCommand,
	"attach-design-doc": attachDesignDocCommand, "reclaim": reclaimCommand,
	"analyze-session": analyzeSessionCommand, "heavy-op": heavyOpCommand,
	"doctor": doctorCommand, "migrate": migrateCommand,
	"decide":          decideCommand,
	"supply-artifact": supplyArtifactCommand,
	"quarantine":      quarantineCommand,
}

func attachDesignDocCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, opts := flags(verb, c)
	p := daemon.DesignDocParams{}
	f.StringVar(&p.Task, "task", "", "task ID")
	f.StringVar(&p.Item, "item", "", "backlog item ID")
	f.StringVar(&p.Reference, "reference", "", "http(s) URL or path inside the target's containment root")
	if err := parse(f, args); err != nil {
		return err
	}
	if (p.Task == "") == (p.Item == "") || p.Reference == "" {
		return usage("attach-design-doc requires exactly one of --task or --item, and --reference")
	}
	if err := requireMessage(verb, opts); err != nil {
		return err
	}
	p.AuthorizingMessage = opts.message
	return rpc(ctx, *opts, "AttachDesignDoc", "", p, out)
}

func reclaimCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, opts := flags(verb, c)
	p := daemon.ReclaimParams{}
	f.BoolVar(&p.HostWide, "host-wide", false, "also reconcile host-scoped heavy-op and pane ledgers (needs a host-wide message class)")
	f.BoolVar(&p.DryRun, "dry-run", false, "report reclaimed and kept entries without writing")
	if err := parse(f, args); err != nil {
		return err
	}
	if err := requireMessage(verb, opts); err != nil {
		return err
	}
	p.AuthorizingMessage = opts.message
	return rpc(ctx, *opts, "Reclaim", "", p, out)
}

func analyzeSessionCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, opts := flags(verb, c)
	p := daemon.AnalyzeParams{}
	f.StringVar(&p.Task, "task", "", "task ID")
	if err := parse(f, args); err != nil {
		return err
	}
	if p.Task == "" {
		return usage("analyze-session requires --task")
	}
	return rpc(ctx, *opts, "AnalyzeSession", "", p, out)
}

func migrateCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, opts := flags(verb, c)
	p := daemon.MigrateParams{}
	f.StringVar(&p.ID, "task", "", "task ID")
	f.Int64Var(&p.Revision, "revision", 0, "named target revision of the task's workflow")
	f.StringVar(&p.Stage, "stage", "", "mapped stage in the new revision (default: the task's current position)")
	if err := parse(f, args); err != nil {
		return err
	}
	if p.ID == "" || p.Revision < 1 {
		return usage("migrate requires --task and --revision >= 1")
	}
	if err := requireMessage(verb, opts); err != nil {
		return err
	}
	p.AuthorizingMessage = opts.message
	return rpc(ctx, *opts, "Migrate", "", p, out)
}
