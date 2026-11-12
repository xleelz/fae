package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

type AnalyzeParams struct {
	Task string `json:"task"`
}

// SessionMetric states its source and whether it is exact or an estimate.
type SessionMetric struct {
	Name   string  `json:"name"`
	Stage  string  `json:"stage,omitempty"`
	Role   string  `json:"role,omitempty"`
	Class  string  `json:"class,omitempty"`
	Value  float64 `json:"value"`
	Count  int     `json:"count,omitempty"`
	Label  string  `json:"label"`
	Source string  `json:"source"`
}
type SessionGap struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}
type SessionAnalysis struct {
	Task     string          `json:"task"`
	State    string          `json:"state"`
	Terminal bool            `json:"terminal"`
	Metrics  []SessionMetric `json:"metrics"`
	Gaps     []SessionGap    `json:"gaps"`
	Spend    string          `json:"spend"`
}

const (
	stageSource  = "plane events.created_at: commit time of the transition entering the stage to the one leaving it"
	turnSource   = "task provenance: distinct dispatch ids per role"
	durSource    = "provenance produced_at (executor-reported) minus the plane commit time of the dispatch event"
	eventsSource = "plane task events by cause"
	// A task without waits has no lease-wait events and so no lease metric.
	leaseWaitSource = "plane lease-wait task events: wait_seconds measured by the daemon when each wait ended"
)

