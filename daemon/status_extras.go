package daemon

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xleelz/fae/hostguard"
)

// statusExtras holds status fields that need SQL, ledger files or /proc. They
// are sampled off the request path so Status never waits for the read pool,
// the writer mutex or the plane (FAE-CLI-012).
type statusExtras struct {
	At             time.Time
	LastCommit     any
	Leases         any
	Panes          any
	HeavyOps       any
	ProviderPools  any
	WorkflowLimit  any
	DispatchPauses any
	Host           any
	ProviderHealth any
	Errors         map[string]string
}
type statusExtrasState struct {
	value      atomic.Pointer[statusExtras]
	refreshing atomic.Bool
}

const statusExtrasMaxAge = 2 * time.Second

// refreshStatusExtras starts at most one background sample when the cached
// one is stale; it never blocks its caller. Only a daemon built by Start
// samples, so unit fixtures with mocked stores are never queried.
func (d *Daemon) refreshStatusExtras() {
	if d.store == nil || d.started.IsZero() || d.ctx == nil || d.ctx.Err() != nil {
		return
	}
	if v := d.statusExtras.value.Load(); v != nil && time.Since(v.At) < statusExtrasMaxAge {
		return
	}
	if !d.statusExtras.refreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer d.statusExtras.refreshing.Store(false)
		ctx, cancel := context.WithTimeout(d.ctx, 3*time.Second)
		defer cancel()
		d.statusExtras.value.Store(d.sampleStatusExtras(ctx, hostguard.Observe))
	}()
}

// sampleStatusExtras reads the host directly rather than the admission
// sampler, which tests replace on running daemons.
func (d *Daemon) sampleStatusExtras(ctx context.Context, observe func(context.Context) (hostguard.Sample, error)) *statusExtras {
	x := &statusExtras{Errors: map[string]string{}}
	note := func(field string, e error) bool {
		if e != nil {
			x.Errors[field] = e.Error()
		}
		return e == nil
	}
	if head, e := d.store.PlaneHead(ctx); note("plane.last_commit", e) {
		x.LastCommit = head
	}
	if leases, e := d.store.LeaseUsageByKind(ctx); note("leases", e) {
		x.Leases = leases
	}
	health, e := d.store.ProviderHealth(ctx)
	if note("provider_health", e) {
		now := time.Now().UTC()
		ineligible := []string{}
		for _, h := range health {
			if !h.Eligible(now) {
				ineligible = append(ineligible, h.Provider+"/"+h.Model)
			}
		}
		x.ProviderHealth = map[string]any{"bindings": len(health), "eligible": len(health) - len(ineligible), "ineligible": ineligible}
	}
	if d.workflowSlots != nil {
		if view, e := d.workflowSlotSnapshot(ctx); note("workflow_limit", e) {
			x.WorkflowLimit = view
		}
	}
	if pauses, e := d.store.ListDispatchPauses(ctx); e == nil {
		x.DispatchPauses = pauses
	}
	if d.live != nil && d.live.guard != nil {
		a := d.live.guard
		if a.host != nil {
			if pool, e := a.host.Snapshot(ctx); note("heavy_ops", e) {
				capacity := pool.Capacity
				if pool.AdmissionLimit > 0 {
					capacity = pool.AdmissionLimit
				}
				holders := []string{}
				for _, l := range pool.Holders {
					holders = append(holders, l.ID)
				}
				x.HeavyOps = map[string]any{"capacity": capacity, "holders": holders, "waiters": len(pool.Waiting)}
			}
			if pools, e := a.host.ProviderPoolsSnapshot(ctx); e == nil {
				x.ProviderPools = pools
			}
		}
		if a.panes != nil {
			if panes, e := a.panes.Snapshot(ctx); note("panes", e) {
				owned := 0
				for _, p := range panes {
					if p.Owner == a.owner {
						owned++
					}
				}
				x.Panes = map[string]any{"live": len(panes), "limit": a.effectivePaneLimit(), "owned": owned}
			}
		}
	}
	if s, e := observe(ctx); note("host", e) {
		reading := func(r hostguard.Reading) any {
			if !r.Known {
				return nil
			}
			return r.Value
		}
		x.Host = map[string]any{"memory_available_bytes": reading(s.Memory), "swap_total_bytes": reading(s.Swap), "io_full_avg10": reading(s.IO10), "io_full_avg60": reading(s.IO60), "memory_full_avg10": reading(s.Memory10), "memory_full_avg60": reading(s.Memory60), "load_per_core": reading(s.LoadPerCore)}
	}
	x.At = time.Now().UTC()
	return x
}

// statusExtraFields renders the cached sample; absent values are null with the reason in errors.
func (d *Daemon) statusExtraFields() map[string]any {
	d.refreshStatusExtras()
	m := map[string]any{"leases": nil, "panes": nil, "heavy_ops": nil, "provider_pools": nil, "dispatch_pauses": nil, "host": nil, "provider_health": nil, "extras_age_ms": nil, "ui": d.uiStatus(), "tui": d.tuiStatus()}
	m["session_isolation"] = d.currentIsolation()
	x := d.statusExtras.value.Load()
	if x == nil {
		m["extras_errors"] = map[string]string{"*": "first sample pending"}
		return m
	}
	m["workflow_limit"] = x.WorkflowLimit
	m["leases"], m["panes"], m["heavy_ops"], m["provider_pools"], m["dispatch_pauses"], m["host"], m["provider_health"] = x.Leases, x.Panes, x.HeavyOps, x.ProviderPools, x.DispatchPauses, x.Host, x.ProviderHealth
	m["extras_age_ms"] = max(0, time.Since(x.At).Milliseconds())
	if len(x.Errors) > 0 {
		m["extras_errors"] = x.Errors
	}
	return m
}

// Endpoints names the private plane endpoint and the instance RPC socket.
func (d *Daemon) Endpoints() (plane, socket string) {
	d.childMu.RLock()
	defer d.childMu.RUnlock()
	return fmt.Sprintf("127.0.0.1:%d", d.child.Port()), d.layout.Socket
}

var buildOnce sync.Once
var buildVersion map[string]any

// BuildVersion reports the binary's module version and VCS build stamp.
func BuildVersion() map[string]any {
	buildOnce.Do(func() {
		buildVersion = map[string]any{"version": "development", "commit": "unknown", "modified": nil, "go": nil}
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		buildVersion["go"] = info.GoVersion
		if v := info.Main.Version; v != "" && v != "(devel)" {
			buildVersion["version"] = v
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				buildVersion["commit"] = s.Value
			case "vcs.modified":
				buildVersion["modified"] = s.Value == "true"
			case "vcs.time":
				buildVersion["commit_time"] = s.Value
			}
		}
	})
	out := map[string]any{}
	for k, v := range buildVersion {
		out[k] = v
	}
	return out
}
