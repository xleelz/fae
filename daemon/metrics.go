package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

const (
	metricsWindow      = 24 * time.Hour
	metricsMaxWindow   = 7 * 24 * time.Hour
	metricsEventLimit  = 20000
	leaseWaitEventKind = "lease-wait"
)

// Durations summarises seconds with the nearest-rank median and 90th percentile.
type Durations struct {
	Count  int     `json:"count"`
	Median float64 `json:"median_seconds"`
	P90    float64 `json:"p90_seconds"`
	Max    float64 `json:"max_seconds"`
}

func summarise(values []float64) Durations {
	if len(values) == 0 {
		return Durations{}
	}
	sort.Float64s(values)
	rank := func(q float64) float64 { return values[max(0, int(math.Ceil(q*float64(len(values))))-1)] }
	return Durations{Count: len(values), Median: rank(0.5), P90: rank(0.9), Max: values[len(values)-1]}
}

type GateMetrics struct {
	Evaluated int            `json:"evaluated"`
	Rejected  int            `json:"rejected"`
	Rate      float64        `json:"rejection_rate"`
	Causes    map[string]int `json:"causes"`
}

// EventMetrics are the FAE-OBS-004 figures derived from the plane event log
// over a window; timings come from event commit times.
type EventMetrics struct {
	WindowSeconds      float64                    `json:"window_seconds"`
	Events             int                        `json:"events"`
	Truncated          bool                       `json:"truncated"`
	TransitionsPerHour float64                    `json:"transitions_per_hour"`
	StageWall          map[string]Durations       `json:"stage_wall_time"`
	AdmissionToRelease Durations                  `json:"admission_to_release_wait"`
	ReleaseToDone      Durations                  `json:"release_to_done_time"`
	Gates              map[string]GateMetrics     `json:"gate_rejections"`
	AttemptsPerTask    Durations                  `json:"attempts_per_task"`
	ParksByReason      map[string]int             `json:"parks_by_reason"`
	LeaseWait          map[string]Durations       `json:"lease_wait"`
	ExternalEffects    map[string]int             `json:"external_effects_by_outcome"`
	Alerts             map[string]int             `json:"alerts_by_source"`
	RoleTime           map[string]RoleTimeMetrics `json:"role_time"`
	RepeatGrantShare   float64                    `json:"repeat_grant_share"`
	RoleGrants         RoleTimeMetrics            `json:"role_grants"`
	TimeLostToRestarts RestartMetrics             `json:"time_lost_to_restarts"`
}

