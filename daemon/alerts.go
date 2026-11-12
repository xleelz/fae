package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// raiseAlert is the daemon's single alert path (FAE-OBS-005): one plane row,
// one alert-raised event, one supervision line and the alerts file mirror. A
// deduplicated repeat of an episode writes nothing. Code already inside a
// Mutate body uses controlplane.Store.InsertAlert instead.
func (d *Daemon) raiseAlert(ctx context.Context, a controlplane.Alert, dedupe bool) (controlplane.Alert, bool, error) {
	d.obs.raise.Lock()
	defer d.obs.raise.Unlock()
	row, raised, err := controlplane.Alert{}, false, errors.New("control plane unavailable")
	if d.store != nil { // admission fixtures run without a plane
		row, raised, err = d.store.RaiseAlert(ctx, controlplane.Audit{Actor: "daemon", Action: "alert-raised", Outcome: "alert"}, a, dedupe)
	}
	if err != nil {
		d.logLine("alerts", "alert not recorded source=%s task=%q error=%q\n", a.Source, a.Task, err.Error())
		return row, false, err
	}
	if raised {
		d.logLine("alerts", "alert raised id=%d severity=%s source=%s task=%q detail=%s\n", row.ID, row.Severity, row.Source, a.Task, row.Detail)
		d.notifyEvents()
		_ = d.mirrorAlerts(ctx)
	}
	return row, raised, nil
}

// alertMirror appends the plane alerts log to <root>/alerts.log as JSON lines:
// one "raised" line per alert and one "acknowledged" line per acknowledgement.
// The plane is the authority; a crash between commit and append is repaired
// by the next sync, which resumes from the file's own contents.
type alertMirror struct {
	mu       sync.Mutex
	loaded   bool
	raised   int64
	ackSince time.Time
	acked    map[int64]bool
}
type alertFileLine struct {
	Event          string          `json:"event"`
	ID             int64           `json:"id"`
	Severity       string          `json:"severity,omitempty"`
	Source         string          `json:"source,omitempty"`
	CreatedAt      *time.Time      `json:"created_at,omitempty"`
	AcknowledgedAt *time.Time      `json:"acknowledged_at,omitempty"`
	Detail         json.RawMessage `json:"detail,omitempty"`
}

func (d *Daemon) alertsFile() string { return filepath.Join(d.layout.Root, alertsFileName) }
func (m *alertMirror) load(path string) (torn bool, err error) {
	m.acked = map[int64]bool{}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		var l alertFileLine
		if json.Unmarshal(line, &l) != nil {
			continue // a torn final line from an interrupted append
		}
		switch l.Event {
		case "raised":
			m.raised = max(m.raised, l.ID)
		case "acknowledged":
			m.acked[l.ID] = true
			if l.AcknowledgedAt != nil && l.AcknowledgedAt.After(m.ackSince) {
				m.ackSince = *l.AcknowledgedAt
			}
		}
	}
	return len(raw) > 0 && raw[len(raw)-1] != '\n', nil
}
func (d *Daemon) mirrorAlerts(ctx context.Context) error {
	m := &d.obs.mirror
	m.mu.Lock()
	defer m.mu.Unlock()
	torn := false
	if !m.loaded {
		var err error
		if torn, err = m.load(d.alertsFile()); err != nil {
			return err
		}
		m.loaded = true
	}
	since := m.ackSince
	if !since.IsZero() {
		since = since.Add(-time.Second) // equal timestamps; the acked set filters repeats
	}
	rows, err := d.store.AlertsChanged(ctx, m.raised, since, 500)
	if err != nil || (len(rows) == 0 && !torn) {
		return err
	}
	var buf bytes.Buffer
	if torn {
		buf.WriteByte('\n')
	}
	raised, ackSince, acked := m.raised, m.ackSince, []int64{}
	for _, a := range rows {
		if a.ID > m.raised {
			created := a.CreatedAt
			line, _ := json.Marshal(alertFileLine{Event: "raised", ID: a.ID, Severity: a.Severity, Source: a.Source, CreatedAt: &created, Detail: a.Detail})
			buf.Write(append(line, '\n'))
			raised = max(raised, a.ID)
		}
		if a.AcknowledgedAt != nil && !m.acked[a.ID] {
			line, _ := json.Marshal(alertFileLine{Event: "acknowledged", ID: a.ID, AcknowledgedAt: a.AcknowledgedAt})
			buf.Write(append(line, '\n'))
			acked = append(acked, a.ID)
			if a.AcknowledgedAt.After(ackSince) {
				ackSince = *a.AcknowledgedAt
			}
		}
	}
	if buf.Len() == 0 {
		return nil
	}
	f, err := os.OpenFile(d.alertsFile(), os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		m.loaded = false // reread the file before the next append
		return err
	}
	m.raised, m.ackSince = raised, ackSince
	for _, id := range acked {
		m.acked[id] = true
	}
	return nil
}

