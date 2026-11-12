package main

import (
	"context"
	"github.com/xleelz/fae/daemon"
	"io"
)

func treeLimitCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, opts := flags("tree-limit", c)
	task := f.String("task", "", "task in the run tree")
	limit := f.Int("max-sessions", 0, "tree session limit (-1 unbounded)")
	if err := parse(f, args); err != nil {
		return err
	}
	if *task == "" || (*limit != -1 && *limit < 1) {
		return usage("tree-limit requires --task and positive --max-sessions or -1")
	}
	if err := requireMessage("tree-limit", opts); err != nil {
		return err
	}
	return rpc(ctx, *opts, "TreeLimit", "", daemon.TreeLimitParams{Task: *task, MaxSessions: *limit, AuthorizingMessage: opts.message}, out)
}
