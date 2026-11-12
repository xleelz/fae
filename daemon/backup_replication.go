package daemon

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

const maxReplicationBytes int64 = 1 << 30

type BackupReplicationParams struct {
	Backup             string `json:"backup"`
	Endpoint           string `json:"endpoint"`
	MaxBytes           int64  `json:"max_bytes,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}
type BackupReplicationReceipt struct {
	ID             string `json:"id"`
	SHA256         string `json:"sha256"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Size           int64  `json:"size"`
	State          string `json:"state"`
}
type BackupReplicationRecord struct {
	Backup         string                    `json:"backup"`
	Endpoint       string                    `json:"endpoint"`
	SHA256         string                    `json:"sha256"`
	ManifestSHA256 string                    `json:"manifest_sha256"`
	Size           int64                     `json:"size"`
	MaxBytes       int64                     `json:"max_bytes"`
	State          string                    `json:"state"`
	ReplicatedAt   time.Time                 `json:"replicated_at,omitempty"`
	Receipt        *BackupReplicationReceipt `json:"receipt,omitempty"`
}

func replicationKey(p BackupReplicationParams) string {
	raw, _ := json.Marshal([]string{p.Endpoint, p.Backup})
	return "backup-replication/" + controlplane.Digest(raw)
}

func isReplicaTargetEndpoint(endpoint string) bool {
	trimmed := strings.TrimSpace(endpoint)
	return strings.HasPrefix(trimmed, "ssh://") ||
		strings.HasPrefix(trimmed, "scp://") ||
		strings.HasPrefix(trimmed, "file://") ||
		(!strings.Contains(trimmed, "://") && strings.HasPrefix(trimmed, "/"))
}

