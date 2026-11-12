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
	for _, endpoint := range []string{"http://localhost:9", "http://example.com", "https://user:pass@example.com", "https://example.com?a=b", "file://remote-host/tmp/x", "https://example.com#fragment"} {
		if _, e := normalizeReplicationParams(BackupReplicationParams{Backup: "/backup", Endpoint: endpoint}); e == nil {
			t.Fatal(endpoint)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:9/", "http://[::1]:9", "https://example.com/backup", "file:///tmp/x", "/tmp/x", "ssh://backup-host/var/backups/fae"} {
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

func TestReplicaTargetParseAndLocalFileReplication(t *testing.T) {
	dir := t.TempDir()
	localDest := filepath.Join(dir, "replica")

	validTargets := []struct {
		raw        string
		wantScheme string
		wantUser   string
		wantHost   string
		wantPort   int
		wantPath   string
	}{
		{localDest, "file", "", "", 0, localDest},
		{"file://" + localDest, "file", "", "", 0, localDest},
		{"ssh://backup-host/var/backups/fae", "ssh", "", "backup-host", 0, "/var/backups/fae"},
		{"ssh://ops@backup-host:2222/var/backups/fae", "ssh", "ops", "backup-host", 2222, "/var/backups/fae"},
		{"scp://backup-user@backup-host/var/backups/fae", "scp", "backup-user", "backup-host", 0, "/var/backups/fae"},
	}
	for _, tc := range validTargets {
		got, err := ParseReplicaTarget(tc.raw)
		if err != nil {
			t.Fatalf("ParseReplicaTarget(%q) unexpected error: %v", tc.raw, err)
		}
		if got.Scheme != tc.wantScheme || got.User != tc.wantUser || got.Host != tc.wantHost || got.Port != tc.wantPort || got.Path != tc.wantPath {
			t.Fatalf("ParseReplicaTarget(%q) = %+v, want scheme=%s user=%s host=%s port=%d path=%s",
				tc.raw, got, tc.wantScheme, tc.wantUser, tc.wantHost, tc.wantPort, tc.wantPath)
		}
	}

	invalidTargets := []string{
		"",
		"relative/path",
		"file:///var/backups/../escape",
		"file://remote-host/var/backups",
		"ssh:///var/backups/fae",
		"ssh://user:password@backup-host/var/backups/fae",
		"ssh://-oProxyCommand=bad/var/backups/fae",
		"ssh://backup-host/var/backups/../escape",
		"ssh://backup-host/",
		"ftp://backup-host/var/backups/fae",
	}
	for _, raw := range invalidTargets {
		if _, err := ParseReplicaTarget(raw); err == nil {
			t.Fatalf("ParseReplicaTarget(%q) succeeded, want error", raw)
		}
	}

	// Verify file:///abs/path local encrypted bundle replication.
	bundlePath := filepath.Join(dir, "plane-20261104.tar.age")
	ciphertext := []byte("age-encryption.org/v1\n-> X25519 mock-key\n--- mock-hmac\nciphertext-payload-bytes\n")
	if err := os.WriteFile(bundlePath, ciphertext, 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "plane-20261104.manifest.json")
	if err := os.WriteFile(manifestPath, []byte(`{"instance":"default","encrypted":true}`), 0600); err != nil {
		t.Fatal(err)
	}

	receipt, err := ReplicateEncryptedBackup(context.Background(), bundlePath, manifestPath, "file://"+localDest)
	if err != nil {
		t.Fatalf("ReplicateEncryptedBackup(file://) failed: %v", err)
	}
	if !receipt.Verified || receipt.ReplicatedAt.IsZero() {
		t.Fatalf("file:// receipt not verified or missing ReplicatedAt: %+v", receipt)
	}
	wantSum := sha256.Sum256(ciphertext)
	if receipt.SHA256 != hex.EncodeToString(wantSum[:]) {
		t.Fatalf("receipt.SHA256 = %s, want %x", receipt.SHA256, wantSum)
	}
}

func TestOffHostSSHBackupReplicationAndRemoteDigestVerification(t *testing.T) {
	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	remoteRoot := filepath.Join(tmp, "remote_fs")
	logFile := filepath.Join(tmp, "ssh_scp.log")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(remoteRoot, 0700); err != nil {
		t.Fatal(err)
	}

	// Install mock scp executable in binDir.
	scpScript := `#!/usr/bin/env bash
set -euo pipefail
echo "scp $*" >> "` + logFile + `"
if [ "${1:-}" = "-P" ]; then
  shift 2
fi
src="$1"
dst="$2"
remote_path="${dst#*:}"
target_file="` + remoteRoot + `${remote_path}"
mkdir -p "$(dirname "$target_file")"
cp "$src" "$target_file"
if [ "${MOCK_CORRUPT_REMOTE:-0}" = "1" ] && [[ "$target_file" == *.age ]]; then
  printf "corrupted-bytes" >> "$target_file"
fi
`
	scpPath := filepath.Join(binDir, "scp")
	if err := os.WriteFile(scpPath, []byte(scpScript), 0755); err != nil {
		t.Fatal(err)
	}

	// Install mock ssh executable in binDir.
	sshScript := `#!/usr/bin/env bash
set -euo pipefail
echo "ssh $*" >> "` + logFile + `"
if [ "${1:-}" = "-p" ]; then
  shift 2
fi
host="$1"
shift
cmd="$1"
shift
case "$cmd" in
  mkdir)
    if [ "${1:-}" = "-p" ]; then
      shift
    fi
    mkdir -p "` + remoteRoot + `$1"
    ;;
  sha256sum)
    sha256sum "` + remoteRoot + `$1"
    ;;
  *)
    echo "unexpected remote command: $cmd" >&2
    exit 1
    ;;
esac
`
	sshPath := filepath.Join(binDir, "ssh")
	if err := os.WriteFile(sshPath, []byte(sshScript), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	bundlePath := filepath.Join(tmp, "backup-bundle.tar.age")
	ciphertext := []byte("age-encryption.org/v1\n-> X25519 recipient\n--- payload\nencrypted-offhost-backup-bytes\n")
	if err := os.WriteFile(bundlePath, ciphertext, 0600); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(tmp, "backup-bundle.manifest.json")
	manifestBytes := []byte(`{"bundle":"backup-bundle.tar.age","format":"age","version":1}`)
	if err := os.WriteFile(manifestPath, manifestBytes, 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Replicate over ssh://backup-host/var/backups/fae and verify remote SHA-256 via ssh ... sha256sum.
	receipt, err := ReplicateEncryptedBackup(context.Background(), bundlePath, manifestPath, "ssh://backup-host/var/backups/fae")
	if err != nil {
		t.Fatalf("ReplicateEncryptedBackup(ssh://backup-host/var/backups/fae) failed: %v", err)
	}
	if !receipt.Verified || receipt.ReplicatedAt.IsZero() {
		t.Fatalf("expected Verified=true and non-zero ReplicatedAt, got %+v", receipt)
	}
	wantBundleSum := sha256.Sum256(ciphertext)
	if receipt.SHA256 != hex.EncodeToString(wantBundleSum[:]) {
		t.Fatalf("receipt.SHA256 = %s, want %x", receipt.SHA256, wantBundleSum)
	}

	replicatedBundleOnDisk := filepath.Join(remoteRoot, "var", "backups", "fae", "backup-bundle.tar.age")
	gotRemoteBytes, err := os.ReadFile(replicatedBundleOnDisk)
	if err != nil {
		t.Fatalf("read replicated remote bundle: %v", err)
	}
	if !bytes.Equal(gotRemoteBytes, ciphertext) {
		t.Fatalf("remote bundle bytes mismatch")
	}

	rawLog, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	logText := string(rawLog)
	if !strings.Contains(logText, "scp ") || !strings.Contains(logText, "ssh backup-host sha256sum /var/backups/fae/backup-bundle.tar.age") {
		t.Fatalf("expected scp and ssh sha256sum invocations in log, got:\n%s", logText)
	}

	// 2. Also verify scp://user@backup-host:2222/var/backups/fae passes -P / -p port flags.
	receiptSCP, err := ReplicateEncryptedBackup(context.Background(), bundlePath, manifestPath, "scp://ops@backup-host:2222/var/backups/fae")
	if err != nil {
		t.Fatalf("ReplicateEncryptedBackup(scp://ops@backup-host:2222/var/backups/fae) failed: %v", err)
	}
	if !receiptSCP.Verified || receiptSCP.ReplicatedAt.IsZero() {
		t.Fatalf("scp:// receipt not verified: %+v", receiptSCP)
	}

	// 3. When remote sha256sum mismatches (corrupted transfer), replication fails and ReplicatedAt stays zero.
	t.Setenv("MOCK_CORRUPT_REMOTE", "1")
	badReceipt, err := ReplicateEncryptedBackup(context.Background(), bundlePath, manifestPath, "ssh://backup-host/var/backups/fae")
	if err == nil || !strings.Contains(err.Error(), "sha256sum mismatch") {
		t.Fatalf("corrupted remote bundle error = %v, want sha256sum mismatch", err)
	}
	if badReceipt.Verified || !badReceipt.ReplicatedAt.IsZero() {
		t.Fatalf("corrupted remote bundle must not mark Verified or ReplicatedAt: %+v", badReceipt)
	}
}

func TestBackupReplicationSSHRetryAndGenuineAgeEncryption(t *testing.T) {
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

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	remoteRoot := filepath.Join(tmp, "remote_fs")
	if err := os.MkdirAll(binDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(remoteRoot, 0700); err != nil {
		t.Fatal(err)
	}

	scpScript := `#!/usr/bin/env bash
set -euo pipefail
if [ "${MOCK_SCP_FAIL:-0}" = "1" ]; then
  echo "simulated transient scp failure" >&2
  exit 1
fi
if [ "${1:-}" = "-P" ]; then
  shift 2
fi
src="$1"
dst="$2"
remote_path="${dst#*:}"
target_file="` + remoteRoot + `${remote_path}"
mkdir -p "$(dirname "$target_file")"
cp "$src" "$target_file"
`
	if err := os.WriteFile(filepath.Join(binDir, "scp"), []byte(scpScript), 0755); err != nil {
		t.Fatal(err)
	}

	sshScript := `#!/usr/bin/env bash
set -euo pipefail
if [ "${1:-}" = "-p" ]; then
  shift 2
fi
host="$1"
shift
cmd="$1"
shift
case "$cmd" in
  mkdir)
    if [ "${1:-}" = "-p" ]; then
      shift
    fi
    mkdir -p "` + remoteRoot + `$1"
    ;;
  sha256sum)
    sha256sum "` + remoteRoot + `$1"
    ;;
  *)
    exit 1
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "ssh"), []byte(sshScript), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	p := BackupReplicationParams{
		Backup:             backup.Path,
		Endpoint:           "ssh://backup-host/var/backups/fae",
		AuthorizingMessage: "message",
	}

	// 1. Transient first-attempt scp failure leaves replication in "pending" state.
	t.Setenv("MOCK_SCP_FAIL", "1")
	if _, err := call(t, d, "BackupReplicate", p, ""); err == nil {
		t.Fatal("expected first-attempt transient scp failure to return error")
	}
	normalized, _ := normalizeReplicationParams(p)
	rawMarker, err := d.store.GetMarker(ctx, replicationKey(normalized))
	if err != nil {
		t.Fatal(err)
	}
	var pendingRec BackupReplicationRecord
	if err := json.Unmarshal(rawMarker, &pendingRec); err != nil {
		t.Fatal(err)
	}
	if pendingRec.State != "pending" {
		t.Fatalf("after first-attempt scp failure, State = %q, want pending", pendingRec.State)
	}

	// 2. Subsequent retry uploads the encrypted .age bundle, verifies remote SHA-256, and succeeds!
	t.Setenv("MOCK_SCP_FAIL", "0")
	repResp, err := call(t, d, "BackupReplicate", p, "")
	if err != nil {
		t.Fatalf("retry after transient scp failure failed: %v", err)
	}
	var confirmedRec BackupReplicationRecord
	if err := json.Unmarshal(repResp.Result, &confirmedRec); err != nil {
		t.Fatal(err)
	}
	if confirmedRec.State != "replicated" || confirmedRec.ReplicatedAt.IsZero() || confirmedRec.Receipt == nil {
		t.Fatalf("expected replicated state after retry, got %+v", confirmedRec)
	}

	// 3. Verify the uploaded <sha256>.age file on the remote host is genuinely encrypted (not raw tar)
	// and decrypts cleanly back to the deterministic tar archive.
	remoteAgePath := filepath.Join(remoteRoot, "var", "backups", "fae", confirmedRec.SHA256+".age")
	ageBytes, err := os.ReadFile(remoteAgePath)
	if err != nil {
		t.Fatalf("read remote .age bundle: %v", err)
	}
	if !bytes.HasPrefix(ageBytes, []byte("age-encryption.org/v1\n")) {
		t.Fatalf("remote .age bundle missing age-encryption.org/v1 header")
	}
	if bytes.Contains(ageBytes, []byte("verified.json")) {
		t.Fatalf("remote .age bundle contains unencrypted plaintext tar filenames")
	}
	decryptedTar, err := DecryptAgeBundle(remoteAgePath, confirmedRec.SHA256, confirmedRec.ManifestSHA256)
	if err != nil {
		t.Fatalf("DecryptAgeBundle failed: %v", err)
	}
	tarSum := sha256.Sum256(decryptedTar)
	if hex.EncodeToString(tarSum[:]) != confirmedRec.SHA256 {
		t.Fatalf("decrypted tar SHA256 = %x, want %s", tarSum, confirmedRec.SHA256)
	}

	// 4. Also verify file:// endpoint works end-to-end through BackupReplicate.
	localTargetDir := filepath.Join(tmp, "local_replica")
	fileParams := BackupReplicationParams{
		Backup:             backup.Path,
		Endpoint:           "file://" + localTargetDir,
		AuthorizingMessage: "message",
	}
	fileResp, err := call(t, d, "BackupReplicate", fileParams, "")
	if err != nil {
		t.Fatalf("BackupReplicate(file://) failed: %v", err)
	}
	var fileRec BackupReplicationRecord
	if err := json.Unmarshal(fileResp.Result, &fileRec); err != nil {
		t.Fatal(err)
	}
	if fileRec.State != "replicated" || fileRec.ReplicatedAt.IsZero() {
		t.Fatalf("expected file:// replication to succeed, got %+v", fileRec)
	}
}
