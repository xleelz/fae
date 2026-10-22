package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/workflow"
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
	Receipt        *BackupReplicationReceipt `json:"receipt,omitempty"`
}

func replicationKey(p BackupReplicationParams) string {
	raw, _ := json.Marshal([]string{p.Endpoint, p.Backup})
	return "backup-replication/" + controlplane.Digest(raw)
}
func normalizeReplicationParams(p BackupReplicationParams) (BackupReplicationParams, error) {
	p.Endpoint = strings.TrimRight(p.Endpoint, "/")
	u, err := url.Parse(p.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return p, errors.New("replication requires exact HTTPS endpoint")
	}
	if u.Scheme == "http" && u.Hostname() != "127.0.0.1" && u.Hostname() != "::1" {
		return p, errors.New("replication HTTP requires literal loopback")
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
		release, e := d.live.guard.Acquire(ctx, engine.Dispatch{ID: sid, TaskID: sid, Config: workflow.Stage{Weight: 1}}, execution.Binding{}, engine.Session{ID: sid})
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