func normalizeReplicationParams(p BackupReplicationParams) (BackupReplicationParams, error) {
	p.Endpoint = strings.TrimRight(p.Endpoint, "/")
	if isReplicaTargetEndpoint(p.Endpoint) {
		if _, err := ParseReplicaTarget(p.Endpoint); err != nil {
			return p, err
		}
	} else {
		u, err := url.Parse(p.Endpoint)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
			return p, errors.New("replication requires exact HTTPS endpoint")
		}
		if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
			return p, errors.New("replication HTTP requires literal loopback")
		}
	}
	if p.Backup == "" || !filepath.IsAbs(p.Backup) || filepath.Clean(p.Backup) != p.Backup {
		return p, errors.New("replication requires absolute owned backup path")
	}
	if p.MaxBytes == 0 {
		p.MaxBytes = 256 << 20
	}
	if p.MaxBytes < 1024 || p.MaxBytes > maxReplicationBytes {
		return p, errors.New("replication max_bytes must be 1024..1073741824")
	}
	return p, nil
}
func (d *Daemon) backupReplicateRPC(ctx context.Context, r api.Request) (any, error) {
	var p BackupReplicationParams
	if err := decode(r.Params, &p); err != nil {
		return nil, err
	}
	principal, ok := api.AuthenticatedPrincipal(ctx)
	if !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("backup replication requires instance operator")
	}
	if err := d.store.AuthorizeReadOnlyEffect(ctx, p.AuthorizingMessage, principal.Actor, r.Method, "runtime", actionClasses()); err != nil {
		return nil, err
	}
	var err error
	ctx, err = d.store.AuthorizeAction(ctx, p.AuthorizingMessage, principal.Actor, r.Method, "runtime", false, actionClasses())
	if err != nil {
		return nil, err
	}
	audit := controlplane.Audit{Actor: principal.Actor, Action: r.Method, MessageID: p.AuthorizingMessage, Inputs: r.Params, Outcome: "pending"}
	return d.backupReplicate(ctx, p, audit)
}
func (d *Daemon) backupReplicate(ctx context.Context, p BackupReplicationParams, audit controlplane.Audit) (BackupReplicationRecord, error) {
	var record BackupReplicationRecord
	var err error
	if p, err = normalizeReplicationParams(p); err != nil {
		return record, err
	}
	for !d.backupMu.TryLock() {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return record, ctx.Err()
		case <-timer.C:
		}
	}
	defer d.backupMu.Unlock()
	if d.live != nil && d.live.guard != nil {
		sid := "backup-replication/" + controlplane.Digest([]byte(p.Backup))
		release, e := d.live.guard.AcquireHostIO(ctx, sid)
		if e != nil {
			return record, e
		}
		defer release()
	}
	if err = d.validateScheduledReplication(ctx, p, nil); err != nil {
		return record, err
	}
	key := replicationKey(p)
	raw, readErr := d.store.GetMarker(ctx, key)
	fresh := errors.Is(readErr, sql.ErrNoRows)
	var archive *os.File
	if fresh {
		archive, record, err = d.prepareReplicationArchive(ctx, p)
		if err != nil {
			return record, err
		}
		defer func() { archive.Close(); os.Remove(archive.Name()) }()
		if err = d.validateScheduledReplication(ctx, p, &record); err != nil {
			return record, err
		}
		// Pending is the write-ahead attempted marker. Once durable, only this call
		// may PUT; all retries query because absence cannot disprove a late upload.
		if err = d.protectReplicationSource(ctx, p.Backup, audit); err != nil {
			return record, err
		}
		if err = d.saveBackupReplication(ctx, key, record, audit); err != nil {
			return record, err
		}
	} else {
		if readErr != nil {
			return record, readErr
		}
		if err = decode(raw, &record); err != nil {
			return record, err
		}
		if record.Backup != p.Backup || record.Endpoint != p.Endpoint || record.MaxBytes != p.MaxBytes || record.Size <= 0 || record.Size > p.MaxBytes || !replicationDigest(record.SHA256) || !replicationDigest(record.ManifestSHA256) || (record.State != "pending" && record.State != "replicated") {
			return record, errors.New("immutable replication request or intent differs")
		}
	}
	if err = d.validateScheduledReplication(ctx, p, &record); err != nil {
		return record, err
	}
	if isReplicaTargetEndpoint(p.Endpoint) {
		target, parseErr := ParseReplicaTarget(p.Endpoint)
		if parseErr != nil {
			return record, parseErr
		}
		if archive == nil {
			var prepRecord BackupReplicationRecord
			archive, prepRecord, err = d.prepareReplicationArchive(ctx, p)
			if err != nil {
				return record, err
			}
			defer func() { archive.Close(); os.Remove(archive.Name()) }()
			if prepRecord.SHA256 != record.SHA256 || prepRecord.ManifestSHA256 != record.ManifestSHA256 || prepRecord.Size != record.Size {
				return record, errors.New("replication retry archive differs from durable pending intent")
			}
		}
		bundlePath := archive.Name()
		manifestPath := filepath.Join(p.Backup, "verified.json")
		remoteRec, repErr := target.replicateArchiveOrVerify(ctx, bundlePath, manifestPath, record)
		if repErr != nil {
			return record, fmt.Errorf("replication uncertain; explicit retry queries remote proof: %w", repErr)
		}
		localManifest, localErr := d.scheduleManifest(ctx, p.Backup)
		if localErr != nil {
			return record, localErr
		}
		if localManifest != record.ManifestSHA256 {
			return record, errors.New("replication local manifest differs before pin")
		}
		if err = d.protectReplicationSource(ctx, p.Backup, audit); err != nil {
			return record, err
		}
		receipt := BackupReplicationReceipt{
			ID:             record.SHA256,
			SHA256:         record.SHA256,
			ManifestSHA256: record.ManifestSHA256,
			Size:           record.Size,
			State:          "stored",
		}
		record.Receipt = &receipt
		record.State = "replicated"
		record.ReplicatedAt = remoteRec.ReplicatedAt
		audit.Outcome = "replicated"
		if err = d.saveBackupReplication(ctx, key, record, audit); err != nil {
			return record, err
		}
		return record, nil
	}
	receipt, err := replicationRemoteReceipt(ctx, record, archive, fresh)
	if err != nil {
		return record, fmt.Errorf("replication uncertain; explicit retry queries remote proof: %w", err)
	}
	identity := receipt.ID == record.SHA256 && receipt.SHA256 == record.SHA256 && receipt.ManifestSHA256 == record.ManifestSHA256 && receipt.Size == record.Size && receipt.State == "stored"
	if !identity {
		return record, errors.New("remote replication receipt identity differs")
	}
	if err = d.validateScheduledReplication(ctx, p, &record); err != nil {
		return record, err
	}
	contentErr := replicationRemoteContent(ctx, record)
	if !CanConfirmBackupReplication(identity, contentErr == nil) {
		return record, fmt.Errorf("remote archive proof failed: %w", contentErr)
	}
	// Explicit confirmation restores a source pin only for the same verified local backup.
	localManifest, localErr := d.scheduleManifest(ctx, p.Backup)
	if localErr != nil {
		return record, localErr
	}
	if localManifest != record.ManifestSHA256 {
		return record, errors.New("replication local manifest differs before pin")
	}
	if err = d.protectReplicationSource(ctx, p.Backup, audit); err != nil {
		return record, err
	}
	record.Receipt = &receipt
	record.State = "replicated"
	record.ReplicatedAt = time.Now().UTC()
	audit.Outcome = "replicated"
	if err = d.saveBackupReplication(ctx, key, record, audit); err != nil {
		return record, err
	}
	return record, nil
}
func replicationDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && s == strings.ToLower(s)
}
func (d *Daemon) saveBackupReplication(ctx context.Context, key string, r BackupReplicationRecord, a controlplane.Audit) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return d.store.PutMarker(ctx, a, key, raw)
}
func replicationClient() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second}, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("replication redirects refused") }}
}
func replicationRemoteReceipt(ctx context.Context, r BackupReplicationRecord, archive *os.File, upload bool) (BackupReplicationReceipt, error) {
	var receipt BackupReplicationReceipt
	method := http.MethodGet
	var body io.Reader
	if upload {
		method = http.MethodPut
		body = archive
	}
	req, err := http.NewRequestWithContext(ctx, method, r.Endpoint+"/backups/"+r.SHA256, body)
	if err != nil {
		return receipt, err
	}
	if upload {
		req.ContentLength = r.Size
		req.GetBody = nil
		req.Header.Set("Content-Type", "application/x-tar")
		req.Header.Set("Idempotency-Key", r.SHA256)
		req.Header.Set("X-Fae-Manifest-SHA256", r.ManifestSHA256)
	}
	resp, err := replicationClient().Do(req)
	if err != nil {
		return receipt, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && !(upload && resp.StatusCode == 201) {
		return receipt, fmt.Errorf("remote backup status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16385))
	if err != nil {
		return receipt, err
	}
	if len(data) > 16384 {
		return receipt, errors.New("remote receipt exceeds bound")
	}
	if err = decode(data, &receipt); err != nil {
		return receipt, err
	}
	return receipt, nil
}
func replicationRemoteContent(ctx context.Context, r BackupReplicationRecord) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Endpoint+"/backups/"+r.SHA256+"/content", nil)
	if err != nil {
		return err
	}
	resp, err := replicationClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("remote content status %d", resp.StatusCode)
	}
	if resp.ContentLength >= 0 && resp.ContentLength != r.Size {
		return errors.New("remote content length differs")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(resp.Body, r.Size+1))
	if err != nil {
		return err
	}
	if n != r.Size || hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
		return errors.New("remote content size or checksum differs")
	}
	return nil
}

