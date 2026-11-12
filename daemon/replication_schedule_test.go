package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/hostguard"
)

func replicationScheduleCall(t *testing.T, d *Daemon, p ReplicationScheduleParams) ReplicationScheduleRecord {
	t.Helper()
	response, e := call(t, d, "BackupReplicationSchedule", p, "")
	if e != nil {
		t.Fatal(e)
	}
	var s ReplicationScheduleRecord
	if e = json.Unmarshal(response.Result, &s); e != nil {
		t.Fatal(e)
	}
	return s
}
func TestReplicationScheduleGuard(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			for _, c := range []bool{false, true} {
				if CanRunReplicationSchedule(a, b, c) != (a && b && c) {
					t.Fatal(a, b, c)
				}
			}
		}
	}
}
func TestReplicationScheduleNativeUncertainRestartFairness(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	scheduleGrant(t, d, "replicate-schedule")
	first, e := backupAdmitted(t, ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	second, e := backupAdmitted(t, ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	puts, queries := 0, 0
	lost := true
	absent := false
	payloads := map[string][]byte{}
	receipts := map[string]BackupReplicationReceipt{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		key := strings.TrimPrefix(r.URL.Path, "/backups/")
		if r.Method == http.MethodPut {
			puts++
			data, _ := io.ReadAll(r.Body)
			sum := sha256.Sum256(data)
			if key != hex.EncodeToString(sum[:]) {
				t.Error("archive identity differs")
			}
			payloads[key] = data
			receipt := BackupReplicationReceipt{ID: key, SHA256: key, ManifestSHA256: r.Header.Get("X-Fae-Manifest-SHA256"), Size: int64(len(data)), State: "stored"}
			receipts[key] = receipt
			if lost {
				lost = false
				w.WriteHeader(503)
				return
			}
			json.NewEncoder(w).Encode(receipt)
			return
		}
		if strings.HasSuffix(key, "/content") {
			w.Write(payloads[strings.TrimSuffix(key, "/content")])
			return
		}
		queries++
		if absent {
			w.WriteHeader(404)
			return
		}
		receipt, ok := receipts[key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(receipt)
	}))
	defer server.Close()
	config := ReplicationScheduleParams{Action: "configure", Endpoint: server.URL, IntervalSeconds: 3600, AuthorizingMessage: "replicate-schedule"}
	plain := config
	plain.AuthorizingMessage = "plain"
	if e = d.store.RecordMessage(ctx, "plain", "Run scheduled backups"); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "BackupReplicationSchedule", plain, ""); e == nil {
		t.Fatal("plain recurrence authority accepted")
	}
	for _, text := range []string{`{"authorize":{"verbs":["BackupReplicationSchedule"]}}`, `{"authorize":{"verbs":["BackupReplicate"]}}`} {
		id := "restricted-" + controlplane.Digest([]byte(text))
		d.store.RecordMessage(ctx, id, text)
		bad := config
		bad.AuthorizingMessage = id
		if _, e = call(t, d, "BackupReplicationSchedule", bad, ""); e == nil {
			t.Fatal("incomplete recurrence grant accepted")
		}
	}
	s := replicationScheduleCall(t, d, config)
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()
	if e = d.tickReplicationSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	d.draining = false
	d.mu.Unlock()
	beginMaintenance(t, d)
	if e = d.tickReplicationSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	endMaintenance(t, d)
	if e = tickAdmitted(t, "replication", func() error { return d.tickReplicationSchedule(ctx, s.NextAt) }); e == nil {
		t.Fatal("uncertain upload succeeded")
	}
	s, e = d.readReplicationSchedule(ctx)
	if e != nil || s.Pending == nil || s.Pending.Backup != first.Path || s.Running {
		t.Fatal("oldest selection not retained", s, e)
	}
	protected, e := d.replicationProtectedBackups(ctx)
	if e != nil || !protected[first.Path] {
		t.Fatal("pending source not protected", e)
	}
	bad := config
	bad.Endpoint = server.URL + "/different"
	if _, e = call(t, d, "BackupReplicationSchedule", bad, ""); e == nil {
		t.Fatal("pending configuration overwritten")
	}
	s = replicationScheduleCall(t, d, ReplicationScheduleParams{Action: "pause", AuthorizingMessage: "replicate-schedule"})
	if s.Pending == nil {
		t.Fatal("pause lost pending")
	}
	if _, e = call(t, d, "BackupReplicationSchedule", bad, ""); e == nil {
		t.Fatal("paused pending configuration overwritten")
	}
	s = replicationScheduleCall(t, d, ReplicationScheduleParams{Action: "resume", AuthorizingMessage: "replicate-schedule"})
	s.Running = true
	if e = d.saveReplicationSchedule(ctx, s, controlplane.Audit{Actor: "daemon", Action: "simulate interrupted claim", Outcome: "running"}); e != nil {
		t.Fatal(e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	recovered, e := d.readReplicationSchedule(ctx)
	if e != nil || recovered.Running || !targetEqual(recovered.Pending, s.Pending) {
		t.Fatal("restart lost claim", recovered, e)
	}
	mu.Lock()
	if puts != 1 || queries != 0 {
		t.Error("startup performed network", puts, queries)
	}
	absent = true
	mu.Unlock()
	if e = tickAdmitted(t, "replication", func() error { return d.tickReplicationSchedule(ctx, recovered.NextAt) }); e == nil {
		t.Fatal("missing remote proof accepted")
	}
	s, e = d.readReplicationSchedule(ctx)
	if e != nil || s.Pending == nil {
		t.Fatal(s, e)
	}
	mu.Lock()
	absent = false
	mu.Unlock()
	if e = tickAdmitted(t, "replication", func() error { return d.tickReplicationSchedule(ctx, s.NextAt) }); e != nil {
		t.Fatal(e)
	}
	s, e = d.readReplicationSchedule(ctx)
	if e != nil || s.Pending != nil || s.LastBackup != first.Path {
		t.Fatal("pending not completed", s, e)
	}
	if e = tickAdmitted(t, "replication", func() error { return d.tickReplicationSchedule(ctx, s.NextAt) }); e != nil {
		t.Fatal(e)
	}
	s, e = d.readReplicationSchedule(ctx)
	if e != nil || s.LastBackup != second.Path {
		t.Fatal("queue did not advance fairly", s, e)
	}
	if e = tickAdmitted(t, "replication", func() error { return d.tickReplicationSchedule(ctx, s.NextAt) }); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	if puts != 2 || queries != 2 {
		t.Error("replayed upload or unbounded drain", puts, queries)
	}
	mu.Unlock()
}
func TestReplicationScheduleNativeSelectedBeforeOutboxAndFreshAuthority(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	scheduleGrant(t, d, "grant")
	backup, e := backupAdmitted(t, ctx, d)
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	puts := 0
	var data []byte
	var receipt BackupReplicationReceipt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPut {
			puts++
			data, _ = io.ReadAll(r.Body)
			sum := sha256.Sum256(data)
			id := hex.EncodeToString(sum[:])
			receipt = BackupReplicationReceipt{ID: id, SHA256: id, ManifestSHA256: r.Header.Get("X-Fae-Manifest-SHA256"), Size: int64(len(data)), State: "stored"}
			json.NewEncoder(w).Encode(receipt)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/content") {
			w.Write(data)
			return
		}
		json.NewEncoder(w).Encode(receipt)
	}))
	defer server.Close()
	s := replicationScheduleCall(t, d, ReplicationScheduleParams{Action: "configure", Endpoint: server.URL, IntervalSeconds: 3600, AuthorizingMessage: "grant"})
	d.backupMu.Lock()
	s.Pending, e = d.selectReplicationBackup(ctx, s, controlplane.Audit{Actor: "daemon", Action: "simulate claim", Outcome: "selected"})
	d.backupMu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	s.Running = true
	if e = d.saveReplicationSchedule(ctx, s, controlplane.Audit{Actor: "daemon", Action: "simulate before outbox crash", Outcome: "running"}); e != nil {
		t.Fatal(e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	s, e = d.readReplicationSchedule(ctx)
	if e != nil || s.Running || s.Pending == nil {
		t.Fatal(s, e)
	}
	// Fresh authority is checked again within the core transfer, not just at selection.
	pinned := context.WithValue(ctx, replicationScheduleContextKey{}, replicationScheduleContext{Claim: *s.Pending, Message: "grant"})
	params := BackupReplicationParams{Backup: backup.Path, Endpoint: s.Endpoint, MaxBytes: s.MaxBytes}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	observe := d.live.guard.observe
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) {
		once.Do(func() { close(entered); <-release })
		return hostguard.Sample{Memory: hostguard.Reading{Value: 100 * float64(hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: 1, Known: true}, LoadPerCore: hostguard.Reading{Value: 1, Known: true}}, nil
	}
	finished := make(chan error, 1)
	go func() {
		_, err := d.backupReplicate(pinned, params, controlplane.Audit{Actor: "daemon", Action: "fixture late authority", Outcome: "attempt"})
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("host admission not reached")
	}
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "operator", Action: "expire fixture grant", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE operator_messages SET created_at=? WHERE message_id=?", time.Now().Add(-48*time.Hour), "grant")
		return err
	}); e != nil {
		t.Fatal(e)
	}

	close(release)
	select {
	case e = <-finished:
		if e == nil {
			t.Fatal("expired post-wait authority accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked transfer did not finish")
	}
	d.live.guard.observe = observe
	assertHostCounts(t, d.live.guard, 0, 0)

	if _, e = d.store.GetMarker(ctx, replicationKey(params)); e != sql.ErrNoRows {
		t.Fatal("expired authority created outbox", e)
	}
	if e = d.tickReplicationSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	s, e = d.readReplicationSchedule(ctx)
	if e != nil || s.State != "blocked" || s.Pending == nil {
		t.Fatal(s, e)
	}
	scheduleGrant(t, d, "fresh")
	s = replicationScheduleCall(t, d, ReplicationScheduleParams{Action: "resume", AuthorizingMessage: "fresh"})
	// A changed self-consistent manifest still cannot replace the selected bytes.
	manifest := filepath.Join(backup.Path, "verified.json")
	original, e := os.ReadFile(manifest)
	if e != nil {
		t.Fatal(e)
	}
	var b planeBackup
	json.Unmarshal(original, &b)
	b.Created = b.Created.Add(time.Second)
	changed, _ := json.Marshal(b)
	os.WriteFile(manifest, changed, 0600)
	if e = tickAdmitted(t, "replication", func() error { return d.tickReplicationSchedule(ctx, s.NextAt) }); e == nil {
		t.Fatal("selected manifest substitution accepted")
	}
	os.WriteFile(manifest, original, 0600)
	s = replicationScheduleCall(t, d, ReplicationScheduleParams{Action: "resume", AuthorizingMessage: "fresh"})
	if e = tickAdmitted(t, "replication", func() error { return d.tickReplicationSchedule(ctx, s.NextAt) }); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	if puts != 1 {
		t.Error("selected-before-outbox failed exact once", puts)
	}
	mu.Unlock()
}