// eventMetrics is a pure function of the ordered event log, so each figure is
// testable against known timings.
func eventMetrics(events []controlplane.Event, window time.Duration) EventMetrics {
	m := EventMetrics{WindowSeconds: window.Seconds(), Events: len(events), StageWall: map[string]Durations{}, Gates: map[string]GateMetrics{}, ParksByReason: map[string]int{}, LeaseWait: map[string]Durations{}, ExternalEffects: map[string]int{}, Alerts: map[string]int{}}
	type task struct {
		admitted, released time.Time
		entered            map[string]time.Time
		attempts           int
	}
	tasks := map[string]*task{}
	stages := map[string][]float64{}
	waits := map[string][]float64{}
	var toRelease, toDone []float64
	transitions := 0
	for _, ev := range events {
		if strings.HasPrefix(ev.Kind, "external-effect-") {
			m.ExternalEffects[strings.TrimPrefix(ev.Kind, "external-effect-")]++
		}
		if ev.Kind == "alert-raised" {
			var a struct {
				Source string `json:"source"`
			}
			_ = json.Unmarshal(ev.Payload, &a)
			m.Alerts[a.Source]++
		}
		if ev.JobID == "" {
			continue
		}
		t := tasks[ev.JobID]
		if t == nil {
			t = &task{admitted: ev.Timestamp, entered: map[string]time.Time{}}
			tasks[ev.JobID] = t
		}
		if ev.Kind == leaseWaitEventKind {
			var w struct {
				Lease string  `json:"lease"`
				Wait  float64 `json:"wait_seconds"`
			}
			if json.Unmarshal(ev.Payload, &w) == nil {
				waits[w.Lease] = append(waits[w.Lease], w.Wait)
			}
			continue
		}
		var e engine.Event
		if json.Unmarshal(ev.Payload, &e) != nil {
			continue
		}
		t.attempts = max(t.attempts, e.Attempt)
		switch ev.Kind {
		case "transition", "retired":
			transitions++
			if since, ok := t.entered[string(e.From)]; ok && e.From != "" {
				stages[string(e.From)] = append(stages[string(e.From)], ev.Timestamp.Sub(since).Seconds())
			}
			if e.To != "" {
				t.entered[string(e.To)] = ev.Timestamp
			}
			if e.From == "Pending" && e.Cause == "admitted" && t.released.IsZero() {
				t.released = ev.Timestamp
				toRelease = append(toRelease, ev.Timestamp.Sub(t.admitted).Seconds())
			}
			if e.To == "Done" && !t.released.IsZero() {
				toDone = append(toDone, ev.Timestamp.Sub(t.released).Seconds())
			}
		case "gate":
			if e.Gate == nil {
				continue
			}
			g := m.Gates[e.Stage]
			if g.Causes == nil {
				g.Causes = map[string]int{}
			}
			g.Evaluated++
			if !e.Gate.Passed {
				g.Rejected++
				g.Causes[string(e.Gate.Value)]++
			}
			g.Rate = float64(g.Rejected) / float64(g.Evaluated)
			m.Gates[e.Stage] = g
		case "park":
			reason, _, _ := strings.Cut(e.Cause, ":")
			m.ParksByReason[reason]++
		}
	}
	if window > 0 {
		m.TransitionsPerHour = float64(transitions) / window.Hours()
	}
	for stage, values := range stages {
		m.StageWall[stage] = summarise(values)
	}
	for lease, values := range waits {
		m.LeaseWait[lease] = summarise(values)
	}
	var attempts []float64
	for _, t := range tasks {
		if t.attempts > 0 {
			attempts = append(attempts, float64(t.attempts))
		}
	}
	m.AttemptsPerTask = summarise(attempts)
	m.AdmissionToRelease, m.ReleaseToDone = summarise(toRelease), summarise(toDone)
	m.RoleTime, m.RoleGrants, m.TimeLostToRestarts = roleAndRestartMetrics(events)
	m.RepeatGrantShare = m.RoleGrants.Share
	return m
}

type MetricsParams struct {
	WindowSeconds int64 `json:"window_seconds"`
}