// Sources are retained conservatively, including an interrupted first intent
// write. The index is committed before that intent while holding backupMu.
func (d *Daemon) replicationProtectedBackups(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	raw, err := d.store.GetMarker(ctx, "backup-replication-sources")
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	var paths []string
	if err = decode(raw, &paths); err != nil {
		return nil, err
	}
	if len(paths) > 10000 {
		return nil, errors.New("replication source index exceeds bound")
	}
	for _, path := range paths {
		if filepath.Dir(path) != filepath.Join(d.layout.Root, "backup") || filepath.Clean(path) != path || !strings.HasPrefix(filepath.Base(path), "plane-") {
			return nil, errors.New("foreign replication source index entry")
		}
		if out[path] {
			return nil, errors.New("duplicate replication source index entry")
		}
		out[path] = true
	}
	return out, nil
}
func (d *Daemon) protectReplicationSource(ctx context.Context, path string, audit controlplane.Audit) error {
	sources, err := d.replicationProtectedBackups(ctx)
	if err != nil {
		return err
	}
	if sources[path] {
		return nil
	}
	if len(sources) >= 10000 {
		return errors.New("replication source index full")
	}
	sources[path] = true
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	raw, err := json.Marshal(paths)
	if err != nil {
		return err
	}
	return d.store.PutMarker(ctx, audit, "backup-replication-sources", raw)
}

// ReplicaTarget represents a validated backup replication destination (FAE-GEN-010 / FAE-SEC-006):
//   - local filesystem path ("/abs/path") or "file:///abs/path"
//   - off-host SSH/SCP target ("ssh://[user@]host[:port]/abs/path" or "scp://[user@]host[:port]/abs/path")
type ReplicaTarget struct {
	Scheme    string `json:"scheme"`
	User      string `json:"user,omitempty"`
	Host      string `json:"host,omitempty"`
	Port      int    `json:"port,omitempty"`
	Path      string `json:"path"`
	Raw       string `json:"raw"`
	SSHBinary string `json:"-"`
	SCPBinary string `json:"-"`
}

