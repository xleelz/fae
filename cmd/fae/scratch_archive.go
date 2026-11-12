package main

import (
	"context"
	"io"
)

// scratchArchiveCleanupCommand goes through the serving instance's daemon
// (FAE-CLI-001): --instance names the live daemon, --scratch-instance the
// removed scratch instance under the same home. Apply needs a message.
func scratchArchiveCleanupCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, o := flags("cleanup scratch-archive", c)
	scratch := f.String("scratch-instance", "", "removed scratch instance whose archives are cleaned")
	source := f.String("source", "", "redundant noncanonical scratch archive")
	survivor := f.String("survivor", "", "canonical archive from completed scratch cleanup")
	digest := f.String("plan-digest", "", "reviewed preview digest")
	apply := f.Bool("apply", false, "remove the verified redundant source")
	dry := f.Bool("dry-run", false, "preview only (default)")
	if err := parse(f, args); err != nil {
		return err
	}
	if *scratch == "" || *source == "" || *survivor == "" || (*apply && (*dry || *digest == "")) {
		return usage("scratch-instance, source and survivor required; apply requires plan-digest and excludes dry-run")
	}
	p := map[string]any{"scratch_instance": *scratch, "source": *source, "survivor": *survivor}
	if *apply {
		p["apply"], p["plan_digest"], p["authorizing_message"] = true, *digest, o.message
	}
	return rpc(ctx, *o, "ScratchArchiveCleanup", "", p, out)
}