// metricsRead is the FAE-OBS-004 Metrics RPC. Event-window truncation and
// unmatched timing boundaries remain explicit; missing time is never estimated.
func (d *Daemon) metricsRead(ctx context.Context, raw json.RawMessage) (any, error) {
	var p MetricsParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	window := metricsWindow
	if p.WindowSeconds != 0 {
		window = time.Duration(p.WindowSeconds) * time.Second
	}
	if window <= 0 || window > metricsMaxWindow {
		return nil, errors.New("metrics window must be 1 second to 7 days")
	}
	now := time.Now().UTC()
	events, truncated, err := d.store.EventsSince(ctx, now.Add(-window), metricsEventLimit)
	if err != nil {
		return nil, err
	}
	m := eventMetrics(events, window)
	m.Truncated = truncated
	out := map[string]any{"at": now, "events": m}
	if d.workflowSlots != nil {
		if view, e := d.workflowSlotSnapshot(ctx); e == nil {
			out["workflow_limit"] = view
		} else {
			out["workflow_limit_error"] = e.Error()
		}
	}
	snapshot, _ := d.status.Load().(statusSnapshot)
	out["tasks_by_state"] = snapshot.Counts
	// FAE-SCH-010: the drive's withheld counts for every reason, from the
	// latest round and cumulatively, as status reports them.
	byReason, totals, round := map[string]int{}, map[string]uint64{}, uint64(0)
	if w := d.withheld.Load(); w != nil {
		byReason, totals, round = w.Counts, w.Totals, w.Round
	}
	out["withheld_by_reason"], out["withheld_totals"], out["withheld_round"] = byReason, totals, round
	if edges, err := d.withheldEdges(ctx); err == nil {
		out["withheld_edges_by_prerequisite"] = edges
	} else {
		out["withheld_edges_by_prerequisite_error"] = err.Error()
	}
	pools, err := d.store.LeasePools(ctx)
	if err != nil {
		return nil, err
	}
	utilisation := []map[string]any{}
	for _, pool := range pools {
		held := 0
		for _, h := range pool.Pool.Holders {
			held += h.Weight
		}
		row := map[string]any{"key": pool.Key, "kind": pool.Kind, "capacity": pool.Pool.Capacity, "held_weight": held, "waiting": len(pool.Pool.Waiting)}
		if pool.Pool.Capacity > 0 {
			row["utilisation"] = float64(held) / float64(pool.Pool.Capacity)
		}
		utilisation = append(utilisation, row)
	}
	out["lease_utilisation"] = utilisation
	if d.live != nil && d.live.guard != nil && d.live.guard.host != nil {
		if pool, err := d.live.guard.host.Snapshot(ctx); err == nil {
			waiting := []float64{}
			for _, w := range pool.Waiting {
				waiting = append(waiting, now.Sub(w.Queued).Seconds())
			}
			held := 0
			for _, h := range pool.Holders {
				held += h.Weight
			}
			out["heavy_op"] = map[string]any{"capacity": pool.Capacity, "held_weight": held, "queue_wait_now": summarise(waiting)}
		}
	}
	samples := d.hostSamples()
	if len(samples) > 0 {
		out["host"] = samples[len(samples)-1]
	}
	health, err := d.store.ProviderHealth(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]map[string]any, 0, len(health))
	for _, h := range health {
		rows = append(rows, map[string]any{"health": h, "effective_score": h.At(now), "eligible": h.Eligible(now)})
	}
	out["provider_health"] = rows
	out["processes"] = d.processReport()
	out["repeat_grant_share"] = m.RepeatGrantShare
	out["role_time"] = m.RoleTime
	out["time_lost_to_restarts"] = m.TimeLostToRestarts
	out["unavailable"] = map[string]string{}
	return out, nil
}

// withheldEdges counts Pending tasks held by an unsatisfied blocking edge, by
// the prerequisite's state (missing when it has no job).
func (d *Daemon) withheldEdges(ctx context.Context) (map[string]int, error) {
	graph, err := d.store.ActiveBacklog(ctx)
	if err != nil {
		return nil, err
	}
	pending := map[string]bool{}
	for _, e := range graph.Edges {
		pending[e.To] = e.DependentState == "Pending"
	}
	out := map[string]int{}
	for id, reasons := range blockedDependencies(graph.Edges) {
		if !pending[id] {
			continue
		}
		for _, r := range reasons {
			_, state, _ := strings.Cut(r, ": ")
			out["prerequisite "+state]++
		}
	}
	return out, nil
}

type HostMetricsPage struct {
	ResolutionSeconds float64      `json:"resolution_seconds"`
	WindowSeconds     float64      `json:"window_seconds"`
	Samples           []HostSample `json:"samples"`
}

func (d *Daemon) hostMetricsRead(raw json.RawMessage) (any, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return nil, err
	}
	return HostMetricsPage{ResolutionSeconds: hostSampleInterval.Seconds(), WindowSeconds: hostSampleWindow.Seconds(), Samples: d.hostSamples()}, nil
}

func (d *Daemon) observabilityRead(ctx context.Context, r api.Request) (any, error) {
	switch r.Method {
	case "Alerts":
		return d.alertsRead(ctx, r.Params)
	case "Metrics":
		return d.metricsRead(ctx, r.Params)
	}
	return d.hostMetricsRead(r.Params)
}