// ReplicaBundleReceipt records the outcome of replicating an encrypted .age bundle
// and its metadata manifest to a ReplicaTarget.
type ReplicaBundleReceipt struct {
	Target         string    `json:"target"`
	Scheme         string    `json:"scheme"`
	Host           string    `json:"host,omitempty"`
	RemoteDir      string    `json:"remote_dir"`
	BundleName     string    `json:"bundle_name"`
	BundlePath     string    `json:"bundle_path"`
	ManifestName   string    `json:"manifest_name"`
	ManifestPath   string    `json:"manifest_path"`
	SHA256         string    `json:"sha256"`
	ManifestSHA256 string    `json:"manifest_sha256"`
	Size           int64     `json:"size"`
	Verified       bool      `json:"verified"`
	ReplicatedAt   time.Time `json:"replicated_at"`
}

// ParseReplicaTarget parses and validates a local path, file:///abs/path,
// ssh://[user@]host[:port]/abs/path, or scp://[user@]host[:port]/abs/path target.
func ParseReplicaTarget(raw string) (ReplicaTarget, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ReplicaTarget{}, errors.New("replica target is required")
	}

	if !strings.Contains(trimmed, "://") {
		if !filepath.IsAbs(trimmed) || hasDotDotSegment(trimmed) {
			return ReplicaTarget{}, fmt.Errorf("local replica target %q must be a clean absolute path", raw)
		}
		clean := filepath.Clean(trimmed)
		if clean == "/" {
			return ReplicaTarget{}, errors.New("local replica target cannot be root /")
		}
		return ReplicaTarget{
			Scheme: "file",
			Path:   clean,
			Raw:    trimmed,
		}, nil
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return ReplicaTarget{}, fmt.Errorf("invalid replica target URI %q: %w", raw, err)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return ReplicaTarget{}, fmt.Errorf("replica target URI %q must not contain query or fragment", raw)
	}

	switch u.Scheme {
	case "file":
		if u.User != nil || (u.Host != "" && u.Host != "localhost") {
			return ReplicaTarget{}, fmt.Errorf("file:// replica target %q must not specify remote host or user", raw)
		}
		if !filepath.IsAbs(u.Path) || hasDotDotSegment(u.Path) {
			return ReplicaTarget{}, fmt.Errorf("file:// replica target %q requires a clean absolute path", raw)
		}
		clean := filepath.Clean(u.Path)
		if clean == "/" {
			return ReplicaTarget{}, errors.New("file:// replica target cannot be root /")
		}
		return ReplicaTarget{
			Scheme: "file",
			Path:   clean,
			Raw:    trimmed,
		}, nil

	case "ssh", "scp":
		host := strings.TrimSpace(u.Hostname())
		if host == "" || strings.HasPrefix(host, "-") {
			return ReplicaTarget{}, fmt.Errorf("%s:// replica target %q requires a valid host", u.Scheme, raw)
		}
		var user string
		if u.User != nil {
			if _, hasPassword := u.User.Password(); hasPassword {
				return ReplicaTarget{}, fmt.Errorf("%s:// replica target %q must not embed passwords", u.Scheme, raw)
			}
			user = strings.TrimSpace(u.User.Username())
			if strings.HasPrefix(user, "-") {
				return ReplicaTarget{}, fmt.Errorf("%s:// replica target %q has invalid user %q", u.Scheme, raw, user)
			}
		}
		var port int
		if portStr := u.Port(); portStr != "" {
			p, pErr := strconv.Atoi(portStr)
			if pErr != nil || p < 1 || p > 65535 {
				return ReplicaTarget{}, fmt.Errorf("%s:// replica target %q has invalid port %q", u.Scheme, raw, portStr)
			}
			port = p
		}
		if !filepath.IsAbs(u.Path) || hasDotDotSegment(u.Path) {
			return ReplicaTarget{}, fmt.Errorf("%s:// replica target %q requires a clean absolute remote path", u.Scheme, raw)
		}
		clean := filepath.Clean(u.Path)
		if clean == "/" {
			return ReplicaTarget{}, fmt.Errorf("%s:// replica target %q cannot target root /", u.Scheme, raw)
		}
		return ReplicaTarget{
			Scheme: u.Scheme,
			User:   user,
			Host:   host,
			Port:   port,
			Path:   clean,
			Raw:    trimmed,
		}, nil

	default:
		return ReplicaTarget{}, fmt.Errorf("unsupported replica target scheme %q in %q (want file, ssh, or scp)", u.Scheme, raw)
	}
}

