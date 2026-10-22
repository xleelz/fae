package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

type BackupRetentionParams struct {
	Keep               int    `json:"keep"`
	Apply              bool   `json:"apply"`
	DryRun             bool   `json:"dry_run"`
	PlanDigest         string `json:"plan_digest,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}
type BackupRetentionKeep struct {
	Path    string   `json:"path"`
	Reasons []string `json:"reasons"`
}
type BackupRetentionResult struct {
	identities map[string]string
	PlanDigest string                `json:"plan_digest"`
	Keep       int                   `json:"keep"`
	Retained   []BackupRetentionKeep `json:"retained"`
	Remove     []string              `json:"remove"`
	Removed    []string              `json:"removed"`
}

// selectBackupRetention protects historical snapshots, not only current
// jobs: cleanup may already have removed a task's physical worktree.
func selectBackupRetention(backups []planeBackup, keep int) (BackupRetentionResult, error) {
	result := BackupRetentionResult{Keep: keep, Retained: []BackupRetentionKeep{}, Remove: []string{}, Removed: []string{}}
	if keep < 1 || keep > 10000 {
		return result, errors.New("keep must be 1..10000")
	}
	sort.Slice(backups, func(i, j int) bool {
		if backups[i].Created.Equal(backups[j].Created) {
			return backups[i].Path < backups[j].Path
		}
		return backups[i].Created.After(backups[j].Created)
	})
	covered := map[backupJob]bool{}
	for i, b := range backups {
		reasons := []string{}
		if i < keep {
			reasons = append(reasons, "newest")
		}
		for _, job := range b.Jobs {
			if !covered[job] {
				reasons = append(reasons, "snapshot coverage: "+job.ID)
				covered[job] = true
			}
		}
		if len(reasons) > 0 {
			sort.Strings(reasons)
			result.Retained = append(result.Retained, BackupRetentionKeep{Path: b.Path, Reasons: reasons})
		} else {
			result.Remove = append(result.Remove, b.Path)
		}
	}
	raw, err := json.Marshal(struct {
		Keep    int           `json:"keep"`
		Backups []planeBackup `json:"backups"`
	}{keep, backups})
	if err != nil {
		return result, err
	}
	result.PlanDigest = controlplane.Digest(raw)
	return result, nil
}
func (d *Daemon) retentionBackups(ctx context.Context) ([]planeBackup, error) {
	root := filepath.Join(d.layout.Root, "backup")
	physical, err := filepath.EvalSymlinks(root)
	if err != nil || physical != root {
		return nil, errors.New("backup root must be physical")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	if len(entries) > 10000 {
		return nil, errors.New("backup retention listing bound exceeded")
	}
	out := make([]planeBackup, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), "plane-") {
			return nil, fmt.Errorf("unknown backup entry %s", entry.Name())
		}
		b, err := d.readBackup(ctx, filepath.Join(root, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("verify retention candidate %s: %w", entry.Name(), err)
		}
		if b.Created.IsZero() {
			return nil, errors.New("backup creation timestamp missing")
		}
		seen := map[string]bool{}
		for _, j := range b.Jobs {
			if j.ID == "" || j.Version < 1 || len(j.Digest) != 64 || seen[j.ID] {
				return nil, errors.New("backup job identity invalid")
			}
			seen[j.ID] = true
		}
		out = append(out, b)
	}
	return out, nil
}
func (d *Daemon) backupRetention(ctx context.Context, p BackupRetentionParams) (BackupRetentionResult, error) {
	var result BackupRetentionResult
	if p.Apply && p.DryRun {
		return result, errors.New("apply and dry-run are mutually exclusive")
	}
	if p.Keep < 1 || p.Keep > 10000 {
		return result, errors.New("keep must be 1..10000")
	}
	if p.Apply && len(p.PlanDigest) != 64 {
		return result, errors.New("apply requires preview plan_digest")
	}
	if !d.backupMu.TryLock() {
		return result, errors.New("backup operation in progress")
	}
	defer d.backupMu.Unlock()
	var err error
	result, err = d.retentionPlan(ctx, p.Keep)
	if err != nil || !p.Apply {
		return result, err
	}
	if p.PlanDigest != result.PlanDigest {
		return result, errors.New("backup retention plan changed; preview again")
	}
	protected := map[string]bool{}
	for _, b := range result.Retained {
		protected[b.Path] = true
	}
	// Recheck the entire set before the first destructive action. No unsafe
	// candidate may cause a partly applied batch, including a damaged survivor.
	current, err := d.retentionPlan(ctx, p.Keep)
	if err != nil {
		return result, err
	}
	if current.PlanDigest != result.PlanDigest {
		return result, errors.New("backup retention plan changed before deletion")
	}
	for _, path := range result.Remove {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		_, err := d.readBackup(ctx, path)
		identity, identityErr := backupRetentionIdentity(path)
		same := identityErr == nil && identity == current.identities[path]
		if !CanPruneBackup(err == nil, protected[path], p.PlanDigest == current.PlanDigest && same) {
			if err != nil {
				return result, err
			}
			return result, errors.New("backup retention guard refused")
		}
		if err = os.RemoveAll(path); err != nil {
			return result, err
		}
		result.Removed = append(result.Removed, path)
	}
	return result, nil
}
func (d *Daemon) backupRetentionRPC(ctx context.Context, r api.Request) (any, error) {
	principal, ok := api.AuthenticatedPrincipal(ctx)
	if !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("backup retention requires instance operator")
	}
	var p BackupRetentionParams
	if err := decode(r.Params, &p); err != nil {
		return nil, err
	}
	if p.Apply {
		if err := d.store.AuthorizeReadOnlyEffect(ctx, p.AuthorizingMessage, principal.Actor, "BackupRetention", "runtime", actionClasses()); err != nil {
			return nil, err
		}
	}
	return d.backupRetention(ctx, p)
}

// Include filesystem identity and exact manifest bytes so replacing a backup
// directory or rewriting its certificate invalidates an outstanding preview.
func (d *Daemon) retentionPlan(ctx context.Context, keep int) (BackupRetentionResult, error) {
	backups, err := d.retentionBackups(ctx)
	if err != nil {
		return BackupRetentionResult{}, err
	}
	result, err := selectBackupRetention(backups, keep)
	if err != nil {
		return result, err
	}
	if d.store != nil {
		pins, e := d.replicationProtectedBackups(ctx)
		if e != nil {
			return result, e
		}
		remove := []string{}
		for _, path := range result.Remove {
			if pins[path] {
				result.Retained = append(result.Retained, BackupRetentionKeep{Path: path, Reasons: []string{"replication journal"}})
			} else {
				remove = append(remove, path)
			}
		}
		result.Remove = remove
		// Include journal protection in the preview, even if no candidate is removed.
		pinned := []string{}
		for path := range pins {
			pinned = append(pinned, path)
		}
		sort.Strings(pinned)
		raw, _ := json.Marshal(struct {
			Plan string
			Pins []string
		}{result.PlanDigest, pinned})
		result.PlanDigest = controlplane.Digest(raw)
	}
	identities := []string{result.PlanDigest}
	result.identities = map[string]string{}
	for _, b := range backups {
		identity, e := backupRetentionIdentity(b.Path)
		if e != nil {
			return result, e
		}
		result.identities[b.Path] = identity
		identities = append(identities, identity)
	}
	raw, _ := json.Marshal(identities)
	result.PlanDigest = controlplane.Digest(raw)
	return result, nil
}

func backupRetentionIdentity(path string) (string, error) {
	info, e := os.Lstat(path)
	if e != nil {
		return "", e
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.IsDir() || st.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0022 != 0 {
		return "", errors.New("physical backup directory identity required")
	}
	raw, e := os.ReadFile(filepath.Join(path, "verified.json"))
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("%s:%d:%d:%d:%d:%s", path, st.Dev, st.Ino, st.Uid, info.Mode().Perm(), controlplane.Digest(raw)), nil
}
