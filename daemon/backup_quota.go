package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"syscall"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

const backupQuotaKey = "backup-storage-quota"

type BackupQuotaParams struct {
	Action             string `json:"action"`
	MaxBytes           int64  `json:"max_bytes"`
	MaxBackups         int    `json:"max_backups"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}
type BackupQuotaPolicy struct {
	MaxBytes   int64 `json:"max_bytes"`
	MaxBackups int   `json:"max_backups"`
}
type BackupQuotaStatus struct {
	BackupQuotaPolicy
	UsageBytes  int64  `json:"usage_bytes"`
	BackupCount int    `json:"backup_count"`
	Exceeded    bool   `json:"exceeded"`
	Enforcement string `json:"enforcement"`
}

func validBackupQuota(p BackupQuotaPolicy) bool {
	return p.MaxBytes >= 0 && p.MaxBytes <= 1<<60 && p.MaxBackups >= 0 && p.MaxBackups <= 10000
}
func (d *Daemon) readBackupQuota(ctx context.Context) (BackupQuotaPolicy, error) {
	var p BackupQuotaPolicy
	if d.store == nil {
		return p, nil
	}
	raw, e := d.store.GetMarker(ctx, backupQuotaKey)
	if errors.Is(e, sql.ErrNoRows) {
		return p, nil
	}
	if e != nil {
		return p, e
	}
	if e = decode(raw, &p); e != nil {
		return p, e
	}
	if !validBackupQuota(p) {
		return p, errors.New("invalid durable backup quota")
	}
	return p, nil
}

// Account filesystem contents, including failed exports and orphaned spools,
// rather than relying on manifests which omit incomplete or unverified data.
func (d *Daemon) backupQuotaStatus(ctx context.Context) (BackupQuotaStatus, error) {
	var status BackupQuotaStatus
	p, e := d.readBackupQuota(ctx)
	if e != nil {
		return status, e
	}
	status.BackupQuotaPolicy = p
	status.Enforcement = "Apparent regular-file bytes, including incomplete backups and replication spools. Backup admission and post-export/verification checkpoints; unknown Dolt export size is not a hard filesystem limit. No automatic deletion."
	entries := 0
	for _, name := range []string{"backup", "backup-replication"} {
		root := filepath.Join(d.layout.Root, name)
		if _, e = os.Lstat(root); os.IsNotExist(e) {
			continue
		} else if e != nil {
			return status, e
		}
		physical, e := filepath.EvalSymlinks(root)
		if e != nil || physical != root {
			return status, errors.New("quota storage root must be physical")
		}
		e = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err = ctx.Err(); err != nil {
				return err
			}
			entries++
			if entries > 1000000 {
				return errors.New("quota storage entry bound exceeded")
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			st, ok := info.Sys().(*syscall.Stat_t)
			if !ok || st.Uid != uint32(os.Getuid()) || (!info.IsDir() && !info.Mode().IsRegular()) {
				return errors.New("quota storage contains foreign or special object")
			}
			if info.IsDir() {
				if name == "backup" && filepath.Dir(path) == root {
					status.BackupCount++
				}
				return nil
			}
			if info.Size() < 0 || status.UsageBytes > math.MaxInt64-info.Size() {
				return errors.New("quota storage size overflow")
			}
			status.UsageBytes += info.Size()
			return nil
		})
		if e != nil {
			return status, e
		}
	}
	status.Exceeded = (p.MaxBytes > 0 && status.UsageBytes > p.MaxBytes) || (p.MaxBackups > 0 && status.BackupCount > p.MaxBackups)
	return status, nil
}

// Caller holds backupMu across admission and the associated filesystem effect.
func (d *Daemon) backupQuotaAdmission(ctx context.Context, additionalBytes int64, additionalBackups int) error {
	if additionalBytes < 0 || additionalBackups < 0 {
		return errors.New("negative quota reservation")
	}
	s, e := d.backupQuotaStatus(ctx)
	if e != nil {
		return e
	}
	bytesFit := additionalBytes <= math.MaxInt64-s.UsageBytes && (s.MaxBytes == 0 || s.UsageBytes+additionalBytes <= s.MaxBytes)
	countFit := additionalBackups <= math.MaxInt-s.BackupCount && (s.MaxBackups == 0 || s.BackupCount+additionalBackups <= s.MaxBackups)
	if !CanAdmitBackupStorage(bytesFit, countFit) {
		return fmt.Errorf("backup quota exceeded: bytes=%d max_bytes=%d backups=%d max_backups=%d", s.UsageBytes, s.MaxBytes, s.BackupCount, s.MaxBackups)
	}
	return nil
}

// Remaining apparent bytes bound replication archive writes; -1 means unlimited.
// Backup count limits apply to creating backup directories, not spool files.
func (d *Daemon) backupQuotaRemaining(ctx context.Context) (int64, error) {
	s, e := d.backupQuotaStatus(ctx)
	if e != nil {
		return 0, e
	}
	if s.MaxBytes == 0 {
		return -1, nil
	}
	if s.UsageBytes >= s.MaxBytes {
		return 0, nil
	}
	return s.MaxBytes - s.UsageBytes, nil
}
func (d *Daemon) backupQuotaRPC(ctx context.Context, r api.Request) (any, error) {
	principal, ok := api.AuthenticatedPrincipal(ctx)
	if !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("backup quota requires instance operator")
	}
	var p BackupQuotaParams
	if e := decode(r.Params, &p); e != nil {
		return nil, e
	}
	if p.Action != "configure" && p.Action != "status" {
		return nil, errors.New("unknown backup quota action")
	}
	if p.Action == "status" && (p.MaxBytes != 0 || p.MaxBackups != 0) {
		return nil, errors.New("only configure accepts quota limits")
	}
	if !validBackupQuota(BackupQuotaPolicy{p.MaxBytes, p.MaxBackups}) {
		return nil, errors.New("max_bytes must be 0..1152921504606846976 and max_backups 0..10000")
	}
	if !d.backupMu.TryLock() {
		return nil, errors.New("backup operation in progress")
	}
	defer d.backupMu.Unlock()
	if p.Action == "configure" {
		if e := d.store.AuthorizeReadOnlyEffect(ctx, p.AuthorizingMessage, "operator", "BackupQuota", "runtime", actionClasses()); e != nil {
			return nil, e
		}
		auth, e := d.store.AuthorizeAction(ctx, p.AuthorizingMessage, "operator", "BackupQuota", "runtime", false, actionClasses())
		if e != nil {
			return nil, e
		}
		if _, e = d.backupQuotaStatus(ctx); e != nil {
			return nil, e
		}
		raw, _ := json.Marshal(BackupQuotaPolicy{p.MaxBytes, p.MaxBackups})
		if e = d.store.PutMarker(auth, controlplane.Audit{Actor: "operator", Action: "BackupQuota", MessageID: p.AuthorizingMessage, Inputs: r.Params, Outcome: "configured"}, backupQuotaKey, raw); e != nil {
			return nil, e
		}
	}
	return d.backupQuotaStatus(ctx)
}