// analyzeSession is read-only and plane-only. It never infers spend and
// reports an unreachable source as a gap rather than a number.
func (d *Daemon) analyzeSession(ctx context.Context, raw json.RawMessage) (any, error) {
	var p AnalyzeParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Task == "" {
		return nil, errors.New("analyze-session requires a task")
	}
	s, err := d.store.GetJob(ctx, p.Task)
	if err != nil {
		return nil, errors.New("unknown task " + p.Task)
	}
	var env envelope
	if err = decodeEnvelope(s.Payload, &env); err != nil {
		return nil, err
	}
	events := []controlplane.Event{}
	for after := int64(0); ; {
		page, err := d.store.JobEvents(ctx, p.Task, after, 10000)
		if err != nil {
			return nil, err
		}
		events = append(events, page...)
		if len(page) < 10000 {
			break
		}
		after = page[len(page)-1].Sequence
	}
	a := SessionAnalysis{Task: p.Task, State: s.State, Terminal: domain.State(s.State).Terminal(), Metrics: []SessionMetric{}, Gaps: []SessionGap{}, Spend: "not inferred"}
	parsed := make([]engine.Event, len(events))
	for i, ev := range events {
		_ = json.Unmarshal(ev.Payload, &parsed[i])
	}
	if len(events) == 0 || events[0].Kind != "task_admitted" {
		a.Gaps = append(a.Gaps, SessionGap{"stage_wall_seconds", "the task's admission event is no longer on the plane (event retention); stage intervals are unknown"})
	} else {
		wall, visits := map[string]float64{}, map[string]int{}
		stage, since := string(domain.Pending), events[0].Timestamp
		visits[stage] = 1
		for i, ev := range events[1:] {
			x := parsed[i+1]
			if ev.Kind == "transition" && string(x.From) == stage {
				wall[stage] += ev.Timestamp.Sub(since).Seconds()
				stage, since = string(x.To), ev.Timestamp
				visits[stage]++
			}
		}
		for _, name := range analysisKeys(wall) {
			a.Metrics = append(a.Metrics, SessionMetric{Name: "stage_wall_seconds", Stage: name, Value: wall[name], Count: visits[name], Label: "exact", Source: stageSource})
		}
		if !a.Terminal {
			a.Gaps = append(a.Gaps, SessionGap{"stage_wall_seconds:" + stage, "the current stage interval is still open"})
		}
	}
	dispatchAt := map[string]time.Time{}
	for i, ev := range events {
		if ev.Kind == "dispatch" && parsed[i].DispatchID != "" {
			dispatchAt[parsed[i].DispatchID] = ev.Timestamp
		}
	}
	turns, seconds, timed := map[string]map[string]bool{}, map[string]float64{}, map[string]int{}
	for _, prov := range env.Job.Provenance {
		if prov.Role == "" || prov.DispatchID == "" {
			continue
		}
		if turns[prov.Role] == nil {
			turns[prov.Role] = map[string]bool{}
		}
		if turns[prov.Role][prov.DispatchID] {
			continue
		}
		turns[prov.Role][prov.DispatchID] = true
		if at, ok := dispatchAt[prov.DispatchID]; ok && !prov.ProducedAt.IsZero() {
			seconds[prov.Role] += prov.ProducedAt.Sub(at).Seconds()
			timed[prov.Role]++
		}
	}
	for _, role := range analysisKeys(turns) {
		a.Metrics = append(a.Metrics, SessionMetric{Name: "turns", Role: role, Value: float64(len(turns[role])), Label: "exact", Source: turnSource})
		if timed[role] > 0 {
			a.Metrics = append(a.Metrics, SessionMetric{Name: "turn_seconds", Role: role, Value: seconds[role], Count: timed[role], Label: "estimate", Source: durSource})
		}
		if timed[role] < len(turns[role]) {
			a.Gaps = append(a.Gaps, SessionGap{"turn_seconds:" + role, "some dispatch events are no longer on the plane"})
		}
	}
	counts := map[string]map[string]int{"retries": {}, "operator_interventions": {}, "parks": {}}
	for i, ev := range events {
		cause := parsed[i].Cause
		switch ev.Kind {
		case "failure":
			counts["retries"][cause]++
		case "park":
			counts["parks"][cause]++
		case "task-control":
			counts["operator_interventions"][strings.SplitN(cause, ":", 2)[0]]++
		case "design-doc-attached", "migrated":
			counts["operator_interventions"][ev.Kind]++
		}
	}
	for _, name := range []string{"retries", "operator_interventions", "parks"} {
		for _, class := range analysisKeys(counts[name]) {
			a.Metrics = append(a.Metrics, SessionMetric{Name: name, Class: class, Value: float64(counts[name][class]), Label: "exact", Source: eventsSource})
		}
	}
	// Lease waits are lease-wait task events whose wait_seconds the daemon
	// measured when the wait ended (FAE-OBS-003).
	waits, waitCounts, refused := map[string]float64{}, map[string]int{}, map[string]int{}
	for _, ev := range events {
		if ev.Kind != leaseWaitEventKind {
			continue
		}
		var w struct {
			Lease   string  `json:"lease"`
			Seconds float64 `json:"wait_seconds"`
			Outcome string  `json:"outcome"`
		}
		if json.Unmarshal(ev.Payload, &w) != nil || w.Lease == "" {
			continue
		}
		waits[w.Lease] += w.Seconds
		waitCounts[w.Lease]++
		if w.Outcome != "granted" {
			refused[w.Lease]++
		}
	}
	if len(events) == 0 || events[0].Kind != "task_admitted" {
		a.Gaps = append(a.Gaps, SessionGap{"lease_wait_seconds", "the task's earliest events are no longer on the plane (event retention); lease waits may be incomplete"})
	}
	for _, lease := range analysisKeys(waits) {
		a.Metrics = append(a.Metrics, SessionMetric{Name: "lease_wait_seconds", Class: lease, Value: waits[lease], Count: waitCounts[lease], Label: "exact", Source: leaseWaitSource})
		if refused[lease] > 0 {
			a.Metrics = append(a.Metrics, SessionMetric{Name: "lease_waits_refused", Class: lease, Value: float64(refused[lease]), Label: "exact", Source: leaseWaitSource})
		}
	}
	a.Gaps = append(a.Gaps, SessionGap{"tokens", "provider session logs are not retained on the plane; token counts are unavailable"})
	return a, nil
}

func analysisKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
