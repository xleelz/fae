package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/workflow"
)

func jobEvent(t *testing.T, seq int64, job string, at time.Time, e engine.Event) controlplane.Event {
	t.Helper()
	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return controlplane.Event{Sequence: seq, Kind: e.Kind, JobID: job, Payload: raw, Timestamp: at}
}

// FAE-OBS-004: every event-derived figure against a run with known timings.
func TestEventMetricsKnownTimings(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	var ev []controlplane.Event
	add := func(job string, s int, e engine.Event) {
		ev = append(ev, jobEvent(t, int64(len(ev)+1), job, at(s), e))
	}
	raw := func(kind, job string, s int, payload string) {
		ev = append(ev, controlplane.Event{Sequence: int64(len(ev) + 1), Kind: kind, JobID: job, Payload: json.RawMessage(payload), Timestamp: at(s)})
	}
	pass, fail, unknown := &workflow.GateResult{Value: "true", Passed: true}, &workflow.GateResult{Value: "false"}, &workflow.GateResult{Value: "unknown"}
	for i, job := range []string{"a", "b"} {
		base := i * 100
		raw("task_admitted", job, base, `{}`)
		add(job, base+10, engine.Event{Kind: "transition", From: domain.Pending, To: "spec", Attempt: 1, Cause: "admitted"})
		add(job, base+40, engine.Event{Kind: "transition", From: "spec", To: "gate", Attempt: 1})
		add(job, base+41, engine.Event{Kind: "gate", Stage: "gate", Attempt: 1, Gate: pass})
		add(job, base+70, engine.Event{Kind: "transition", From: "gate", To: domain.Done, Attempt: 1 + i})
	}
	add("b", 210, engine.Event{Kind: "gate", Stage: "gate", Attempt: 2, Gate: fail})
	add("b", 211, engine.Event{Kind: "gate", Stage: "gate", Attempt: 3, Gate: unknown})
	add("c", 220, engine.Event{Kind: "park", Stage: "spec", Attempt: 1, Cause: "MissingInput: [plan]"})
	add("c", 221, engine.Event{Kind: "park", Stage: "spec", Attempt: 1, Cause: "all-routes-unavailable"})
	raw(leaseWaitEventKind, "c", 230, `{"lease":"heavy-op","wait_seconds":12.5,"outcome":"granted"}`)
	raw(leaseWaitEventKind, "c", 231, `{"lease":"heavy-op","wait_seconds":2.5,"outcome":"refused"}`)
	raw("external-effect-intent", "c", 240, `{}`)
	raw("external-effect-performed", "c", 241, `{}`)
	raw("alert-raised", "", 250, `{"id":1,"source":"dead-edge"}`)
	m := eventMetrics(ev, 2*time.Hour)
	if m.Events != len(ev) || m.TransitionsPerHour != 3 {
		t.Fatalf("transitions %+v", m)
	}
	if m.StageWall["spec"] != (Durations{Count: 2, Median: 30, P90: 30, Max: 30}) || m.StageWall["gate"] != (Durations{Count: 2, Median: 30, P90: 30, Max: 30}) {
		t.Fatalf("stage wall %+v", m.StageWall)
	}
	if m.AdmissionToRelease != (Durations{Count: 2, Median: 10, P90: 10, Max: 10}) || m.ReleaseToDone != (Durations{Count: 2, Median: 60, P90: 60, Max: 60}) {
		t.Fatalf("waits %+v %+v", m.AdmissionToRelease, m.ReleaseToDone)
	}
	g := m.Gates["gate"]
	if g.Evaluated != 4 || g.Rejected != 2 || g.Rate != 0.5 || g.Causes["false"] != 1 || g.Causes["unknown"] != 1 {
		t.Fatalf("gates %+v", g)
	}
	if m.AttemptsPerTask != (Durations{Count: 3, Median: 1, P90: 3, Max: 3}) {
		t.Fatalf("attempts %+v", m.AttemptsPerTask)
	}
	if m.ParksByReason["MissingInput"] != 1 || m.ParksByReason["all-routes-unavailable"] != 1 {
		t.Fatalf("parks %+v", m.ParksByReason)
	}
	if m.LeaseWait["heavy-op"] != (Durations{Count: 2, Median: 2.5, P90: 12.5, Max: 12.5}) {
		t.Fatalf("lease wait %+v", m.LeaseWait)
	}
	if m.ExternalEffects["intent"] != 1 || m.ExternalEffects["performed"] != 1 || m.Alerts["dead-edge"] != 1 {
		t.Fatalf("effects %+v alerts %+v", m.ExternalEffects, m.Alerts)
	}
	if summarise(nil) != (Durations{}) {
		t.Fatal("empty summary")
	}
}

