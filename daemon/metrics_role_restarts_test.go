package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/scheduler"
	"math"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
)

// FAE-OBS-004: a remediation wave and parallel reviewers must be distinguished,
// and role/restart durations must come from committed identity and timing data.
func TestRoleAndRestartMetricsKnownTimings(t *testing.T) {
	epoch := time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)
	at := func(n int) time.Time { return epoch.Add(time.Duration(n) * time.Second) }
	var events []controlplane.Event
	add := func(kind, task string, n int, p any) {
		raw, err := json.Marshal(p)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, controlplane.Event{Kind: kind, JobID: task, Timestamp: at(n), Payload: raw})
	}
	session := func(kind, id, role, dispatch string, visit, n int) {
		add(kind, "a", n, map[string]any{"session": id, "lease_id": "lease-" + id, "role": role, "stage": "review", "dispatch_id": dispatch, "visit": visit})
		leaseKind := "lease_released"
		if kind == "session-launched" {
			leaseKind = "lease_granted"
		}
		add(leaseKind, "", n, map[string]any{"pool": "role/reviewer", "value": map[string]any{"id": "lease-" + id}})
	}
	add("daemon-starting", "", 0, map[string]any{"started_at": at(-2)})
	add("daemon-ready", "", 2, nil) // cold startup is not restart loss
	session("session-launched", "one", "reviewer", "d1", 1, 10)
	session("session-launched", "two", "reviewer", "d1", 1, 10)
	session("session-reaped", "one", "reviewer", "d1", 1, 20)
	session("session-reaped", "two", "reviewer", "d1", 1, 30)
	session("session-launched", "three", "reviewer", "d2", 2, 30)
	session("session-reaped", "three", "reviewer", "d2", 2, 60)
	// The second wave consumed 30s of a total 60 role-seconds: share 1/2.
	add("daemon-shutting-down", "", 70, nil)
	add("daemon-stopped", "", 72, nil)
	add("daemon-starting", "", 75, map[string]any{"restart": true, "started_at": at(73)})
	add("daemon-ready", "", 80, nil)
	// A nested Dolt outage overlaps the daemon roll; count the union once.
	add("dolt-restarted", "", 81, map[string]any{"detected_at": at(76), "recovered_at": at(81)})
	add("dolt-restarted", "", 90, map[string]any{"detected_at": at(85), "recovered_at": at(90)})
	roles, total, restarts := roleAndRestartMetrics(events)
	if total.Seconds != 60 || total.RepeatSeconds != 30 || total.Share != 0.5 || total.Completed != 3 || total.Unmatched != 0 || roles["reviewer"] != total {
		t.Fatalf("role metrics: %+v %+v", roles, total)
	}
	if restarts.Seconds != 16 || restarts.Daemon != (Durations{Count: 1, Median: 10, P90: 10, Max: 10}) || restarts.Dolt != (Durations{Count: 2, Median: 5, P90: 5, Max: 5}) || restarts.UnknownDowntime != 0 || restarts.Incomplete != 0 {
		t.Fatalf("restart metrics %+v", restarts)
	}
	m := eventMetrics(events, time.Hour)
	if m.RepeatGrantShare != 0.5 || m.TimeLostToRestarts.Seconds != 16 {
		t.Fatalf("metric aggregation %+v", m)
	}
}

func TestRoleAndRestartMetricsWindowAndCrashBoundaries(t *testing.T) {
	epoch := time.Unix(10000, 0)
	ev := func(kind string, n int, p string) controlplane.Event {
		return controlplane.Event{Kind: kind, JobID: "t", Timestamp: epoch.Add(time.Duration(n) * time.Second), Payload: json.RawMessage(p)}
	}
	events := []controlplane.Event{
		ev("session-reaped", 0, `{"session":"old","role":"coder"}`),
		ev("lease_released", 0, `{"pool":"role/coder","value":{"id":"old"}}`),
		ev("session-launched", 1, `{"session":"new","role":"coder","stage":"implement","dispatch_id":"d","visit":2}`),
		ev("lease_granted", 1, `{"pool":"role/coder","value":{"id":"new"}}`),
		ev("session-reaped", 11, `{"session":"new","role":"coder"}`),
		ev("lease_released", 11, `{"pool":"role/coder","value":{"id":"new"}}`),
		ev("session-launched", 12, `{"session":"open","role":"coder","stage":"implement","dispatch_id":"d2"}`),
		ev("lease_granted", 12, `{"pool":"role/coder","value":{"id":"open"}}`),
		ev("daemon-starting", 20, `{"restart":true}`),
		ev("daemon-ready", 25, `{}`),
		ev("dolt-restarted", 26, `{}`), // historical event without timing is unknown
	}
	_, total, restarts := roleAndRestartMetrics(events)
	if total.Completed != 1 || total.Unmatched != 2 || total.Share != 1 || total.Seconds != 10 || math.IsNaN(total.Share) {
		t.Fatalf("boundary role metrics %+v", total)
	}
	if restarts.Seconds != 5 || restarts.UnknownDowntime != 1 || restarts.Incomplete != 1 {
		t.Fatalf("crash boundaries %+v", restarts)
	}
	_, empty, none := roleAndRestartMetrics(nil)
	if empty != (RoleTimeMetrics{}) || none != (RestartMetrics{}) {
		t.Fatalf("empty %+v %+v", empty, none)
	}
}

