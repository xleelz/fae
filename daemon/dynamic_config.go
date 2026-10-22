package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/hostguard"
)

// RuntimeConfig is a partial update. Zero integer limits and nil thresholds
// preserve the existing setting; explicit invalid threshold values are rejected.
type RuntimeConfig struct {
	PaneLimit          int               `json:"pane_limit,omitempty"`
	HeavyCapacity      int               `json:"heavy_capacity,omitempty"`
	TasksInFlight      int               `json:"tasks_in_flight,omitempty"`
	HostThresholds     *hostguard.Config `json:"host_thresholds,omitempty"`
	AuthorizingMessage string            `json:"authorizing_message,omitempty"`
}

func (d *Daemon) readRuntimeConfig(ctx context.Context) (RuntimeConfig, error) {
	var c RuntimeConfig
	raw, e := d.store.GetMarker(ctx, "runtime-config")
	if errors.Is(e, sql.ErrNoRows) {
		return c, nil
	}
	if e != nil {
		return c, e
	}
	e = decode(raw, &c)
	if e != nil {
		return c, e
	}
	return c, validateRuntimeConfig(c)
}
func validateRuntimeConfig(c RuntimeConfig) error {
	if c.PaneLimit != 0 && (c.PaneLimit < 2 || c.PaneLimit > 1000000) {
		return errors.New("pane limit must be between 2 and 1000000")
	}
	if c.HeavyCapacity < 0 || c.HeavyCapacity > 1000000 {
		return errors.New("heavy capacity must be between 1 and 1000000")
	}
	if c.TasksInFlight < 0 || c.TasksInFlight > 256 {
		return errors.New("tasks in flight must be between 1 and 256")
	}
	if c.HostThresholds != nil && !hostguard.ValidConfig(*c.HostThresholds) {
		return errors.New("invalid host thresholds")
	}
	return nil
}
func (d *Daemon) runtimePaneLimit(ctx context.Context, fallback int) (int, error) {
	c, e := d.readRuntimeConfig(ctx)
	if e != nil {
		return 0, e
	}
	if c.PaneLimit != 0 {
		return c.PaneLimit, nil
	}
	return fallback, nil
}
func (d *Daemon) refreshRuntimeConfig(ctx context.Context) error {
	if d.live == nil || d.live.guard == nil {
		return nil
	}
	c, e := d.readRuntimeConfig(ctx)
	if e != nil {
		return e
	}
	return d.applyRuntimeConfig(ctx, c)
}
func (d *Daemon) applyRuntimeConfig(ctx context.Context, c RuntimeConfig) error {
	a := d.live.guard
	heavy := d.live.config.HeavyCapacity
	if c.HeavyCapacity != 0 {
		heavy = c.HeavyCapacity
	}
	cfg := hostguard.DefaultConfig()
	if c.HostThresholds != nil {
		cfg = *c.HostThresholds
	}
	if err := a.host.ConfigureHost(ctx, d.layout.Root, heavy, cfg); err != nil {
		return err
	}
	n, h, err := a.host.HostLimits(ctx)
	if err != nil {
		return err
	}
	a.dynamicHeavy.Store(int64(n))
	a.dynamicHost.Store(&h)
	tasks := d.config.MaxInFlight
	if c.TasksInFlight != 0 {
		tasks = c.TasksInFlight
	}
	a.dynamicTasks.Store(int64(tasks))
	if a.panes != nil {
		limit := a.paneLimit
		if c.PaneLimit != 0 {
			limit = c.PaneLimit
		}
		if err = a.panes.Configure(ctx, d.layout.Root, limit); err != nil {
			return err
		}
		effective, e := a.panes.Limit(ctx)
		if e != nil {
			return e
		}
		a.dynamicPaneLimit.Store(int64(effective))
		a.requestedPaneLimit.Store(int64(limit))
	}
	return nil
}
func (d *Daemon) updateRuntimeConfig(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var patch RuntimeConfig
	if err := decode(raw, &patch); err != nil {
		return nil, err
	}
	if err := validateRuntimeConfig(patch); err != nil {
		return nil, err
	}
	if d.live == nil {
		return nil, errors.New("runtime host configuration requires live execution")
	}
	if patch.PaneLimit == 0 && patch.HeavyCapacity == 0 && patch.TasksInFlight == 0 && patch.HostThresholds == nil {
		return nil, errors.New("at least one runtime limit required")
	}
	if patch.PaneLimit != 0 {
		if d.live.guard.panes == nil {
			return nil, errors.New("pane configuration requires Herdr")
		}
		if err := validatePaneFloor(defaults.Workflow(), patch.PaneLimit); err != nil {
			return nil, err
		}
		for _, j := range d.jobs {
			var e envelope
			if err := decodeEnvelope(j.Payload, &e); err != nil {
				return nil, err
			}
			if err := validatePaneFloor(e.Job.Document, patch.PaneLimit); err != nil {
				return nil, fmt.Errorf("task %s: %w", j.ID, err)
			}
		}
	}
	c, err := d.readRuntimeConfig(ctx)
	if err != nil {
		return nil, err
	}
	if patch.PaneLimit != 0 {
		c.PaneLimit = patch.PaneLimit
	}
	if patch.HeavyCapacity != 0 {
		c.HeavyCapacity = patch.HeavyCapacity
	}
	if patch.TasksInFlight != 0 {
		c.TasksInFlight = patch.TasksInFlight
	}
	if patch.HostThresholds != nil {
		c.HostThresholds = patch.HostThresholds
	}
	c.AuthorizingMessage = ""
	payload, _ := json.Marshal(c)
	if err = d.store.PutMarker(ctx, audit, "runtime-config", payload); err != nil {
		return nil, err
	}
	if err = d.applyRuntimeConfig(ctx, c); err != nil {
		return nil, err
	}
	d.publishStatus()
	d.signal()
	return d.dynamicConfiguration(), nil
}
func (d *Daemon) effectiveTasksInFlight() int {
	if d.live != nil && d.live.guard != nil {
		if n := d.live.guard.dynamicTasks.Load(); n > 0 {
			return int(n)
		}
	}
	return d.config.MaxInFlight
}
func (d *Daemon) dynamicConfiguration() map[string]any {
	a := d.live.guard
	c := a.hostConfig()
	m := map[string]any{"heavy_capacity": int(a.dynamicHeavy.Load()), "tasks_in_flight": d.effectiveTasksInFlight(), "host_thresholds": c}
	if a.panes != nil {
		m["effective_pane_limit"] = a.effectivePaneLimit()
		m["pane_limit"] = int(a.requestedPaneLimit.Load())
	}
	return m
}
func (d *Daemon) releasePaneConfiguration(ctx context.Context) error {
	if d.live == nil || d.live.guard == nil {
		return nil
	}
	a := d.live.guard
	var e error
	if a.panes != nil {
		e = a.panes.Unconfigure(ctx, d.layout.Root, a.owner, a.birth)
	}
	return errors.Join(e, a.host.UnconfigureHost(ctx, d.layout.Root, a.owner, a.birth))
}
