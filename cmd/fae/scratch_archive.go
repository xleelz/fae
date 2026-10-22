package main

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/daemon"
	"io"
)

func scratchArchiveCleanupCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, o := flags("cleanup scratch-archive", c)
	source := f.String("source", "", "redundant noncanonical scratch archive")
	survivor := f.String("survivor", "", "canonical archive from completed scratch cleanup")
	digest := f.String("plan-digest", "", "reviewed preview digest")
	apply := f.Bool("apply", false, "remove the verified redundant source")
	dry := f.Bool("dry-run", false, "preview only (default)")
	if err := parse(f, args); err != nil {
		return err
	}
	if *source == "" || *survivor == "" || (*apply && (*dry || *digest == "")) {
		return usage("source and survivor required; apply requires plan-digest and excludes dry-run")
	}
	result, err := daemon.CleanupScratchArchive(ctx, daemon.ScratchArchiveCleanupParams{Home: o.home, Instance: o.instance, Source: *source, Survivor: *survivor, Apply: *apply, PlanDigest: *digest})
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(result)
}
