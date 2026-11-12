package main

import (
	"context"
	"io"
	"time"

	"github.com/xleelz/fae/daemon"
)

func metricsCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	f, opts := flags(verb, c)
	if verb == "host-metrics" {
		if err := parse(f, args); err != nil {
			return err
		}
		return rpc(ctx, *opts, "HostMetrics", "", map[string]any{}, out)
	}
	window := f.Duration("window", 24*time.Hour, "event window for derived metrics (at most 7 days)")
	if err := parse(f, args); err != nil {
		return err
	}
	if *window < time.Second || *window > 7*24*time.Hour {
		return usage("metrics window must be 1s..168h")
	}
	return rpc(ctx, *opts, "Metrics", "", daemon.MetricsParams{WindowSeconds: int64(window.Seconds())}, out)
}