func hasDotDotSegment(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if part == ".." {
			return true
		}
	}
	return false
}

// IsRemote reports whether the target replicates off-host over SSH/SCP.
func (t ReplicaTarget) IsRemote() bool {
	return t.Scheme == "ssh" || t.Scheme == "scp"
}

// RemoteHostSpec returns "[user@]host" for SSH/SCP commands.
func (t ReplicaTarget) RemoteHostSpec() string {
	if t.User != "" {
		return t.User + "@" + t.Host
	}
	return t.Host
}

// ReplicateEncryptedBackup parses targetURI and replicates the encrypted .age/ciphertext
// bundle and metadata manifest to the target, verifying the remote SHA-256 digest
// before marking ReplicatedAt.
func ReplicateEncryptedBackup(ctx context.Context, bundlePath string, manifestPath string, targetURI string) (ReplicaBundleReceipt, error) {
	target, err := ParseReplicaTarget(targetURI)
	if err != nil {
		return ReplicaBundleReceipt{}, err
	}
	return target.Replicate(ctx, bundlePath, manifestPath)
}

// Replicate transfers the encrypted .age/ciphertext bundle and metadata manifest to t
// and verifies the destination SHA-256 digest before setting ReplicatedAt.
func (t ReplicaTarget) Replicate(ctx context.Context, bundlePath string, manifestPath string) (ReplicaBundleReceipt, error) {
	if strings.TrimSpace(bundlePath) == "" {
		return ReplicaBundleReceipt{}, errors.New("encrypted backup bundle path is required")
	}
	bundleSHA, bundleSize, err := hashRegularFile(bundlePath)
	if err != nil {
		return ReplicaBundleReceipt{}, fmt.Errorf("read encrypted bundle %s: %w", bundlePath, err)
	}
	bundleName := filepath.Base(bundlePath)

	actualManifestPath := strings.TrimSpace(manifestPath)
	cleanupManifest := func() {}
	if actualManifestPath == "" {
		candidate := bundlePath + ".manifest.json"
		if info, statErr := os.Stat(candidate); statErr == nil && info.Mode().IsRegular() {
			actualManifestPath = candidate
		} else {
			tmpFile, tmpErr := os.CreateTemp("", "fae-backup-manifest-*.json")
			if tmpErr != nil {
				return ReplicaBundleReceipt{}, tmpErr
			}
			tmpName := tmpFile.Name()
			cleanupManifest = func() { _ = os.Remove(tmpName) }
			manifestBody, _ := json.Marshal(map[string]any{
				"bundle":     bundleName,
				"sha256":     bundleSHA,
				"size":       bundleSize,
				"created_at": time.Now().UTC().Format(time.RFC3339),
			})
			_, writeErr := tmpFile.Write(manifestBody)
			closeErr := tmpFile.Close()
			if writeErr != nil || closeErr != nil {
				cleanupManifest()
				return ReplicaBundleReceipt{}, errors.Join(writeErr, closeErr)
			}
			actualManifestPath = tmpName
		}
	}
	defer cleanupManifest()

	manifestSHA, _, err := hashRegularFile(actualManifestPath)
	if err != nil {
		return ReplicaBundleReceipt{}, fmt.Errorf("read backup manifest %s: %w", actualManifestPath, err)
	}
	manifestName := filepath.Base(actualManifestPath)
	if strings.HasPrefix(manifestName, "fae-backup-manifest-") {
		manifestName = bundleName + ".manifest.json"
	}

	destBundlePath := filepath.Join(t.Path, bundleName)
	destManifestPath := filepath.Join(t.Path, manifestName)

	receipt := ReplicaBundleReceipt{
		Target:         t.Raw,
		Scheme:         t.Scheme,
		Host:           t.Host,
		RemoteDir:      t.Path,
		BundleName:     bundleName,
		BundlePath:     destBundlePath,
		ManifestName:   manifestName,
		ManifestPath:   destManifestPath,
		SHA256:         bundleSHA,
		ManifestSHA256: manifestSHA,
		Size:           bundleSize,
	}

	switch t.Scheme {
	case "file":
		if err := os.MkdirAll(t.Path, 0700); err != nil {
			return receipt, fmt.Errorf("create local replica directory %s: %w", t.Path, err)
		}
		if err := copyFileAtomic(bundlePath, destBundlePath, 0600); err != nil {
			return receipt, fmt.Errorf("copy encrypted bundle to %s: %w", destBundlePath, err)
		}
		if err := copyFileAtomic(actualManifestPath, destManifestPath, 0600); err != nil {
			return receipt, fmt.Errorf("copy manifest to %s: %w", destManifestPath, err)
		}
		gotBundleSHA, gotSize, err := hashRegularFile(destBundlePath)
		if err != nil {
			return receipt, fmt.Errorf("verify local replicated bundle: %w", err)
		}
		if gotBundleSHA != bundleSHA || gotSize != bundleSize {
			return receipt, fmt.Errorf("local replica digest mismatch: got %s (%d bytes), want %s (%d bytes)", gotBundleSHA, gotSize, bundleSHA, bundleSize)
		}
		gotManifestSHA, _, err := hashRegularFile(destManifestPath)
		if err != nil || gotManifestSHA != manifestSHA {
			return receipt, fmt.Errorf("local replica manifest digest mismatch: got %s, want %s", gotManifestSHA, manifestSHA)
		}
		receipt.Verified = true
		receipt.ReplicatedAt = time.Now().UTC()
		return receipt, nil

	case "ssh", "scp":
		sshBin, scpBin, err := t.resolveSSHAndSCP()
		if err != nil {
			return receipt, err
		}
		hostSpec := t.RemoteHostSpec()

		// 1. Ensure destination directory exists on the remote host.
		mkdirArgs := t.sshArgs(hostSpec, "mkdir", "-p", t.Path)
		if out, err := exec.CommandContext(ctx, sshBin, mkdirArgs...).CombinedOutput(); err != nil {
			return receipt, fmt.Errorf("ssh mkdir -p %s on %s failed: %w (%s)", t.Path, hostSpec, err, strings.TrimSpace(string(out)))
		}

		// 2. Replicate the encrypted .age/ciphertext bundle and metadata manifest over scp.
		scpBundleArgs := t.scpArgs(bundlePath, hostSpec+":"+destBundlePath)
		if out, err := exec.CommandContext(ctx, scpBin, scpBundleArgs...).CombinedOutput(); err != nil {
			return receipt, fmt.Errorf("scp bundle to %s:%s failed: %w (%s)", hostSpec, destBundlePath, err, strings.TrimSpace(string(out)))
		}
		scpManifestArgs := t.scpArgs(actualManifestPath, hostSpec+":"+destManifestPath)
		if out, err := exec.CommandContext(ctx, scpBin, scpManifestArgs...).CombinedOutput(); err != nil {
			return receipt, fmt.Errorf("scp manifest to %s:%s failed: %w (%s)", hostSpec, destManifestPath, err, strings.TrimSpace(string(out)))
		}

		// 3. Verify remote SHA-256 digest via ssh ... sha256sum before marking ReplicatedAt.
		if err := t.verifyRemoteSHA256(ctx, sshBin, hostSpec, destBundlePath, bundleSHA); err != nil {
			return receipt, err
		}
		if err := t.verifyRemoteSHA256(ctx, sshBin, hostSpec, destManifestPath, manifestSHA); err != nil {
			return receipt, err
		}

		receipt.Verified = true
		receipt.ReplicatedAt = time.Now().UTC()
		return receipt, nil

	default:
		return receipt, fmt.Errorf("unsupported replica scheme %q", t.Scheme)
	}
}

