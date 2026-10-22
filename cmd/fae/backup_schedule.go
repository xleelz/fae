package main

import (
	"context"
	"io"
	"time"
)

func backupAutomaticScheduleCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) < 2 {
		return usage("backup creation-schedule|replication-schedule configure|pause|resume|status")
	}
	kind, action := args[0], args[1]
	switch action {
	case "configure", "pause", "resume", "status":
	default:
		return usage("unknown backup schedule action")
	}
	f, o := flags("backup "+kind+" "+action, c)
	var interval *time.Duration
	var endpoint *string
	var maxBytes *int64
	if action == "configure" {
		interval = f.Duration("interval", time.Hour, "time between runs (1s..365d, whole seconds; must fit grant lifetime)")
		if kind == "replication-schedule" {
			endpoint = f.String("endpoint", "", "replica service HTTPS URL (literal loopback HTTP permitted)")
			maxBytes = f.Int64("max-bytes", 0, "archive bound (0 uses 256 MiB; maximum 1 GiB)")
		}
	}
	if err := parse(f, args[2:]); err != nil {
		return err
	}
	params := map[string]any{"action": action, "authorizing_message": o.message}
	if action == "configure" {
		if *interval < time.Second || *interval > 365*24*time.Hour || *interval%time.Second != 0 {
			return usage("interval must be whole seconds in 1s..365d")
		}
		params["interval_seconds"] = int64(*interval / time.Second)
		if endpoint != nil {
			if *endpoint == "" || *maxBytes < 0 || *maxBytes > 1<<30 || (*maxBytes > 0 && *maxBytes < 1024) {
				return usage("endpoint required; max-bytes must be 0 or 1024..1073741824")
			}
			params["endpoint"] = *endpoint
			params["max_bytes"] = *maxBytes
		}
	}
	method := "BackupSchedule"
	if kind == "replication-schedule" {
		method = "BackupReplicationSchedule"
	}
	return rpc(ctx, *o, method, "", params, out)
}