type AlertsParams struct {
	After          int64 `json:"after"`
	Limit          int   `json:"limit"`
	Unacknowledged bool  `json:"unacknowledged"`
}
type AlertsPage struct {
	Alerts     []controlplane.Alert `json:"alerts"`
	NextCursor int64                `json:"next_cursor"`
	File       string               `json:"file"`
}
type AlertAcknowledgeParams struct {
	ID                 int64  `json:"id"`
	AuthorizingMessage string `json:"authorizing_message"`
}

func (d *Daemon) alertsRead(ctx context.Context, raw json.RawMessage) (any, error) {
	var p AlertsParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Limit == 0 {
		p.Limit = 100
	}
	rows, err := d.store.Alerts(ctx, p.After, p.Limit, p.Unacknowledged)
	if err != nil {
		return nil, err
	}
	page := AlertsPage{Alerts: rows, File: d.alertsFile()}
	if len(rows) == p.Limit {
		page.NextCursor = rows[len(rows)-1].ID
	}
	return page, nil
}

// alertAcknowledge is the audited acknowledgement verb (FAE-OBS-005).
func (d *Daemon) alertAcknowledge(ctx context.Context, raw json.RawMessage, a controlplane.Audit) (any, error) {
	var p AlertAcknowledgeParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.ID <= 0 {
		return nil, errors.New("alert acknowledge requires --id")
	}
	row, err := d.store.AcknowledgeAlert(ctx, a, p.ID)
	if err != nil {
		return nil, err
	}
	d.logLine("alerts", "alert acknowledged id=%d actor=%s message=%q\n", row.ID, a.Actor, a.MessageID)
	d.notifyEvents()
	_ = d.mirrorAlerts(ctx)
	return row, nil
}

