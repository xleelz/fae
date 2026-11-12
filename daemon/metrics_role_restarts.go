package daemon

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// RoleTimeMetrics measures completed durable role-lease grants. A repeat is another
// dispatch of the same task, role and stage, including a durable stage visit >1.
// Parallel reviewers of the same role in one dispatch are one grant wave, not
// repeats. Unmatched session boundaries are reported rather than estimated.
type RoleTimeMetrics struct {
	Seconds       float64 `json:"role_time_seconds"`
	RepeatSeconds float64 `json:"repeat_role_time_seconds"`
	Share         float64 `json:"repeat_grant_share"`
	Completed     int     `json:"completed_grants"`
	Unmatched     int     `json:"unmatched_grants"`
}

type RestartMetrics struct {
	Seconds         float64   `json:"seconds"`
	Daemon          Durations `json:"daemon"`
	Dolt            Durations `json:"dolt"`
	UnknownDowntime int       `json:"unknown_crash_downtime"`
	Incomplete      int       `json:"incomplete_intervals"`
}

type metricInterval struct{ start, end time.Time }

// roleAndRestartMetrics uses only committed events, so reads and daemon restarts
// cannot reset counters. Restart time is the union of measured outage intervals:
// shutdown-to-ready for clean daemon rolls, startup-to-ready after crashes
// (unknown pre-start crash downtime is counted separately), and detected-to-
// recovered for Dolt. Cold starts are not restarts.
func roleAndRestartMetrics(events []controlplane.Event) (map[string]RoleTimeMetrics, RoleTimeMetrics, RestartMetrics) {
	type session struct {
		Role, Stage, Dispatch, Task string
		Repeat                      bool
	}
	sessions := map[string]session{}
	type grant struct {
		session string
		at      time.Time
		repeat  bool
	}
	grants := map[string]grant{}
	firstDispatch := map[string]string{}
	byRole := map[string]RoleTimeMetrics{}
	total := RoleTimeMetrics{}
	restarts := RestartMetrics{}
	var daemonStarts, doltStarts []float64
	var intervals []metricInterval
	var shutdown, start time.Time
	var previousReady, restarting bool
	closeInterval := func(a, b time.Time) float64 {
		if a.IsZero() || b.Before(a) {
			return -1
		}
		intervals = append(intervals, metricInterval{a, b})
		return b.Sub(a).Seconds()
	}
	for _, ev := range events {
		switch ev.Kind {
		case "session-launched":
			var p struct {
				Session  string `json:"session"`
				LeaseID  string `json:"lease_id"`
				Role     string `json:"role"`
				Stage    string `json:"stage"`
				Dispatch string `json:"dispatch_id"`
				Visit    int    `json:"visit"`
			}
			if json.Unmarshal(ev.Payload, &p) != nil || p.Session == "" || p.Role == "" {
				continue
			}
			leaseID := p.LeaseID
			if leaseID == "" {
				leaseID = p.Session
			}
			if _, exists := sessions[leaseID]; exists {
				continue
			}
			sessions[leaseID] = session{Role: p.Role, Stage: p.Stage, Dispatch: p.Dispatch, Task: ev.JobID, Repeat: p.Visit > 1}

		case "lease_granted", "lease_released", "lease_Reclaimed", "lease_recovered":
			var p struct {
				Pool  string `json:"pool"`
				Value struct {
					ID        string `json:"id"`
					RequestID string
				} `json:"value"`
			}
			if json.Unmarshal(ev.Payload, &p) != nil || !strings.HasPrefix(p.Pool, "role/") {
				continue
			}
			id := p.Value.ID
			if id == "" {
				id = p.Value.RequestID
			}
			if id == "" {
				continue
			}
			key := p.Pool + "\x00" + id
			if ev.Kind == "lease_granted" {
				if _, ok := grants[key]; !ok {
					metadata := sessions[id]
					wave := metadata.Task + "\x00" + metadata.Role + "\x00" + metadata.Stage
					first, seen := firstDispatch[wave]
					repeat := metadata.Repeat || seen && first != metadata.Dispatch
					if metadata.Role != "" && !seen {
						firstDispatch[wave] = metadata.Dispatch
					}
					grants[key] = grant{session: id, at: ev.Timestamp, repeat: repeat}
				}
				continue
			}
			g, ok := grants[key]
			metadata, known := sessions[id]
			role := metadata.Role
			if role == "" {
				role = "unknown"
			}
			row := byRole[role]
			if ok && known && !ev.Timestamp.Before(g.at) {
				seconds := ev.Timestamp.Sub(g.at).Seconds()
				row.Seconds += seconds
				row.Completed++
				if g.repeat {
					row.RepeatSeconds += seconds
				}
			} else {
				row.Unmatched++
			}
			delete(grants, key)
			byRole[role] = row

		case "daemon-shutting-down":
			if shutdown.IsZero() {
				shutdown = ev.Timestamp
			}
		case "daemon-starting":
			var p struct {
				Started time.Time `json:"started_at"`
				Restart bool      `json:"restart"`
			}
			_ = json.Unmarshal(ev.Payload, &p)
			start = ev.Timestamp
			if !p.Started.IsZero() && !p.Started.After(ev.Timestamp) {
				start = p.Started
			}
			restarting = p.Restart || previousReady || !shutdown.IsZero()
		case "daemon-ready":
			if restarting {
				from := shutdown
				if from.IsZero() {
					from = start
					restarts.UnknownDowntime++
				}
				if n := closeInterval(from, ev.Timestamp); n >= 0 {
					daemonStarts = append(daemonStarts, n)
				} else {
					restarts.Incomplete++
				}
			}
			previousReady = true
			restarting = false
			shutdown = time.Time{}
			start = time.Time{}
		case "dolt-restarted":
			var p struct {
				Detected  time.Time `json:"detected_at"`
				Recovered time.Time `json:"recovered_at"`
			}
			if json.Unmarshal(ev.Payload, &p) != nil || p.Detected.IsZero() {
				restarts.Incomplete++
				continue
			}
			end := p.Recovered
			if end.IsZero() {
				end = ev.Timestamp
			}
			if n := closeInterval(p.Detected, end); n >= 0 {
				doltStarts = append(doltStarts, n)
			} else {
				restarts.Incomplete++
			}
		}
	}
	for _, g := range grants {
		role := sessions[g.session].Role
		if role == "" {
			role = "unknown"
		}
		row := byRole[role]
		row.Unmatched++
		byRole[role] = row
	}

	for role, row := range byRole {
		if row.Seconds > 0 {
			row.Share = row.RepeatSeconds / row.Seconds
		}
		byRole[role] = row
		total.Seconds += row.Seconds
		total.RepeatSeconds += row.RepeatSeconds
		total.Completed += row.Completed
		total.Unmatched += row.Unmatched
	}
	if total.Seconds > 0 {
		total.Share = total.RepeatSeconds / total.Seconds
	}
	if restarting || !shutdown.IsZero() {
		restarts.Incomplete++
	}
	restarts.Daemon = summarise(daemonStarts)
	restarts.Dolt = summarise(doltStarts)
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start.Before(intervals[j].start) })
	var merged metricInterval
	for _, iv := range intervals {
		if merged.start.IsZero() {
			merged = iv
			continue
		}
		if !iv.start.After(merged.end) {
			if iv.end.After(merged.end) {
				merged.end = iv.end
			}
		} else {
			restarts.Seconds += merged.end.Sub(merged.start).Seconds()
			merged = iv
		}
	}
	if !merged.start.IsZero() {
		restarts.Seconds += merged.end.Sub(merged.start).Seconds()
	}
	return byRole, total, restarts
}