// A scheduled upload deferred by host admission keeps its durable selection
// but consumes no run and no interval, like a park; a later tick uploads it
// exactly once.
func TestReplicationScheduleDeferredUploadConsumesNoRun(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	scheduleGrant(t, d, "grant")
	backup, e := admitted(t, "plane backup", func() (planeBackup, error) { return backupAdmitted(t, ctx, d) })
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	puts := 0
	var data []byte
	var receipt BackupReplicationReceipt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPut {
			puts++
			data, _ = io.ReadAll(r.Body)
			sum := sha256.Sum256(data)
			id := hex.EncodeToString(sum[:])
			receipt = BackupReplicationReceipt{ID: id, SHA256: id, ManifestSHA256: r.Header.Get("X-Fae-Manifest-SHA256"), Size: int64(len(data)), State: "stored"}
			json.NewEncoder(w).Encode(receipt)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/content") {
			w.Write(data)
			return
		}
		json.NewEncoder(w).Encode(receipt)
	}))
	defer server.Close()
	s := replicationScheduleCall(t, d, ReplicationScheduleParams{Action: "configure", Endpoint: server.URL, IntervalSeconds: 3600, AuthorizingMessage: "grant"})
	// Selection passes; the upload's own admission, once the run is claimed,
	// meets I/O pressure.
	var deferUpload sync.Map
	deferUpload.Store("on", true)
	d.live.guard.observe = func(ctx context.Context) (hostguard.Sample, error) {
		if on, _ := deferUpload.Load("on"); on == true {
			if current, e := d.readReplicationSchedule(ctx); e == nil && current.Running {
				return hostSample(20, 0.5), nil
			}
		}
		return hostSample(1, 0.5), nil
	}
	if reason, deferred := deferredRefusal(d.tickReplicationSchedule(ctx, s.NextAt)); !deferred || reason != "IOPressure" {
		t.Fatal("pressure did not defer the upload", reason)
	}
	after, e := d.readReplicationSchedule(ctx)
	if e != nil || after.State != "active" || after.Running || after.RunCount != 0 || !after.NextAt.Equal(s.NextAt) || after.Pending == nil || after.Pending.Backup != backup.Path || !strings.Contains(after.LastError, "host admission: IOPressure") {
		t.Fatal("deferred upload consumed a run or lost its selection", after, e)
	}
	deferUpload.Store("on", false)
	if e = d.tickReplicationSchedule(ctx, s.NextAt); e != nil {
		t.Fatal(e)
	}
	after, e = d.readReplicationSchedule(ctx)
	if e != nil || after.RunCount != 1 || after.Pending != nil || after.LastBackup != backup.Path || after.LastError != "" || !after.NextAt.After(s.NextAt) {
		t.Fatal("deferred upload not retried", after, e)
	}
	mu.Lock()
	defer mu.Unlock()
	if puts != 1 {
		t.Fatal("upload not exactly once", puts)
	}
}