// Appendix A: the host ring keeps 15 minutes at 5 s, oldest first, and an
// unreadable reading stays null rather than zero.
func TestHostMetricsRingBuffer(t *testing.T) {
	d := &Daemon{layout: Layout{Root: t.TempDir()}}
	n := 0
	d.obs.sample = func(context.Context) (hostguard.Sample, error) {
		n++
		if n == 200 {
			return hostguard.Sample{}, errors.New("meminfo unreadable")
		}
		return hostguard.Sample{Memory: hostguard.Reading{Value: float64(n), Known: true}}, nil
	}
	for i := 0; i < 200; i++ {
		_ = d.sampleHost(context.Background())
	}
	got := d.hostSamples()
	if len(got) != 180 || *got[0].MemoryAvailable != 21 || *got[178].MemoryAvailable != 199 || got[179].MemoryAvailable != nil || got[179].Error == "" || got[0].IOFull10 != nil || got[0].DiskUsed == nil {
		t.Fatalf("ring %d %+v %+v", len(got), got[0], got[179])
	}
	page, err := d.hostMetricsRead(nil)
	if err != nil || page.(HostMetricsPage).ResolutionSeconds != 5 || page.(HostMetricsPage).WindowSeconds != 900 {
		t.Fatal(page, err)
	}
}

func TestAlertMirrorLoadsTornFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), alertsFileName)
	at := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	content := `{"event":"raised","id":3}` + "\n" + `{"event":"acknowledged","id":2,"acknowledged_at":"` + at.Format(time.RFC3339Nano) + `"}` + "\n" + `{"event":"raised","id":4}` + "\n" + `{"event":"rai`
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	var m alertMirror
	torn, err := m.load(path)
	if err != nil || !torn || m.raised != 4 || !m.acked[2] || !m.ackSince.Equal(at) {
		t.Fatalf("mirror raised=%d acked=%v since=%v %v %v", m.raised, m.acked, m.ackSince, torn, err)
	}
}

func rpcResult[T any](t *testing.T, d *Daemon, method string, params any) T {
	t.Helper()
	reply, err := call(t, d, method, params, "")
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	var out T
	if err = json.Unmarshal(reply.Result, &out); err != nil {
		t.Fatalf("%s: %v %s", method, err, reply.Result)
	}
	return out
}
func zombieChildren(t *testing.T) map[string]bool {
	t.Helper()
	all, err := processowner.Census()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, p := range all {
		if p.PPID == os.Getpid() && p.Defunct() {
			out[p.Start+"/"+p.Command] = true
		}
	}
	return out
}
func alertFileLines(t *testing.T, path string) []alertFileLine {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []alertFileLine
	s := bufio.NewScanner(f)
	for s.Scan() {
		var l alertFileLine
		if json.Unmarshal(s.Bytes(), &l) == nil {
			out = append(out, l)
		}
	}
	return out
}

