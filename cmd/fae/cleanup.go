package main

import (
	"context"
	"encoding/json"
	"flag"
	"github.com/xleelz/fae/daemon"
	"io"
	"time"
)

func cleanupCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	if verb == "backup" && len(args) > 0 && (args[0] == "unpin" || args[0] == "quota" || args[0] == "retention" || args[0] == "replicate" || args[0] == "retention-schedule" || args[0] == "creation-schedule" || args[0] == "replication-schedule") {
		return backupPolicyCommand(ctx, args, c, out)
	}
	if verb == "cleanup" && len(args) > 0 && args[0] == "scratch-archive" {
		return scratchArchiveCleanupCommand(ctx, args[1:], c, out)
	}
	if verb == "cleanup" && len(args) > 0 && args[0] == "scratch" {
		return scratchCleanupCommand(ctx, args[1:], c, out)
	}
	if len(args) == 0 || (verb == "cleanup" && args[0] != "retired" && args[0] != "done") || (verb == "backup" && args[0] != "plane") {
		return usage("cleanup retired | cleanup done | backup plane")
	}
	f, o := flags(verb+" "+args[0], c)
	// cleanup done: Done tasks whose landing is in the integration ref.
	p := daemon.CleanupParams{Done: verb == "cleanup" && args[0] == "done"}
	applyHelp := "archive and remove the planned retired tasks' worktrees and branches"
	if p.Done {
		applyHelp = "archive and remove the planned integrated Done tasks' worktrees and branches"
	}
	f.BoolVar(&p.Apply, "apply", false, applyHelp)
	f.BoolVar(&p.DryRun, "dry-run", false, "plan only (default)")
	f.StringVar(&p.Backup, "backup", "", "verified plane backup path")
	pace := f.Duration("pace", 100*time.Millisecond, "pause between task removals (minimum 10ms)")
	var limit *int
	if verb == "cleanup" {
		limit = f.Int("limit", 200, "most tasks planned in one run (1..10000; already-cleaned and skipped tasks are not counted)")
	}
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	if p.Apply && p.DryRun {
		return usage("apply and dry-run are mutually exclusive")
	}
	p.PaceMS = int(pace.Milliseconds())
	if *pace < 10*time.Millisecond || *pace > time.Minute {
		return usage("pace must be 10ms..1m")
	}
	if limit != nil {
		if *limit < 1 || *limit > 10000 {
			return usage("limit must be 1..10000")
		}
		f.Visit(func(fl *flag.Flag) {
			if fl.Name == "limit" {
				p.Limit = *limit
			}
		})
	}
	p.AuthorizingMessage = o.message
	method := "CleanupRetired"
	if verb == "backup" {
		method = "PlaneBackup"
	}
	return rpc(ctx, *o, method, "", p, out)
}

func scratchCleanupCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, o := flags("cleanup scratch", c)
	apply := f.Bool("apply", false, "stop, back up and remove marked scratch instance")
	dry := f.Bool("dry-run", false, "inspect only (default)")
	binary := f.String("dolt", "dolt", "Dolt executable for offline backup")
	if err := parse(f, args); err != nil {
		return err
	}
	if *apply && *dry {
		return usage("apply and dry-run are mutually exclusive")
	}
	result, err := daemon.CleanupScratch(ctx, daemon.ScratchCleanupParams{Home: o.home, Instance: o.instance, DoltBinary: *binary, AuthorizingMessage: o.message, Apply: *apply})
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
