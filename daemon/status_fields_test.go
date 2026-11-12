package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

func statusBody(t *testing.T, d *Daemon, params any) map[string]any {
	t.Helper()
	r, err := call(t, d, "Status", params, "")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err = json.Unmarshal(r.Result, &body); err != nil {
		t.Fatal(err)
	}
	return body
}
func lookup(body map[string]any, path string) (any, bool) {
	var v any = body
	for _, key := range strings.Split(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		if v, ok = m[key]; !ok {
			return nil, false
		}
	}
	return v, true
}

// FAE-CLI-012: the JSON status carries every listed field (withheld counts are
// reported by the scheduler's withheld view) and answers under 1 s while the
// read pool is saturated by heavy reads.
func TestStatusFieldListAndLatencyUnderHeavyReads(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "listed", Tree: "tree", AuthorizingMessage: "message"}, "listed"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "listed", "Done")
	var body map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for {
		body = statusBody(t, d, map[string]any{"tasks": true})
		commit, _ := lookup(body, "plane.last_commit")
		done, _ := lookup(body, "snapshot.counts.Done")
		if commit != nil && done != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, path := range []string{
		"instance", "pid", "uptime_seconds", "version.version", "version.commit", "version.binary_sha256",
		"plane.endpoint", "plane.data_dir", "plane.schema_version", "plane.last_commit.hash", "plane.last_commit.time",
		"adapter", "snapshot.drive.alive", "snapshot.drive.rounds", "snapshot.drive.last_round", "snapshot.drive.parked", "snapshot.drive.cause",
		"snapshot.counts", "leases", "panes", "heavy_ops", "host.memory_available_bytes", "host.io_full_avg10", "host.memory_full_avg10", "host.load_per_core",
		"provider_health.bindings", "provider_health.eligible", "ui.enabled", "snapshot.maintenance", "shutting_down", "snapshot.age_ms", "extras_age_ms", "socket",
	} {
		if _, ok := lookup(body, path); !ok {
			t.Errorf("status lacks %s: %v", path, body)
		}
	}
	tasks, _ := lookup(body, "snapshot.tasks")
	if rows, ok := tasks.([]any); !ok || len(rows) != 1 {
		t.Fatalf("--tasks rows %v", tasks)
	}
	if plain := statusBody(t, d, nil); plain["snapshot"].(map[string]any)["tasks"] != nil {
		t.Fatal("task rows included without --tasks")
	}
	if c, _ := lookup(body, "snapshot.counts.Done"); c != float64(1) {
		t.Fatalf("counts %v", body["snapshot"])
	}
	// show reports the latest artifact per stage of a finished task.
	r, err := call(t, d, "Show", ShowParams{Task: "listed"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var shown struct {
		Tasks []ShowTask `json:"tasks"`
	}
	if err = json.Unmarshal(r.Result, &shown); err != nil || len(shown.Tasks) != 1 || shown.Tasks[0].Stage != "Done" || len(shown.Tasks[0].Artifacts) < 10 {
		t.Fatalf("show %s %v", r.Result, err)
	}
	stages := map[string]bool{}
	for _, a := range shown.Tasks[0].Artifacts {
		if stages[a.Stage] || a.ArtifactID == "" {
			t.Fatalf("latest artifacts not one per stage: %+v", shown.Tasks[0].Artifacts)
		}
		stages[a.Stage] = true
	}
	// Saturate the read pool with four blocked heavy reads; status must not queue behind them.
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	for i := 0; i < 4; i++ {
		go d.readPool.run(context.Background(), "heavy-"+string(rune('a'+i)), func(work context.Context) (any, error) {
			started <- struct{}{}
			select {
			case <-release:
			case <-work.Done():
			}
			return nil, nil
		})
	}
	for i := 0; i < 4; i++ {
		<-started
	}
	for i := 0; i < 20; i++ {
		begin := time.Now()
		statusBody(t, d, map[string]any{"tasks": true})
		if elapsed := time.Since(begin); elapsed >= time.Second {
			t.Fatalf("status took %v with four heavy reads in flight", elapsed)
		}
	}
	close(release)
}

// The live branch reads the host-wide heavy and pane ledgers and the injected
// host sampler; plane failures are reported per field rather than failing status.
func TestStatusExtrasSampleLiveLedgers(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m.MatchExpectationsInOrder(false)
	m.ExpectQuery("dolt_log").WillReturnError(errors.New("plane down"))
	m.ExpectQuery("resource_pools").WillReturnRows(sqlmock.NewRows([]string{"kind", "payload"}).AddRow("role", `{"Capacity":2,"TTL":1,"WarnAfter":1,"MaxWait":1,"Holders":[{"ID":"h"}],"Waiting":[{"ID":"w"}]}`))
	m.ExpectQuery("provider_health").WillReturnError(errors.New("plane down"))
	dir := t.TempDir()
	host, err := scheduler.OpenDynamicLedger(filepath.Join(dir, "host"), 2)
	if err != nil {
		t.Fatal(err)
	}
	panes, err := scheduler.OpenPaneLedger(filepath.Join(dir, "panes"), 7)
	if err != nil {
		t.Fatal(err)
	}
	d := &Daemon{store: controlplane.New(db, controlplane.Options{})}
	d.live = &liveExecutor{guard: &liveAdmission{daemon: d, host: host, panes: panes, paneLimit: 7}}
	x := d.sampleStatusExtras(context.Background(), func(context.Context) (hostguard.Sample, error) {
		return hostguard.Sample{Memory: hostguard.Reading{Value: 8 << 30, Known: true}, IO10: hostguard.Reading{Value: 1.5, Known: true}}, nil
	})
	heavy, _ := x.HeavyOps.(map[string]any)
	pane, _ := x.Panes.(map[string]any)
	hostView, _ := x.Host.(map[string]any)
	leases, _ := x.Leases.(map[string]controlplane.LeaseUsage)
	if heavy == nil || heavy["capacity"] != 2 || pane == nil || pane["limit"] != 7 || pane["live"] != 0 || hostView["memory_available_bytes"] != float64(8<<30) || hostView["load_per_core"] != nil {
		t.Fatalf("live extras %+v", x)
	}
	if leases["role"].Holders != 1 || leases["role"].Waiters != 1 || x.Errors["plane.last_commit"] == "" || x.Errors["provider_health"] == "" || x.LastCommit != nil {
		t.Fatalf("plane extras %+v %+v", leases, x.Errors)
	}
}