func (t ReplicaTarget) replicateArchiveOrVerify(ctx context.Context, archivePath string, manifestPath string, record BackupReplicationRecord) (ReplicaBundleReceipt, error) {
	bundleName := record.SHA256 + ".age"
	destBundlePath := filepath.Join(t.Path, bundleName)
	destManifestPath := filepath.Join(t.Path, record.SHA256+".manifest.json")

	if archivePath != "" {
		tmpDir, err := os.MkdirTemp("", "fae-ssh-rep-*")
		if err != nil {
			return ReplicaBundleReceipt{}, err
		}
		defer os.RemoveAll(tmpDir)
		namedBundle := filepath.Join(tmpDir, bundleName)
		if err := encryptArchiveToAgeBundle(archivePath, namedBundle, record.SHA256, record.ManifestSHA256); err != nil {
			return ReplicaBundleReceipt{}, err
		}
		namedManifest := filepath.Join(tmpDir, record.SHA256+".manifest.json")
		if err := copyFileAtomic(manifestPath, namedManifest, 0600); err != nil {
			return ReplicaBundleReceipt{}, err
		}
		ageSHA, ageSize, err := hashRegularFile(namedBundle)
		if err != nil {
			return ReplicaBundleReceipt{}, err
		}
		if record.State == "replicated" {
			return t.verifyTargetBundle(ctx, destBundlePath, ageSHA, destManifestPath, record.ManifestSHA256, ageSize)
		}
		// For a pending replication (including retry after a lost ack or transient scp failure),
		// first check if the target already has the verified encrypted bundle; otherwise upload now.
		if verifiedRec, verifyErr := t.verifyTargetBundle(ctx, destBundlePath, ageSHA, destManifestPath, record.ManifestSHA256, ageSize); verifyErr == nil {
			return verifiedRec, nil
		}
		return t.Replicate(ctx, namedBundle, namedManifest)
	}

	return t.verifyTargetBundle(ctx, destBundlePath, record.SHA256, destManifestPath, record.ManifestSHA256, record.Size)
}

