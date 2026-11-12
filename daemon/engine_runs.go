package daemon

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
)

// environmentDelta is an engine run's sanitized-environment delta
// (FAE-ROB-030): the daemon variables the child did not receive, the names the
// declared command set, and the names Fae's policy passed with its own value.
// Only names are kept; no value, and so no secret, is ever recorded.
func environmentDelta(parent, child, explicit []string) controlplane.EnvironmentDelta {
	values := func(env []string) map[string]string {
		out := map[string]string{}
		for _, entry := range env {
			if key, value, ok := strings.Cut(entry, "="); ok && key != "" {
				out[key] = value
			}
		}
		return out
	}
	daemon, passed, set := values(parent), values(child), values(explicit)
	d := controlplane.EnvironmentDelta{Removed: []string{}, Set: []string{}, Replaced: []string{}}
	for key := range set {
		d.Set = append(d.Set, key)
	}
	for key, value := range daemon {
		v, ok := passed[key]
		switch {
		case !ok:
			d.Removed = append(d.Removed, key)
		case v != value && !hasKey(set, key):
			d.Replaced = append(d.Replaced, key)
		}
	}
	for key := range passed {
		if _, inherited := daemon[key]; !inherited && !hasKey(set, key) {
			d.Replaced = append(d.Replaced, key)
		}
	}
	sort.Strings(d.Removed)
	sort.Strings(d.Set)
	sort.Strings(d.Replaced)
	return d
}

func hasKey(m map[string]string, key string) bool { _, ok := m[key]; return ok }

// recordEngineRun stores one completed engine run with its environment delta
// before its evidence can reach a checkpoint; a run that cannot be recorded
// is not evidence.
func (x *liveExecutor) recordEngineRun(ctx context.Context, d engine.Dispatch, argv, explicit []string, run effects.RunEvidence, started time.Time) error {
	id, err := x.daemon.store.RecordEngineRun(ctx, controlplane.EngineRun{TaskID: d.TaskID, DispatchID: d.ID, Command: argv, Tree: run.Tree, Environment: environmentDelta(os.Environ(), run.Environment, explicit), ExitCode: run.ExitCode, ExecutedCount: run.ExecutedCount, OutputDigest: run.OutputDigest, Started: started, Finished: time.Now().UTC()})
	if err != nil {
		return err
	}
	return x.recordFormalReceipt(ctx, d, id, argv, run)
}
