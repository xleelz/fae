package main

import (
	"context"
	"github.com/xleelz/fae/daemon"
	"io"
)

func reviewWorkspaceCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 || (args[0] != "review-plan" && args[0] != "review-prepare") {
		return usage("workspace requires review-plan|review-prepare")
	}
	action := args[0]
	f, opts := flags("workspace "+action, c)
	p := daemon.ReviewWorkspaceParams{}
	f.StringVar(&p.Task, "task", "", "task identity")
	f.StringVar(&p.RunID, "run-id", "", "run identity; default daemon run")
	f.StringVar(&p.Adapter.ID, "adapter", "", "installed adapter bundle")
	f.Int64Var(&p.Adapter.Revision, "adapter-revision", 0, "exact adapter revision; zero selects latest")
	f.StringVar(&p.Adapter.Hash, "adapter-hash", "", "expected adapter hash")
	if err := parse(f, args[1:]); err != nil {
		return err
	}
	if p.Task == "" || p.Adapter.Revision < 0 {
		return usage("workspace requires task and nonnegative adapter revision")
	}
	method := "ReviewWorkspacePlan"
	if action == "review-prepare" {
		if p.Adapter.ID == "" {
			return usage("review-prepare requires --adapter")
		}
		if err := requireMessage("workspace review-prepare", opts); err != nil {
			return err
		}
		method = "ReviewWorkspacePrepare"
		p.AuthorizingMessage = opts.message
	}
	return rpc(ctx, *opts, method, "", p, out)
}