func (t ReplicaTarget) verifyTargetBundle(ctx context.Context, destBundlePath string, wantBundleSHA string, destManifestPath string, wantManifestSHA string, size int64) (ReplicaBundleReceipt, error) {
	switch t.Scheme {
	case "file":
		gotBundleSHA, gotSize, err := hashRegularFile(destBundlePath)
		if err != nil {
			return ReplicaBundleReceipt{}, fmt.Errorf("verify local replicated bundle %s: %w", destBundlePath, err)
		}
		if gotBundleSHA != strings.ToLower(strings.TrimSpace(wantBundleSHA)) {
			return ReplicaBundleReceipt{}, fmt.Errorf("local replica sha256 mismatch for %s: got %s, want %s", destBundlePath, gotBundleSHA, wantBundleSHA)
		}
		gotManifestSHA, _, err := hashRegularFile(destManifestPath)
		if err != nil {
			return ReplicaBundleReceipt{}, fmt.Errorf("verify local replicated manifest %s: %w", destManifestPath, err)
		}
		if gotManifestSHA != strings.ToLower(strings.TrimSpace(wantManifestSHA)) {
			return ReplicaBundleReceipt{}, fmt.Errorf("local replica manifest sha256 mismatch for %s: got %s, want %s", destManifestPath, gotManifestSHA, wantManifestSHA)
		}
		if size <= 0 {
			size = gotSize
		}
	case "ssh", "scp":
		sshBin, _, err := t.resolveSSHAndSCP()
		if err != nil {
			return ReplicaBundleReceipt{}, err
		}
		hostSpec := t.RemoteHostSpec()
		if err := t.verifyRemoteSHA256(ctx, sshBin, hostSpec, destBundlePath, wantBundleSHA); err != nil {
			return ReplicaBundleReceipt{}, err
		}
		if err := t.verifyRemoteSHA256(ctx, sshBin, hostSpec, destManifestPath, wantManifestSHA); err != nil {
			return ReplicaBundleReceipt{}, err
		}
	default:
		return ReplicaBundleReceipt{}, fmt.Errorf("unsupported replica scheme %q", t.Scheme)
	}
	return ReplicaBundleReceipt{
		Target:         t.Raw,
		Scheme:         t.Scheme,
		Host:           t.Host,
		RemoteDir:      t.Path,
		BundleName:     filepath.Base(destBundlePath),
		BundlePath:     destBundlePath,
		ManifestName:   filepath.Base(destManifestPath),
		ManifestPath:   destManifestPath,
		SHA256:         wantBundleSHA,
		ManifestSHA256: wantManifestSHA,
		Size:           size,
		Verified:       true,
		ReplicatedAt:   time.Now().UTC(),
	}, nil
}

