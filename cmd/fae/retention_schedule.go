package main

import (
	"context"
	"io"
	"time"
)

func backupRetentionScheduleCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("backup retention-schedule configure|pause|resume|status")
	}
	action := args[0]
	if isHelp(action) {
		return subcommandHelp("backup retention-schedule", "configure|pause|resume|status", c.json || wantsJSON(args), out)
	}
	switch action {
	case "configure", "pause", "resume", "status":
	default:
		return usage("unknown retention schedule action")
	}
	f, o := flags("backup retention-schedule "+action, c)
	var interval *time.Duration
	var keep *int
	if action == "configure" {
		interval = f.Duration("interval", time.Hour, "time between retention runs (1s..365d, whole seconds)")
		keep = f.Int("keep", 3, "minimum newest verified backups to retain")
	}
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	p := map[string]any{"action": action, "authorizing_message": o.message}
	if action == "configure" {
		if *interval < time.Second || *interval > 365*24*time.Hour || *interval%time.Second != 0 || *keep < 1 || *keep > 10000 {
			return usage("interval must be whole seconds in 1s..365d; keep must be 1..10000")
		}
		p["interval_seconds"] = int64(*interval / time.Second)
		p["keep"] = *keep
	}
	return rpc(ctx, *o, "BackupRetentionSchedule", "", p, out)
}
