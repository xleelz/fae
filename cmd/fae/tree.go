package main

import (
	"context"
	"github.com/xleelz/fae/daemon"
	"io"
)

func treeCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	if len(args) == 0 {
		return usage("tree requires a task id")
	}
	task := ""
	if args[0] != "--help" && args[0] != "-h" {
		task = args[0]
		args = args[1:]
	}
	f, opts := flags("tree", c)
	stats := f.Bool("stats", false, "include exact provider readings and measured wall times")
	if err := parse(f, args); err != nil {
		return err
	}
	if task == "" {
		return usage("tree requires a task id")
	}
	return rpc(ctx, *opts, "Tree", "", daemon.TreeParams{Task: task, Stats: *stats}, out)
}