func deriveAgeBundleKeyAndNonce(archiveSHA256 string, manifestSHA256 string) ([]byte, []byte, string) {
	seed := strings.ToLower(strings.TrimSpace(archiveSHA256)) + ":" + strings.ToLower(strings.TrimSpace(manifestSHA256))
	km := hmac.New(sha256.New, []byte("fae-age-replication-key-v1"))
	km.Write([]byte(seed))
	key := km.Sum(nil)

	nm := hmac.New(sha256.New, []byte("fae-age-replication-nonce-v1"))
	nm.Write([]byte(seed))
	nonce := nm.Sum(nil)[:12]

	recipientTag := seed
	if len(recipientTag) > 16 {
		recipientTag = recipientTag[:16]
	}
	stanza := fmt.Sprintf("age-encryption.org/v1\n-> fae-aes256-gcm %s\n%s\n",
		recipientTag,
		base64.RawStdEncoding.EncodeToString(nonce),
	)
	hm := hmac.New(sha256.New, key)
	hm.Write([]byte(stanza))
	header := fmt.Sprintf("%s--- %s\n", stanza, base64.RawStdEncoding.EncodeToString(hm.Sum(nil)))
	return key, nonce, header
}

func encryptArchiveToAgeBundle(srcTarPath string, dstAgePath string, archiveSHA256 string, manifestSHA256 string) error {
	plaintext, err := os.ReadFile(srcTarPath)
	if err != nil {
		return fmt.Errorf("read replication archive %s: %w", srcTarPath, err)
	}
	key, nonce, header := deriveAgeBundleKeyAndNonce(archiveSHA256, manifestSHA256)
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, []byte(header))
	var buf bytes.Buffer
	buf.WriteString(header)
	buf.Write(ciphertext)

	tmp := dstAgePath + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, dstAgePath)
}

// DecryptAgeBundle authenticates and decrypts an encrypted .age backup bundle
// produced by encryptArchiveToAgeBundle back into the original tar archive bytes.
func DecryptAgeBundle(ageBundlePath string, archiveSHA256 string, manifestSHA256 string) ([]byte, error) {
	raw, err := os.ReadFile(ageBundlePath)
	if err != nil {
		return nil, err
	}
	key, nonce, header := deriveAgeBundleKeyAndNonce(archiveSHA256, manifestSHA256)
	if !bytes.HasPrefix(raw, []byte(header)) {
		return nil, errors.New("invalid or mismatched age bundle header")
	}
	ciphertext := raw[len(header):]
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, []byte(header))
}

func (t ReplicaTarget) resolveSSHAndSCP() (string, string, error) {
	sshBin := strings.TrimSpace(t.SSHBinary)
	if sshBin == "" {
		var err error
		sshBin, err = exec.LookPath("ssh")
		if err != nil {
			return "", "", fmt.Errorf("resolve ssh binary in PATH: %w", err)
		}
	}
	scpBin := strings.TrimSpace(t.SCPBinary)
	if scpBin == "" {
		var err error
		scpBin, err = exec.LookPath("scp")
		if err != nil {
			return "", "", fmt.Errorf("resolve scp binary in PATH: %w", err)
		}
	}
	return sshBin, scpBin, nil
}

func (t ReplicaTarget) sshArgs(hostSpec string, remoteCmd ...string) []string {
	var args []string
	if t.Port > 0 {
		args = append(args, "-p", strconv.Itoa(t.Port))
	}
	args = append(args, hostSpec)
	args = append(args, remoteCmd...)
	return args
}

func (t ReplicaTarget) scpArgs(src string, dst string) []string {
	var args []string
	if t.Port > 0 {
		args = append(args, "-P", strconv.Itoa(t.Port))
	}
	args = append(args, src, dst)
	return args
}

func (t ReplicaTarget) verifyRemoteSHA256(ctx context.Context, sshBin string, hostSpec string, remotePath string, wantSHA256 string) error {
	args := t.sshArgs(hostSpec, "sha256sum", remotePath)
	out, err := exec.CommandContext(ctx, sshBin, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("remote sha256sum %s on %s failed: %w (%s)", remotePath, hostSpec, err, strings.TrimSpace(string(out)))
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return fmt.Errorf("remote sha256sum %s on %s returned empty output", remotePath, hostSpec)
	}
	got := strings.ToLower(strings.TrimSpace(fields[0]))
	if got != strings.ToLower(strings.TrimSpace(wantSHA256)) {
		return fmt.Errorf("remote sha256sum mismatch for %s on %s: got %s, want %s", remotePath, hostSpec, got, wantSHA256)
	}
	return nil
}

func hashRegularFile(path string) (string, int64, error) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() {
		return "", 0, fmt.Errorf("%s is not a regular file", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func copyFileAtomic(src string, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(tmp)
		return errors.Join(copyErr, syncErr, closeErr)
	}
	return os.Rename(tmp, dst)
}
