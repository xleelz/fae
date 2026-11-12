package main

import (
	"context"
	"errors"
	"flag"
	"github.com/xleelz/fae/daemon"
	"io"
)

func workflowLimitCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, opts := flags("workflow-limit", c)
	max := f.Int("max", -1, "requested host workflow limit (-1 withdraws request)")
	if err := parse(f, args); err != nil {
		return err
	}
	set := false
	f.Visit(func(flag *flag.Flag) {
		if flag.Name == "max" {
			set = true
		}
	})
	if !set {
		return rpc(ctx, *opts, "WorkflowLimit", "", struct{}{}, out)
	}
	if *max != -1 && (*max < 1 || *max > 1000000) {
		return errors.New("workflow limit must be -1 or 1..1000000")
	}
	if err := requireMessage("workflow-limit", opts); err != nil {
		return err
	}
	return rpc(ctx, *opts, "WorkflowLimitSet", "", daemon.WorkflowLimitParams{Max: *max, AuthorizingMessage: opts.message}, out)
}