// detectDeadEdges alerts once per dead blocking edge episode: a prerequisite
// that is missing, or terminal or Escalated without satisfying the edge
// (FAE-SCH-002). The prerequisite's job version names the episode.
func (d *Daemon) detectDeadEdges(ctx context.Context) error {
	// The plane's dependency rows carry each prerequisite's state: no status
	// snapshot lag and no terminal snapshot to materialise (FAE-CP-009).
	graph, err := d.store.ActiveBacklog(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, e := range graph.Edges {
		if e.Kind != "blocking" {
			continue
		}
		from := e.Prerequisite
		if e.Present && (from.State == "Done" || !backlogTerminal(from.State) && from.State != "Escalated" || from.State == "Retired" && headSatisfied(from)) {
			continue
		}
		outcome, version := "missing", int64(0)
		if e.Present {
			outcome, version = from.State, e.Version
		}
		detail, _ := json.Marshal(map[string]any{"from": e.From, "to": e.To, "prerequisite_outcome": outcome, "prerequisite_version": version})
		if _, _, err := d.raiseAlert(ctx, controlplane.Alert{Severity: "warning", Source: "dead-edge", Task: e.To, Detail: detail}, true); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return firstErr
}

// detectRouteExhaustion alerts once per route-exhaustion episode
// (FAE-OBS-005): a task whose role has had no eligible route for the
// engine's route-exhaustion bound (Config.RouteExhaustion, default 30 min).
// It follows the engine's clock, engine.Job.RoutesUnavailableSince: the
// first park of the episode, kept across daemon restarts while no route runs,
// and restarted by Resume and by restart with information. The episode
// alerts whether the task is still parked past the bound (a drive that has
// not decided it again) or the engine has escalated it ProviderUnavailable at
// the bound.
func (d *Daemon) detectRouteExhaustion(ctx context.Context) error {
	return d.routeExhaustion(ctx, d.statusTasks(), time.Now().UTC())
}

// routeExhaustionBound is the engine's route-exhaustion bound (drive.go
// passes the same Config.RouteExhaustion to engine.Options).
func (d *Daemon) routeExhaustionBound() time.Duration {
	if d.config.RouteExhaustion > 0 {
		return d.config.RouteExhaustion
	}
	return engine.DefaultRouteExhaustion
}

// routeExhaustionClock is a task's route-exhaustion episode as the observer
// read it at one checkpoint version: when it began, and the stage and reason
// of its latest park. The alert's detail holds only these, so an episode is
// one alert row whether it is raised while the task is parked or after its
// escalation, and across daemon restarts (raiseAlert deduplicates by detail).
type routeExhaustionClock struct {
	version       int64
	since         time.Time
	stage, reason string
	alerted       bool
}

func (d *Daemon) routeExhaustion(ctx context.Context, tasks map[string]BoardItem, now time.Time) error {
	exhausted := map[string]BoardItem{}
	for id, t := range tasks {
		if !t.InFlight && (engine.RoutesUnavailableReason(t.Reason) || t.State == string(domain.Escalated) && t.EscalationKind == domain.ProviderUnavailable) {
			exhausted[id] = t
		}
	}
	jobs := d.publishedDriveJobs()
	d.obs.mu.Lock()
	if d.obs.routeClock == nil {
		d.obs.routeClock = map[string]routeExhaustionClock{}
	}
	for id := range d.obs.routeClock {
		if _, ok := exhausted[id]; !ok {
			delete(d.obs.routeClock, id)
		}
	}
	known := maps.Clone(d.obs.routeClock)
	d.obs.mu.Unlock()
	ids := make([]string, 0, len(exhausted))
	for id := range exhausted {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	bound := d.routeExhaustionBound()
	var firstErr error
	for _, id := range ids {
		clock, ok := known[id]
		if !ok || clock.version != exhausted[id].Version {
			if clock, ok = d.readRouteExhaustionClock(ctx, id, exhausted[id], jobs[id], now); !ok {
				continue
			}
			d.obs.mu.Lock()
			d.obs.routeClock[id] = clock
			d.obs.mu.Unlock()
		}
		if clock.alerted || clock.since.IsZero() || now.Sub(clock.since) < bound {
			continue
		}
		detail, _ := json.Marshal(map[string]any{"task": id, "stage": clock.stage, "since": clock.since.Format(time.RFC3339Nano), "bound_seconds": bound.Seconds(), "reason": clock.reason})
		if _, _, err := d.raiseAlert(ctx, controlplane.Alert{Source: "route-exhaustion", Task: id, Detail: detail}, true); err != nil {
			firstErr = errors.Join(firstErr, err)
			continue
		}
		clock.alerted = true
		d.obs.mu.Lock()
		d.obs.routeClock[id] = clock
		d.obs.mu.Unlock()
	}
	return firstErr
}

// readRouteExhaustionClock reads a task's episode from its published
// checkpoint: the engine's clock, and the latest park for want of a route in
// its history. A checkpoint published at another version is read on a later
// round (ok false). A route park with no published checkpoint (a fixture row)
// is dated by its latest park event, else from first sight; an escalation
// without the engine's clock (one an older daemon recorded) stays undated (a
// zero since) and raises nothing.
func (d *Daemon) readRouteExhaustionClock(ctx context.Context, id string, t BoardItem, s controlplane.JobSnapshot, now time.Time) (routeExhaustionClock, bool) {
	clock := routeExhaustionClock{version: t.Version, stage: t.State, reason: t.Reason}
	escalated := t.State == string(domain.Escalated)
	if s.ID != "" {
		var e envelope
		if s.Version != t.Version || decodeEnvelope(s.Payload, &e) != nil {
			return clock, false
		}
		for i := len(e.Job.History) - 1; i >= 0; i-- {
			if h := e.Job.History[i]; h.Kind == "park" && engine.RoutesUnavailableReason(h.Cause) {
				clock.stage, clock.reason = h.Stage, h.Cause
				break
			}
		}
		if clock.since = e.Job.RoutesUnavailableSince; !clock.since.IsZero() || escalated {
			return clock, true
		}
	}
	if escalated {
		return clock, true
	}
	park, err := d.store.LastJobEvent(ctx, id, "park")
	var e engine.Event
	if err == nil && json.Unmarshal(park.Payload, &e) == nil && engine.RoutesUnavailableReason(e.Cause) {
		clock.since, clock.reason = park.Timestamp, e.Cause
	} else {
		clock.since = now // the episode's start is unproven; count from first sight
	}
	return clock, true
}

// publishedDriveJobs is the drive's checkpoints as last published; callers
// must not mutate it.
func (d *Daemon) publishedDriveJobs() map[string]controlplane.JobSnapshot {
	if s, ok := d.status.Load().(statusSnapshot); ok {
		return s.driveJobs
	}
	return nil
}

// statusTasks reads the published snapshot; it never takes the writer mutex.
func (d *Daemon) statusTasks() map[string]BoardItem {
	out := map[string]BoardItem{}
	if v, ok := d.status.Load().(statusSnapshot); ok {
		for _, t := range v.Tasks {
			out[t.ID] = t
		}
	}
	return out
}
