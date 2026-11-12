package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xleelz/fae/engine"
)

func TestAnalyzeSessionMatchesRecordedTimingsAndLabels(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, e = call(t, d, "Admit", AdmitParams{ID: "alpha", Content: "alpha", Tree: "tree", AuthorizingMessage: "message"}, "alpha"); e != nil {
		t.Fatal(e)
	}
	awaitState(t, d, "alpha", "Done")
	// FAE-CLI-052: recorded lease waits are reported, per lease, exactly.
	for _, w := range []leaseWait{{Task: "alpha", Stage: "implement", Lease: "role:implementer", Outcome: "granted", Wait: 1500 * time.Millisecond}, {Task: "alpha", Stage: "qa", Lease: "role:implementer", Outcome: "granted", Wait: 500 * time.Millisecond}, {Task: "alpha", Stage: "qa", Lease: "heavy-op", Outcome: "refused", Wait: 2 * time.Second}} {
		d.recordLeaseWait(w)
	}
	if _, e = call(t, d, "AnalyzeSession", AnalyzeParams{Task: "missing"}, ""); e == nil {
		t.Fatal("unknown task analyzed")
	}
	var a SessionAnalysis
	r, e := call(t, d, "AnalyzeSession", AnalyzeParams{Task: "alpha"}, "")
	decodeResult(t, r, e, &a)
	if a.Task != "alpha" || a.State != "Done" || !a.Terminal || a.Spend != "not inferred" {
		t.Fatalf("header %s", r.Result)
	}
	// Recompute stage wall time independently from the plane's recorded events.
	events, e := d.store.JobEvents(ctx, "alpha", 0, 100000)
	if e != nil || len(events) == 0 || events[0].Kind != "task_admitted" {
		t.Fatal(len(events), e)
	}
	want := map[string]float64{}
	stage, since := "Pending", events[0].Timestamp
	for _, ev := range events[1:] {
		var x engine.Event
		if e = json.Unmarshal(ev.Payload, &x); e != nil {
			t.Fatal(e)
		}
		if ev.Kind == "transition" && string(x.From) == stage {
			want[stage] += ev.Timestamp.Sub(since).Seconds()
			stage, since = string(x.To), ev.Timestamp
		}
	}
	got := map[string]float64{}
	turns := map[string]float64{}
	labels := map[string]string{}
	for _, m := range a.Metrics {
		if m.Source == "" || (m.Label != "exact" && m.Label != "estimate") {
			t.Fatalf("unlabelled or unsourced metric %+v", m)
		}
		labels[m.Name] = m.Label
		switch m.Name {
		case "stage_wall_seconds":
			got[m.Stage] = m.Value
		case "turns":
			turns[m.Role] = m.Value
		}
	}
	if len(got) != len(want) || len(want) < 10 {
		t.Fatalf("stages %v want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("stage %s: %v want exactly %v", k, got[k], v)
		}
	}
	if labels["stage_wall_seconds"] != "exact" || labels["turns"] != "exact" || labels["turn_seconds"] != "estimate" {
		t.Fatalf("labels %v", labels)
	}
	j, e := d.store.GetJob(ctx, "alpha")
	if e != nil {
		t.Fatal(e)
	}
	var env envelope
	if e = decodeEnvelope(j.Payload, &env); e != nil {
		t.Fatal(e)
	}
	dispatches := map[string]map[string]bool{}
	for _, p := range env.Job.Provenance {
		if p.Role == "" {
			continue
		}
		if dispatches[p.Role] == nil {
			dispatches[p.Role] = map[string]bool{}
		}
		dispatches[p.Role][p.DispatchID] = true
	}
	for role, ids := range dispatches {
		if turns[role] != float64(len(ids)) {
			t.Fatalf("role %s turns %v want %d", role, turns[role], len(ids))
		}
	}
	gaps := map[string]bool{}
	for _, g := range a.Gaps {
		if g.Reason == "" {
			t.Fatal("gap without reason", g)
		}
		gaps[g.Name] = true
	}
	if !gaps["tokens"] || gaps["lease_wait_seconds"] {
		t.Fatalf("gaps %+v", a.Gaps)
	}
	waits := map[string]SessionMetric{}
	for _, m := range a.Metrics {
		if m.Name == "lease_wait_seconds" || m.Name == "lease_waits_refused" {
			waits[m.Name+" "+m.Class] = m
		}
	}
	if m := waits["lease_wait_seconds role:implementer"]; m.Value != 2 || m.Count != 2 || m.Label != "exact" {
		t.Fatalf("role lease waits %+v", waits)
	}
	if m := waits["lease_wait_seconds heavy-op"]; m.Value != 2 || m.Count != 1 || waits["lease_waits_refused heavy-op"].Value != 1 || len(waits) != 3 {
		t.Fatalf("heavy lease waits %+v", waits)
	}
}