// Durable events are the sole metric input across process restarts.
func TestRoleRestartMetricsSurviveDaemonRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	record := func(kind string) {
		raw, _ := json.Marshal(map[string]any{"session": "metric-session", "lease_id": "metric-lease", "role": "coder", "stage": "implement", "dispatch_id": "metric-dispatch", "visit": 2})
		if err := d.store.RecordTaskEvent(ctx, controlplane.Audit{Actor: "daemon", Action: "metric fixture", Outcome: "ok"}, "metric-task", controlplane.Event{Kind: kind, Payload: raw}); err != nil {
			t.Fatal(err)
		}
	}
	record("session-launched")
	audit := controlplane.Audit{Actor: "daemon", Action: "metric fixture", Outcome: "ok"}
	owner := scheduler.Owner{PID: 1, Started: 1}
	if err := d.store.EnsureLeasePool(ctx, audit, controlplane.LeasePoolConfig{Key: "role/metric", Kind: "role", Capacity: 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.store.EnqueueLease(ctx, audit, "role/metric", scheduler.Request{ID: "metric-lease", Owner: owner, Weight: 1, Queued: time.Now()}); err != nil {
		t.Fatal(err)
	}
	update, err := d.store.AdvanceLeases(ctx, audit, "role/metric", time.Now(), map[scheduler.Owner]scheduler.Liveness{owner: scheduler.Alive})
	if err != nil || len(update.Granted) != 1 {
		t.Fatalf("grant %+v: %v", update, err)
	}
	record("session-reaped")
	if err := d.store.ReleaseLease(ctx, audit, "role/metric", "metric-lease", owner, "session completed"); err != nil {
		t.Fatal(err)
	}

	before := rpcResult[map[string]json.RawMessage](t, d, "Metrics", MetricsParams{})
	var b EventMetrics
	if err = json.Unmarshal(before["events"], &b); err != nil || b.RoleGrants.Completed != 1 || b.RepeatGrantShare != 1 {
		t.Fatalf("before restart %+v %v", b, err)
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	after := rpcResult[map[string]json.RawMessage](t, d, "Metrics", MetricsParams{})
	var a EventMetrics
	if err = json.Unmarshal(after["events"], &a); err != nil || a.RoleGrants != b.RoleGrants || a.TimeLostToRestarts.Daemon.Count != 1 || a.TimeLostToRestarts.Seconds <= 0 {
		t.Fatalf("after restart %+v before %+v: %v", a, b, err)
	}
	var unavailable map[string]string
	if err = json.Unmarshal(after["unavailable"], &unavailable); err != nil || unavailable["repeat_grant_share"] != "" || unavailable["time_lost_to_restarts"] != "" {
		t.Fatalf("metrics remain unavailable %s: %v", after["unavailable"], err)
	}
}

func TestSessionEventsPreserveRoleGrantIdentity(t *testing.T) {
	root := "/isolated/instance"
	dispatch := &engine.Dispatch{ID: "dispatch", Stage: "implement", Visit: 2, Sessions: []engine.Session{{ID: "session", Role: "coder"}}}
	events := sessionEvents(controlplane.JobSnapshot{}, engine.Job{TaskID: "task", InFlight: dispatch}, root)
	if len(events) != 1 {
		t.Fatalf("session events %+v", events)
	}
	var p struct {
		Session string `json:"session"`
		LeaseID string `json:"lease_id"`
		Visit   int    `json:"visit"`
	}
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("%x", sha256.Sum256([]byte(root+"\x00session")))
	if p.Session != "session" || p.LeaseID != want || p.Visit != 2 {
		t.Fatalf("session provenance %+v want lease %s", p, want)
	}
}

func TestRoleMetricsDoNotCountUnadmittedSessionsAsGrants(t *testing.T) {
	epoch := time.Unix(10000, 0)
	event := func(kind string, n int, p string) controlplane.Event {
		return controlplane.Event{Kind: kind, JobID: "task", Timestamp: epoch.Add(time.Duration(n) * time.Second), Payload: json.RawMessage(p)}
	}
	events := []controlplane.Event{
		event("session-launched", 0, `{"session":"planned","role":"coder","stage":"implement","dispatch_id":"d1","visit":1}`),
		event("session-reaped", 1, `{"session":"planned","role":"coder"}`),
		event("session-launched", 2, `{"session":"admitted","role":"coder","stage":"implement","dispatch_id":"d2","visit":1}`),
		event("lease_granted", 3, `{"pool":"role/coder","value":{"id":"admitted"}}`),
		event("session-reaped", 7, `{"session":"admitted","role":"coder"}`),
		event("lease_released", 8, `{"pool":"role/coder","value":{"id":"admitted"}}`),
	}
	_, total, _ := roleAndRestartMetrics(events)
	if total.Seconds != 5 || total.Completed != 1 || total.RepeatSeconds != 0 || total.Share != 0 || total.Unmatched != 0 {
		t.Fatalf("planned turn counted as a grant %+v", total)
	}
}
