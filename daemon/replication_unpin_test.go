package daemon

import (
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
)

func TestReplicationUnpinNativeProofRestartAndClaims(t *testing.T) {
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
	backup, e := d.backupPlane(ctx)
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	puts := 0
	corrupt := false
	lost := false
	var payload []byte
	var receipt BackupReplicationReceipt
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPut {
			puts++
			payload, _ = io.ReadAll(r.Body)
			id := strings.TrimPrefix(r.URL.Path, "/backups/")
			receipt = BackupReplicationReceipt{ID: id, SHA256: id, ManifestSHA256: r.Header.Get("X-Fae-Manifest-SHA256"), Size: int64(len(payload)), State: "stored"}
			if lost {
				w.WriteHeader(503)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/content") {
			if corrupt {
				w.Write([]byte("bad"))
			} else {
				w.Write(payload)
			}
			return
		}
		json.NewEncoder(w).Encode(receipt)
	}))
	defer server.Close()
	rp := BackupReplicationParams{Backup: backup.Path, Endpoint: server.URL, AuthorizingMessage: "message"}
	if _, e = call(t, d, "BackupReplicate", rp, ""); e != nil {
		t.Fatal(e)
	}
	p := BackupReplicationUnpinParams{Backup: backup.Path, AuthorizingMessage: "message"}
	preview := func() BackupReplicationUnpinResult {
		t.Helper()
		r, e := call(t, d, "BackupReplicationUnpin", p, "")
		if e != nil {
			t.Fatal(e)
		}
		var result BackupReplicationUnpinResult
		if e = json.Unmarshal(r.Result, &result); e != nil {
			t.Fatal(e)
		}
		return result
	}
	plan := preview()
	if plan.Completed || plan.Outboxes != 1 {
		t.Fatal(plan)
	}
	restricted := p
	restricted.AuthorizingMessage = "restricted"
	d.store.RecordMessage(ctx, "restricted", `{"text":"only admit","authorize":{"verbs":["Admit"]}}`)
	if _, e = call(t, d, "BackupReplicationUnpin", restricted, ""); e == nil {
		t.Fatal("restricted grant accepted")
	}
	p.Apply = true
	p.PlanDigest = strings.Repeat("0", 64)
	if _, e = call(t, d, "BackupReplicationUnpin", p, ""); e == nil {
		t.Fatal("stale digest accepted")
	}
	p.PlanDigest = plan.PlanDigest
	mu.Lock()
	corrupt = true
	mu.Unlock()
	if _, e = call(t, d, "BackupReplicationUnpin", p, ""); e == nil {
		t.Fatal("corrupt remote accepted")
	}
	mu.Lock()
	corrupt = false
	mu.Unlock()
	// A second endpoint's uncertain outbox blocks even when the first is confirmed.
	normalized, _ := normalizeReplicationParams(rp)
	raw, e := d.store.GetMarker(ctx, replicationKey(normalized))
	if e != nil {
		t.Fatal(e)
	}
	var record BackupReplicationRecord
	json.Unmarshal(raw, &record)
	record.Endpoint = server.URL + "/other"
	record.State = "pending"
	record.Receipt = nil
	second := BackupReplicationParams{Backup: record.Backup, Endpoint: record.Endpoint, MaxBytes: record.MaxBytes}
	audit := controlplane.Audit{Actor: "operator", Action: "test", MessageID: "message", Outcome: "recorded"}
	if e = d.saveBackupReplication(ctx, replicationKey(second), record, audit); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "BackupReplicationUnpin", p, ""); e == nil {
		t.Fatal("uncertain other endpoint accepted")
	}
	// Restore a confirmed alias endpoint serving the same exact receipt.
	record.Endpoint = server.URL + "/other"
	record.State = "replicated"
	record.Receipt = &receipt
	if e = d.saveBackupReplication(ctx, replicationKey(second), record, audit); e != nil {
		t.Fatal(e)
	}
	p.Apply = false
	p.PlanDigest = ""
	plan = preview()
	if plan.Outboxes != 2 {
		t.Fatal(plan)
	}
	schedule := ReplicationScheduleRecord{NextAt: time.Now().Add(time.Hour), State: "paused", IntervalSeconds: 3600, Endpoint: server.URL, MaxBytes: normalized.MaxBytes, AuthorizingMessage: "message", Pending: &ReplicationSchedulePending{Backup: backup.Path, Endpoint: server.URL, MaxBytes: normalized.MaxBytes, ManifestDigest: record.ManifestSHA256}}
	if e = d.saveReplicationSchedule(ctx, schedule, audit); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "BackupReplicationUnpin", p, ""); e == nil {
		t.Fatal("paused claim accepted")
	}
	schedule.Pending = nil
	if e = d.saveReplicationSchedule(ctx, schedule, audit); e != nil {
		t.Fatal(e)
	}
	plan = preview()
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	p.Apply = true
	p.PlanDigest = plan.PlanDigest
	r, e := call(t, d, "BackupReplicationUnpin", p, "")
	if e != nil {
		t.Fatal(e)
	}
	var done BackupReplicationUnpinResult
	json.Unmarshal(r.Result, &done)
	if !done.Completed {
		t.Fatal(done)
	}
	pins, e := d.replicationProtectedBackups(ctx)
	if e != nil || pins[backup.Path] {
		t.Fatal(pins, e)
	}
	if _, e = d.readBackup(ctx, backup.Path); e != nil {
		t.Fatal("unpin deleted local backup", e)
	}
	if e = os.Rename(backup.Path, backup.Path+"-held"); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "BackupReplicate", rp, ""); e == nil {
		t.Fatal("missing source repinned")
	}
	if e = os.Rename(backup.Path+"-held", backup.Path); e != nil {
		t.Fatal(e)
	}
	manifestPath := filepath.Join(backup.Path, "verified.json")
	original, e := os.ReadFile(manifestPath)
	if e != nil {
		t.Fatal(e)
	}
	var changed planeBackup
	json.Unmarshal(original, &changed)
	changed.Created = changed.Created.Add(1)
	altered, _ := json.Marshal(changed)
	os.WriteFile(manifestPath, altered, 0600)
	if _, e = call(t, d, "BackupReplicate", rp, ""); e == nil {
		t.Fatal("substituted manifest repinned")
	}
	pins, e = d.replicationProtectedBackups(ctx)
	if e != nil || pins[backup.Path] {
		t.Fatal("failed confirmation recreated pin", e)
	}
	os.WriteFile(manifestPath, original, 0600)
	if _, e = call(t, d, "BackupReplicate", rp, ""); e != nil {
		t.Fatal(e)
	}
	pins, e = d.replicationProtectedBackups(ctx)
	if e != nil || !pins[backup.Path] {
		t.Fatal("confirmed retry did not repin", e)
	}
	mu.Lock()
	defer mu.Unlock()
	if puts != 1 {
		t.Fatal("unpin replayed upload", puts)
	}
}
