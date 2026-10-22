package main

import (
	"context"
	"io"
)

func reviewHealthCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, opts := flags("review-health", c)
	task := f.String("task", "", "task with installed package review workspace binding")
	if e := parse(f, args); e != nil {
		return e
	}
	if *task == "" {
		return usage("review-health requires --task")
	}
	return rpc(ctx, *opts, "ReviewTargetHealth", "", map[string]string{"task": *task}, out)
}