// TestObservabilityNative drives a scripted run on a real plane and checks
// FAE-OBS-001 (every log line parses), FAE-OBS-004 (Metrics and HostMetrics),
// FAE-OBS-005 (process-leak, dead-edge and route-exhaustion sources, the
// alerts file, the audited acknowledgement), FAE-OBS-006 (provider line
// pairing), FAE-HOST-022 (a fixture chain in the instance tree raises the
// leak alert) and FAE-HOST-023 (no defunct children after the run).
func TestObservabilityNative(t *testing.T) {
	cfg, ctx := fixture(t)
	before := zombieChildren(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	for _, id := range []string{"one", "two"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"one", "two"} {
		awaitState(t, d, id, "Done")
	}

	// FAE-HOST-022: a fixture chain of small processes in the daemon's tree.
	chain := exec.Command("/bin/sh", "-c", "sleep 60 & sleep 60 & sleep 60 & sleep 60 & sleep 60 & wait")
	chain.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = chain.Start(); err != nil {
		t.Fatal(err)
	}
	killChain := func() { _ = syscall.Kill(-chain.Process.Pid, syscall.SIGKILL); _ = chain.Wait() }
	defer killChain()
	d.obs.mu.Lock()
	d.obs.limits = processowner.LeakLimits{Processes: 4, RSS: 64 << 30, Defunct: 20}
	d.obs.mu.Unlock()
	deadline := time.Now().Add(10 * time.Second)
	for {
		d.observeRound(ctx)
		if r := d.processReport(); r != nil && len(r.Leaks) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no leak detected: %+v", d.processReport())
		}
		time.Sleep(50 * time.Millisecond)
	}
	d.observeRound(ctx) // still over the bound: no second alert
	killChain()
	d.observeRound(ctx)

	// Dead edge: a cancelled blocking prerequisite holds its dependent.
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	item := func(id string) BacklogItemParams {
		return BacklogItemParams{AdmitParams: AdmitParams{ID: id, Tree: "tree", Priority: 10, Content: "accept " + id}}
	}
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{item("pre"), item("dep")}, Edges: []controlplane.BacklogEdge{{From: "pre", To: "dep", Kind: "blocking"}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Cancel", TaskControlParams{ID: "pre", Reason: "cancelled prerequisite", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(10 * time.Second)
	for d.statusTasks()["pre"].State != "Cancelled" {
		if time.Now().After(deadline) {
			t.Fatal("status snapshot never showed the cancellation")
		}
		time.Sleep(20 * time.Millisecond)
	}
	d.observeRound(ctx)
	d.observeRound(ctx) // deduplicated by episode

	// Route exhaustion: a task parked on unavailable routes beyond the bound.
	park, _ := json.Marshal(engine.Event{Kind: "park", Stage: "implement", Cause: "all-routes-unavailable"})
	if err = d.store.RecordTaskEvent(ctx, controlplane.Audit{Actor: "daemon", Action: "test park", Outcome: "ok"}, "stuck", controlplane.Event{Kind: "park", Payload: park}); err != nil {
		t.Fatal(err)
	}
	parked, err := d.store.LastJobEvent(ctx, "stuck", "park")
	if err != nil {
		t.Fatal(err)
	}
	stuck := map[string]BoardItem{"stuck": {JobIdentity: JobIdentity{ID: "stuck", State: "implement"}, Reason: "all-routes-unavailable"}}
	// FAE-RT-010: a park on routes the bridge refused is a route park too;
	// its alert carries the reason naming the refusals.
	refusedReason := "all-routes-unavailable: every route for role coder was refused by the bridge before its provider ran: p/m: refused"
	refusedPark, _ := json.Marshal(engine.Event{Kind: "park", Stage: "implement", Cause: refusedReason})
	if err = d.store.RecordTaskEvent(ctx, controlplane.Audit{Actor: "daemon", Action: "test park", Outcome: "ok"}, "refused", controlplane.Event{Kind: "park", Payload: refusedPark}); err != nil {
		t.Fatal(err)
	}
	stuck["refused"] = BoardItem{JobIdentity: JobIdentity{ID: "refused", State: "implement"}, Reason: refusedReason}
	d.obs.round.Lock()
	for _, after := range []time.Duration{29 * time.Minute, 31 * time.Minute, 40 * time.Minute} {
		if err = d.routeExhaustion(ctx, stuck, parked.Timestamp.Add(after)); err != nil {
			t.Fatal(err)
		}
	}
	d.obs.round.Unlock()

	page := rpcResult[AlertsPage](t, d, "Alerts", AlertsParams{})
	bySource := map[string][]controlplane.Alert{}
	for _, a := range page.Alerts {
		bySource[a.Source] = append(bySource[a.Source], a)
	}
	if len(bySource["process-leak"]) != 1 || len(bySource["dead-edge"]) != 1 || len(bySource["route-exhaustion"]) != 2 {
		t.Fatalf("alert sources %+v", page.Alerts)
	}
	routeAlerts := map[string]controlplane.Alert{}
	for _, a := range bySource["route-exhaustion"] {
		var detail struct{ Task string }
		_ = json.Unmarshal(a.Detail, &detail)
		routeAlerts[detail.Task] = a
	}
	if !strings.Contains(string(routeAlerts["refused"].Detail), "every route for role coder was refused") {
		t.Fatalf("refused-route alert lacks the reason: %s", routeAlerts["refused"].Detail)
	}
	var leak struct {
		Leak processowner.Leak `json:"leak"`
	}
	if err = json.Unmarshal(bySource["process-leak"][0].Detail, &leak); err != nil || leak.Leak.Command != "sleep" || leak.Leak.Reason != "processes" || leak.Leak.Processes < 5 {
		t.Fatalf("leak alert %s %v", bySource["process-leak"][0].Detail, err)
	}
	if !strings.Contains(string(bySource["dead-edge"][0].Detail), `"prerequisite_outcome":"Cancelled"`) || !strings.Contains(string(routeAlerts["stuck"].Detail), `"task":"stuck"`) {
		t.Fatalf("details %s %s", bySource["dead-edge"][0].Detail, routeAlerts["stuck"].Detail)
	}
	if history, err := d.store.LastJobEvent(ctx, "dep", "alert-raised"); err != nil || !strings.Contains(string(history.Payload), "dead-edge") {
		t.Fatalf("dead edge missing from the dependent's history: %v", err)
	}

	// The audited acknowledgement verb.
	target := bySource["dead-edge"][0].ID
	if _, err = call(t, d, "AlertAcknowledge", AlertAcknowledgeParams{ID: target}, ""); err == nil {
		t.Fatal("acknowledgement without authority accepted")
	}
	acked := rpcResult[controlplane.Alert](t, d, "AlertAcknowledge", AlertAcknowledgeParams{ID: target, AuthorizingMessage: "message"})
	if acked.AcknowledgedAt == nil {
		t.Fatal("acknowledgement not recorded")
	}
	if _, err = call(t, d, "AlertAcknowledge", AlertAcknowledgeParams{ID: target, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("repeated acknowledgement accepted")
	}
	open := rpcResult[AlertsPage](t, d, "Alerts", AlertsParams{Unacknowledged: true})
	for _, a := range open.Alerts {
		if a.ID == target {
			t.Fatal("acknowledged alert listed as open")
		}
	}
	if ev, err := d.store.LastJobEvent(ctx, "", "alert-acknowledged"); err != nil || !strings.Contains(string(ev.Payload), `"message":"message"`) {
		t.Fatalf("acknowledgement event %s %v", ev.Payload, err)
	}

	// FAE-OBS-006: a non-ok provider observation pairs one row with one line.
	dispatch := engine.Dispatch{TaskID: "one", Stage: "implement", ID: "dispatch-1"}
	observedAt := time.Now().UTC()
	for i := 0; i < 2; i++ {
		if err = d.observeProvider(ctx, dispatch, controlplane.ProviderObservation{ID: "session-throttled", TaskID: "one", Provider: "claude", Model: "m", Outcome: providerhealth.RateLimited, At: observedAt}); err != nil {
			t.Fatal(err)
		}
	}
	if err = d.observeProvider(ctx, dispatch, controlplane.ProviderObservation{ID: "session-ok", TaskID: "one", Provider: "claude", Model: "m", Outcome: providerhealth.OK, At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	// FAE-OBS-004 reads.
	metrics := rpcResult[map[string]json.RawMessage](t, d, "Metrics", MetricsParams{})
	var em EventMetrics
	if err = json.Unmarshal(metrics["events"], &em); err != nil || em.ReleaseToDone.Count != 2 || em.AdmissionToRelease.Count != 2 || len(em.StageWall) == 0 || len(em.Gates) == 0 || em.TransitionsPerHour <= 0 || em.Alerts["process-leak"] != 1 {
		t.Fatalf("event metrics %+v %v", em, err)
	}
	var byState map[string]int
	if err = json.Unmarshal(metrics["tasks_by_state"], &byState); err != nil || byState["Done"] != 2 || byState["Cancelled"] != 1 {
		t.Fatalf("tasks by state %s", metrics["tasks_by_state"])
	}
	var withheld map[string]int
	if err = json.Unmarshal(metrics["withheld_edges_by_prerequisite"], &withheld); err != nil || withheld["prerequisite Cancelled"] != 1 {
		t.Fatalf("withheld %s", metrics["withheld_edges_by_prerequisite"])
	}
	for _, key := range []string{"processes", "provider_health", "lease_utilisation", "host", "unavailable"} {
		if len(metrics[key]) == 0 || string(metrics[key]) == "null" {
			t.Fatalf("metrics lacks %s: %v", key, metrics)
		}
	}
	host := rpcResult[HostMetricsPage](t, d, "HostMetrics", map[string]any{})
	if len(host.Samples) == 0 || (runtime.GOOS == "linux" && host.Samples[len(host.Samples)-1].MemoryAvailable == nil) {
		t.Fatalf("host metrics %+v", host)
	}
	if _, err = call(t, d, "Metrics", MetricsParams{WindowSeconds: -1}, ""); err == nil {
		t.Fatal("negative window accepted")
	}

	// The alerts file mirrors the plane and survives a torn append and restart.
	lines := alertFileLines(t, page.File)
	if len(lines) != len(page.Alerts)+1 || lines[len(lines)-1].Event != "acknowledged" || lines[len(lines)-1].ID != target {
		t.Fatalf("alerts file %+v for %+v", lines, page.Alerts)
	}
	f, err := os.OpenFile(page.File, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"event":"rai`)
	f.Close()
	stop(t, d)
	d = nil
	if after := zombieChildren(t); len(after) > len(before) {
		deadline = time.Now().Add(5 * time.Second)
		for len(zombieChildren(t)) > len(before) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
	}
	for z := range zombieChildren(t) {
		if !before[z] {
			t.Fatalf("defunct child left after the run: %s", z)
		}
	}
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.obs.round.Lock()
	if err = d.routeExhaustion(ctx, map[string]BoardItem{"stuck": stuck["stuck"]}, parked.Timestamp.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	d.obs.round.Unlock()
	if _, _, err = d.raiseAlert(ctx, controlplane.Alert{Source: "test-restart", Detail: json.RawMessage(`{"n":1}`)}, false); err != nil {
		t.Fatal(err)
	}
	ids := map[int64]int{}
	for _, l := range alertFileLines(t, page.File) {
		if l.Event == "raised" {
			ids[l.ID]++
		}
	}
	if len(ids) != len(page.Alerts)+1 {
		t.Fatalf("alerts file after restart %+v", ids)
	}
	for id, n := range ids {
		if n != 1 {
			t.Fatalf("alert %d mirrored %d times", id, n)
		}
	}
	stop(t, d)
	d = nil

	// FAE-OBS-001: every supervision line parses, across both daemon runs.
	raw, err := os.ReadFile(filepath.Join(cfg.Root, cfg.Instance, "supervision.log"))
	if err != nil {
		t.Fatal(err)
	}
	components := map[string]int{}
	provider := 0
	for _, l := range parseSupervisionLines(t, raw, cfg.Instance) {
		components[l[2]]++
		if l[2] == "provider" {
			provider++
			if !strings.Contains(l[3], "outcome=rate-limited") || !strings.Contains(l[3], `task="one" stage="implement" turn="dispatch-1"`) {
				t.Fatalf("provider line %q", l[3])
			}
		}
	}
	if components["daemon"] < 4 || components["alerts"] < 5 || provider != 1 {
		t.Fatalf("components %v", components)
	}
}

// The shared alert path never panics without a plane (admission fixtures).
func TestRaiseAlertWithoutPlane(t *testing.T) {
	d := &Daemon{}
	if _, raised, err := d.raiseAlert(context.Background(), controlplane.Alert{Source: "host-hard-refusal"}, false); err == nil || raised {
		t.Fatal(raised, err)
	}
}
