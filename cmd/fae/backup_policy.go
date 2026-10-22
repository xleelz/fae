package main

import (
	"context"
	"io"
)

func backupPolicyCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if args[0] == "unpin" || args[0] == "quota" {
		return backupStorageCommand(ctx, args, c, out)
	}
	if args[0] == "creation-schedule" || args[0] == "replication-schedule" {
		return backupAutomaticScheduleCommand(ctx, args, c, out)
	}
	if args[0] == "retention-schedule" {
		return backupRetentionScheduleCommand(ctx, args[1:], c, out)
	}
	f, o := flags("backup "+args[0], c)
	if args[0] == "retention" {
		keep := f.Int("keep", 1, "minimum newest verified backups to retain")
		apply := f.Bool("apply", false, "apply the exact reviewed retention plan")
		dry := f.Bool("dry-run", false, "preview only (default)")
		digest := f.String("plan-digest", "", "digest from retention preview")
		if err := parse(f, args[1:]); err != nil {
			return err
		}
		if *keep < 1 || *keep > 10000 || (*apply && (*dry || *digest == "")) {
			return usage("keep must be 1..10000; apply requires plan-digest and excludes dry-run")
		}
		return rpc(ctx, *o, "BackupRetention", "", map[string]any{"keep": *keep, "apply": *apply, "plan_digest": *digest, "authorizing_message": o.message}, out)
	}
	backup := f.String("backup", "", "verified owned plane backup path")
	maxBytes := f.Int64("max-bytes", 0, "maximum archive size (0 uses 256 MiB; ceiling 1 GiB)")
	endpoint := f.String("endpoint", "", "replica service HTTPS URL (literal loopback HTTP for local services)")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	if *backup == "" || *endpoint == "" || *maxBytes < 0 || *maxBytes > 1<<30 {
		return usage("backup replicate requires backup and endpoint")
	}
	return rpc(ctx, *o, "BackupReplicate", "", map[string]any{"backup": *backup, "endpoint": *endpoint, "max_bytes": *maxBytes, "authorizing_message": o.message}, out)
}
