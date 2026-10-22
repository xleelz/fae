package main

import (
	"context"
	"io"
)

func backupStorageCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if args[0] == "unpin" {
		f, o := flags("backup unpin", c)
		backup := f.String("backup", "", "verified owned replication source")
		apply := f.Bool("apply", false, "apply the exact reviewed unpin plan")
		dry := f.Bool("dry-run", false, "preview only (default)")
		digest := f.String("plan-digest", "", "digest from unpin preview")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		if *backup == "" || (*apply && (*dry || *digest == "")) {
			return usage("backup unpin requires backup; apply requires plan-digest and excludes dry-run")
		}
		return rpc(ctx, *o, "BackupReplicationUnpin", "", map[string]any{"backup": *backup, "apply": *apply, "plan_digest": *digest, "authorizing_message": o.message}, out)
	}
	if len(args) < 2 || (args[1] != "configure" && args[1] != "status") {
		return usage("backup quota configure|status")
	}
	f, o := flags("backup quota "+args[1], c)
	params := map[string]any{"action": args[1], "authorizing_message": ""}
	var maxBytes *int64
	var maxBackups *int
	if args[1] == "configure" {
		maxBytes = f.Int64("max-bytes", 0, "backup and replication spool byte limit (0 unlimited; export checkpoint enforcement)")
		maxBackups = f.Int("max-backups", 0, "plane backup directory limit including partial backups (0 unlimited)")
	}
	if err := parse(f, args[2:]); err != nil {
		return err
	}
	params["authorizing_message"] = o.message
	if maxBytes != nil {
		if *maxBytes < 0 || *maxBytes > 1<<60 || *maxBackups < 0 || *maxBackups > 10000 {
			return usage("max-bytes must be 0..1152921504606846976; max-backups must be 0..10000")
		}
		params["max_bytes"] = *maxBytes
		params["max_backups"] = *maxBackups
	}
	return rpc(ctx, *o, "BackupQuota", "", params, out)
}
