package daemon

import (
	"context"

	"github.com/xleelz/fae/effects"
)

// processLimit selects FAE-HOST-015's per-run process cap for brokered host
// commands: the configured task count (default effects.DefaultProcessLimit;
// -1 disables it). A host that cannot delegate pids.max degrades to the
// RLIMIT_NPROC cap; only a host where neither works runs uncapped, logged.
func (d *Daemon) processLimit(ctx context.Context, tasks int) *effects.ProcessLimit {
	if tasks < 0 {
		d.logLine("host", "per-run process limit disabled by configuration (FAE-HOST-015)\n")
		return nil
	}
	if tasks == 0 {
		tasks = effects.DefaultProcessLimit
	}
	limit, err := effects.ProbeProcessLimit(ctx, tasks)
	if err != nil {
		d.logLine("host", "per-run process limit unavailable, brokered runs are uncapped (FAE-HOST-015 gap): %v\n", err)
		return nil
	}
	if limit.Rlimit {
		d.logLine("host", "per-run process limit tasks=%d runner=%s degraded to RLIMIT_NPROC: %s\n", limit.Tasks, limit.Runner, limit.Degraded)
		return limit
	}
	d.logLine("host", "per-run process limit tasks=%d runner=%s\n", limit.Tasks, limit.Runner)
	return limit
}
