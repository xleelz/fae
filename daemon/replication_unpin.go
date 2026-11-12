package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

type BackupReplicationUnpinParams struct {
	Backup             string `json:"backup"`
	Apply              bool   `json:"apply"`
	PlanDigest         string `json:"plan_digest,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}
type BackupReplicationUnpinResult struct {
	Backup     string `json:"backup"`
	PlanDigest string `json:"plan_digest"`
	Outboxes   int    `json:"outboxes"`
	Completed  bool   `json:"completed"`
}

func (d *Daemon) backupReplicationUnpinRPC(ctx context.Context, r api.Request) (any, error) {
	var p BackupReplicationUnpinParams
	if err := decode(r.Params, &p); err != nil {
		return nil, err
	}
	principal, ok := api.AuthenticatedPrincipal(ctx)
	if !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("replication unpin requires instance operator")
	}
	authorize := func() error {
		return d.store.AuthorizeReadOnlyEffect(ctx, p.AuthorizingMessage, principal.Actor, r.Method, "runtime", actionClasses())
	}
	if err := authorize(); err != nil {
		return nil, err
	}
	var err error
	ctx, err = d.store.AuthorizeAction(ctx, p.AuthorizingMessage, principal.Actor, r.Method, "runtime", false, actionClasses())
	if err != nil {
		return nil, err
	}
	return d.backupReplicationUnpin(ctx, p, controlplane.Audit{Actor: principal.Actor, Action: r.Method, MessageID: p.AuthorizingMessage, Inputs: r.Params, Outcome: "unpinned"}, authorize)
}
func (d *Daemon) backupReplicationUnpin(ctx context.Context, p BackupReplicationUnpinParams, a controlplane.Audit, authorize func() error) (BackupReplicationUnpinResult, error) {
	result := BackupReplicationUnpinResult{Backup: p.Backup}
	// The scheduler uses this same lock order. Hold both through proof and pin mutation.
	if !scheduleControlLock(ctx, &d.replicationScheduleMu, &d.replicationControls) {
		return result, errors.New("replication schedule busy")
	}
	defer d.replicationScheduleMu.Unlock()
	if !d.backupMu.TryLock() {
		return result, errors.New("backup operation busy")
	}
	defer d.backupMu.Unlock()
	if d.live != nil && d.live.guard != nil {
		sid := "replication-unpin/" + time.Now().UTC().Format("20060102150405.000000000")
		release, err := d.live.guard.AcquireHostIO(ctx, sid)
		if err != nil {
			return result, err
		}
		defer release()
	}
	pins, err := d.replicationProtectedBackups(ctx)
	if err != nil {
		return result, err
	}
	if !pins[p.Backup] {
		return result, errors.New("backup is not replication pinned")
	}
	schedule, err := d.readReplicationSchedule(ctx)
	if err != nil {
		return result, err
	}
	noClaim := schedule.Pending == nil || schedule.Pending.Backup != p.Backup
	if !noClaim {
		return result, errors.New("backup has pending scheduled replication claim")
	}
	manifest, err := d.scheduleManifest(ctx, p.Backup)
	if err != nil {
		return result, err
	}
	markers, err := d.store.ReplicationMarkers(ctx)
	if err != nil {
		return result, err
	}
	records := map[string]json.RawMessage{}
	for key, raw := range markers {
		var record BackupReplicationRecord
		if err = decode(raw, &record); err != nil {
			return result, err
		}
		normalized, e := normalizeReplicationParams(BackupReplicationParams{Backup: record.Backup, Endpoint: record.Endpoint, MaxBytes: record.MaxBytes})
		if e != nil || replicationKey(normalized) != key || normalized.Endpoint != record.Endpoint || normalized.MaxBytes != record.MaxBytes {
			return result, errors.New("invalid replication outbox identity")
		}
		if record.Backup != p.Backup {
			continue
		}
		if record.State != "replicated" || record.Receipt == nil || record.ManifestSHA256 != manifest || !replicationDigest(record.SHA256) || record.Size <= 0 || record.Size > record.MaxBytes {
			return result, errors.New("backup has uncertain or invalid replication outbox")
		}
		expected := BackupReplicationReceipt{ID: record.SHA256, SHA256: record.SHA256, ManifestSHA256: manifest, Size: record.Size, State: "stored"}
		if *record.Receipt != expected {
			return result, errors.New("stored replication receipt differs")
		}
		records[key] = raw
	}
	if len(records) == 0 {
		return result, errors.New("pinned backup has no confirmed outbox")
	}
	for _, raw := range records {
		var record BackupReplicationRecord
		_ = json.Unmarshal(raw, &record)
		if err = authorize(); err != nil {
			return result, err
		}
		receipt, e := replicationRemoteReceipt(ctx, record, nil, false)
		if e != nil {
			return result, e
		}
		if receipt != *record.Receipt {
			return result, errors.New("remote replication receipt differs")
		}
		if err = authorize(); err != nil {
			return result, err
		}
		if err = replicationRemoteContent(ctx, record); err != nil {
			return result, err
		}
	}
	if !CanUnpinReplication(len(records) > 0, true, noClaim) {
		return result, errors.New("replication unpin proof incomplete")
	}
	// All source pins are included so any intervening pin mutation invalidates preview.
	raw, _ := json.Marshal(struct {
		Backup, Manifest string
		Pins             map[string]bool
		Outboxes         map[string]json.RawMessage
	}{p.Backup, manifest, pins, records})
	result.PlanDigest = controlplane.Digest(raw)
	result.Outboxes = len(records)
	if !p.Apply {
		return result, nil
	}
	if p.PlanDigest != result.PlanDigest {
		return result, errors.New("replication unpin plan is stale")
	}
	if err = authorize(); err != nil {
		return result, err
	}
	// Verify local evidence once more after potentially slow network proof.
	current, err := d.scheduleManifest(ctx, p.Backup)
	if err != nil {
		return result, err
	}
	if current != manifest {
		return result, errors.New("replication source changed during proof")
	}
	delete(pins, p.Backup)
	paths := make([]string, 0, len(pins))
	for path := range pins {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	raw, _ = json.Marshal(paths)
	if err = d.store.PutMarker(ctx, a, "backup-replication-sources", raw); err != nil {
		return result, err
	}
	result.Completed = true
	return result, nil
}
