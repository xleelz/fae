package daemon

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
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

	"github.com/xleelz/fae/api"
)

func TestBackupReplicationGuardAndEndpoint(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			if CanConfirmBackupReplication(a, b) != (a && b) {
				t.Fatal(a, b)
			}
		}
	}
	for _, endpoint := range []string{"http://localhost:9", "http://example.com", "https://user:pass@example.com", "https://example.com?a=b", "file:///tmp/x", "https://example.com#fragment"} {
		if _, e := normalizeReplicationParams(BackupReplicationParams{Backup: "/backup", Endpoint: endpoint}); e == nil {
			t.Fatal(endpoint)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:9/", "http://[::1]:9", "https://example.com/backup"} {
		if _, e := normalizeReplicationParams(BackupReplicationParams{Backup: "/backup", Endpoint: endpoint}); e != nil {
			t.Fatal(endpoint, e)
		}
	}
}
func replicationFixtureBackup(t *testing.T, d *Daemon) string {
	t.Helper()
	path := filepath.Join(d.layout.Root, "backup", "plane-replication-test")
	if e := os.MkdirAll(path, 0700); e != nil {
		t.Fatal(e)
	}
	data := []byte("immutable backup content\n")
	if e := os.WriteFile(filepath.Join(path, "data"), data, 0600); e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(data)
	manifest := planeBackup{Path: path, Instance: d.config.Instance, Workspace: d.config.Workspace, Created: time.Unix(100, 0).UTC(), Files: map[string]string{"data": hex.EncodeToString(sum[:])}, Jobs: []backupJob{}}
	raw, _ := json.Marshal(manifest)
	if e := os.WriteFile(filepath.Join(path, "verified.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	return path
}
func TestBackupReplicationArchiveDeterministicBounded(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	d := &Daemon{layout: Layout{Root: root}, config: Config{Instance: "test", Workspace: "/workspace"}}
	p := BackupReplicationParams{Backup: replicationFixtureBackup(t, d), Endpoint: "https://example.com", MaxBytes: 1 << 20}
	a, r, e := d.prepareReplicationArchive(context.Background(), p)
	if e != nil {
		t.Fatal(e)
	}
	defer os.Remove(a.Name())
	defer a.Close()
	b, r2, e := d.prepareReplicationArchive(context.Background(), p)
	if e != nil {
		t.Fatal(e)
	}
	defer os.Remove(b.Name())
	defer b.Close()
	if r.SHA256 != r2.SHA256 || r.Size != r2.Size {
		t.Fatal("nondeterministic archive")
	}
	tr := tar.NewReader(a)
	names := []string{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		names = append(names, h.Name)
		if h.Typeflag != tar.TypeReg || h.Uid != 0 || h.Gid != 0 || h.Mode != 0600 {
			t.Fatal(h)
		}
	}
	if strings.Join(names, ",") != "data,verified.json" {
		t.Fatal(names)
	}
	p.MaxBytes = 1024
	if _, _, e = d.prepareReplicationArchive(context.Background(), p); e == nil {
		t.Fatal("bound ignored")
	}
	p.MaxBytes = 1 << 20
	manifest := filepath.Join(p.Backup, "verified.json")
	saved := filepath.Join(root, "saved.json")
	os.Rename(manifest, saved)
	os.Symlink(saved, manifest)
	if _, _, e = d.prepareReplicationArchive(context.Background(), p); e == nil {
		t.Fatal("manifest symlink accepted")
	}
	os.Remove(manifest)
	os.Rename(saved, manifest)
	os.WriteFile(filepath.Join(p.Backup, "data"), []byte("changed"), 0600)
	if _, _, e = d.prepareReplicationArchive(context.Background(), p); e == nil {
		t.Fatal("corrupt backup uploaded")
	}
}
func TestBackupReplicationNativeLostAckRestartAndProofs(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
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
	response, err := call(t, d, "PlaneBackup", CleanupParams{AuthorizingMessage: "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var backup planeBackup
	if err = json.Unmarshal(response.Result, &backup); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	puts, queries, fetches := 0, 0, 0
	var payload []byte
	var proof BackupReplicationReceipt
	mode := "lost-ack"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPut {
			puts++
			payload, _ = io.ReadAll(r.Body)
			sum := sha256.Sum256(payload)
			id := hex.EncodeToString(sum[:])
			if r.Header.Get("Idempotency-Key") != id || r.URL.Path != "/backups/"+id || r.ContentLength != int64(len(payload)) {
				t.Error("upload identity differs")
			}
			proof = BackupReplicationReceipt{ID: id, SHA256: id, ManifestSHA256: r.Header.Get("X-Fae-Manifest-SHA256"), Size: int64(len(payload)), State: "stored"}
			w.WriteHeader(503)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/content") {
			fetches++
			if mode == "corrupt-content" {
				w.Write(bytes.Repeat([]byte("x"), len(payload)))
				return
			}
			if mode == "oversized-content" {
				w.Write(append(payload, 1))
				return
			}
			w.Write(payload)
			return
		}
		queries++
		if mode == "absent" {
			w.WriteHeader(404)
			return
		}
		v := proof
		if mode == "corrupt-proof" {
			v.ManifestSHA256 = strings.Repeat("0", 64)
		}
		json.NewEncoder(w).Encode(v)
	}))
	defer server.Close()
	p := BackupReplicationParams{Backup: backup.Path, Endpoint: server.URL, AuthorizingMessage: "message"}
	// Delegated workflow credentials cannot invoke the operator boundary.
	operatorToken := make([]byte, 32)
	roleToken := make([]byte, 32)
	roleToken[0] = 9
	authServer, e := api.New(operatorToken, d.backupReplicateRPC)
	if e != nil {
		t.Fatal(e)
	}
	if e = authServer.AddPrincipal(roleToken, api.Principal{Actor: "workflow-role", Task: "task"}); e != nil {
		t.Fatal(e)
	}
	if e = authServer.AddPrincipal(bytes.Repeat([]byte{8}, 32), api.Principal{Actor: "operator", Task: "task"}); e == nil {
		t.Fatal("task-scoped operator capability accepted")
	}
	params, _ := json.Marshal(p)
	request, _ := json.Marshal(api.Request{Version: 1, Method: "BackupReplicate", Params: params})
	for _, token := range [][]byte{roleToken} {
		req := httptest.NewRequest(http.MethodPost, "/v1/rpc", bytes.NewReader(request))
		req.Header.Set("Authorization", hex.EncodeToString(token))
		out := httptest.NewRecorder()
		authServer.ServeHTTP(out, req)
		var answer api.Response
		if e = json.Unmarshal(out.Body.Bytes(), &answer); e != nil {
			t.Fatal(e)
		}
		if answer.Error == "" {
			t.Fatal("task scoped request accepted")
		}
	}

	for _, grant := range []string{"missing", "restricted"} {
		if grant == "restricted" {
			d.store.RecordMessage(ctx, grant, `{"text":"task only","authorize":{"verbs":["Admit"]}}`)
		}
		bad := p
		bad.AuthorizingMessage = grant
		if _, err = call(t, d, "BackupReplicate", bad, ""); err == nil {
			t.Fatal("unauthorized replication")
		}
	}
	mu.Lock()
	unauthorizedPuts := puts
	mu.Unlock()
	if unauthorizedPuts != 0 {
		t.Fatal("unauthorized upload", unauthorizedPuts)
	}
	normalizedPre, _ := normalizeReplicationParams(p)
	if _, e := d.store.GetMarker(ctx, replicationKey(normalizedPre)); e == nil {
		t.Fatal("unauthorized durable intent")
	}
	if _, err = call(t, d, "BackupReplicate", p, ""); err == nil {
		t.Fatal("lost acknowledgment reported success")
	}
	normalized, _ := normalizeReplicationParams(p)
	raw, err := d.store.GetMarker(ctx, replicationKey(normalized))
	if err != nil {
		t.Fatal(err)
	}
	var record BackupReplicationRecord
	json.Unmarshal(raw, &record)
	if record.State != "pending" || record.SHA256 == "" {
		t.Fatal(record)
	}
	protected, err := d.replicationProtectedBackups(ctx)
	if err != nil || !protected[backup.Path] {
		t.Fatal("source not protected", err)
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"absent", "corrupt-proof", "corrupt-content", "oversized-content"} {
		mu.Lock()
		mode = bad
		mu.Unlock()
		if _, err = call(t, d, "BackupReplicate", p, ""); err == nil {
			t.Fatal("bad remote proof accepted", bad)
		}
	}
	mu.Lock()
	mode = "valid"
	mu.Unlock()
	for n := 0; n < 2; n++ {
		response, err = call(t, d, "BackupReplicate", p, "")
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(response.Result, &record)
		if record.State != "replicated" || record.Receipt == nil {
			t.Fatal(record)
		}
	}
	mu.Lock()
	if puts != 1 || queries != 6 || fetches != 4 {
		t.Error("unexpected transfer counts", puts, queries, fetches)
	}
	mu.Unlock()
	changed := p
	changed.MaxBytes = 1 << 20
	if _, err = call(t, d, "BackupReplicate", changed, ""); err == nil {
		t.Fatal("request drift accepted")
	}
	// A completed repeat still verifies actual remote bytes.
	mu.Lock()
	mode = "corrupt-content"
	mu.Unlock()
	if _, err = call(t, d, "BackupReplicate", p, ""); err == nil {
		t.Fatal("confirmed stale content accepted")
	}
}
