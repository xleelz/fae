package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/execution"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

type CleanupParams struct {
	Backup             string `json:"backup"`
	Apply              bool   `json:"apply"`
	DryRun             bool   `json:"dry_run"`
	PaceMS             int    `json:"pace_ms"`
	AuthorizingMessage string `json:"authorizing_message"`
}
type backupJob struct {
	ID      string `json:"job_id"`
	Version int64  `json:"version"`
	State   string `json:"state"`
	Digest  string `json:"payload_digest"`
}
type planeBackup struct {
	Path      string            `json:"path"`
	Instance  string            `json:"instance"`
	Workspace string            `json:"workspace"`
	Created   time.Time         `json:"created"`
	Files     map[string]string `json:"files"`
	Jobs      []backupJob       `json:"jobs"`
}

func backupFiles(ctx context.Context, root string) (map[string]string, error) {
	files := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("symlink in backup")
		}
		if entry.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		if rel == "verified.json" {
			return nil
		}
		if !entry.Type().IsRegular() {
			return errors.New("nonregular backup file")
		}
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		h := sha256.New()
		_, e = io.Copy(h, &backupReader{ctx: ctx, r: f})
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		files[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return files, err
}
func (d *Daemon) backupPlane(ctx context.Context) (planeBackup, error) {
	if !d.backupMu.TryLock() {
		return planeBackup{}, errors.New("backup operation in progress")
	}
	defer d.backupMu.Unlock()
	if d.live != nil && d.live.guard != nil {
		sid := fmt.Sprintf("plane-backup-%d", time.Now().UnixNano())
		release, err := d.live.guard.Acquire(ctx, engine.Dispatch{ID: sid, TaskID: sid, Config: workflow.Stage{Weight: 1}}, execution.Binding{}, engine.Session{ID: sid})
		if err != nil {
			return planeBackup{}, err
		}
		defer release()
	}
	if message, ok := ctx.Value(backupScheduleAuthorityKey{}).(string); ok {
		if err := d.backupRecurrenceAuthority(ctx, message); err != nil {
			return planeBackup{}, fmt.Errorf("backup schedule authority: %w", err)
		}
	}
	if err := d.backupQuotaAdmission(ctx, 1, 1); err != nil {
		return planeBackup{}, err
	}
	b := planeBackup{Instance: d.config.Instance, Workspace: d.config.Workspace, Created: time.Now().UTC()}
	parent := filepath.Join(d.layout.Root, "backup")
	physical, err := filepath.EvalSymlinks(parent)
	if err != nil || physical != parent {
		return b, errors.New("backup root must be physical")
	}
	b.Path, err = os.MkdirTemp(parent, "plane-")
	if err != nil {
		return b, err
	}
	remote := filepath.Join(b.Path, "remote")
	if err = d.store.BackupPlane(ctx, remote); err != nil {
		return b, fmt.Errorf("Dolt backup: %w", err)
	}
	if err = d.backupQuotaAdmission(ctx, 0, 0); err != nil {
		return b, err
	}
	binary := d.config.DoltBinary
	if binary == "" {
		binary = "dolt"
	}
	backupEnv := scopedEnv(b.Path)
	for _, pair := range [][2]string{{"user.name", "Fae backup"}, {"user.email", "fae@localhost"}} {
		config := exec.CommandContext(ctx, binary, "config", "--global", "--add", pair[0], pair[1])
		config.Dir = b.Path
		config.Env = backupEnv
		if _, err = config.CombinedOutput(); err != nil {
			return b, fmt.Errorf("backup verification identity: %w", err)
		}
	}
	cmd := exec.CommandContext(ctx, binary, "backup", "restore", (&url.URL{Scheme: "file", Path: remote}).String(), "restored")
	cmd.Dir = b.Path
	cmd.Env = backupEnv
	if output, e := cmd.CombinedOutput(); e != nil {
		return b, fmt.Errorf("verify backup restore: %w: %s", e, output)
	}
	cmd = exec.CommandContext(ctx, binary, "sql", "--result-format=json", "-q", "SELECT job_id,version,state,payload_digest FROM jobs ORDER BY job_id")
	cmd.Dir = filepath.Join(b.Path, "restored")
	cmd.Env = backupEnv
	output, err := cmd.Output()
	if err != nil {
		return b, fmt.Errorf("verify restored jobs: %w", err)
	}
	var result struct {
		Rows []backupJob `json:"rows"`
	}
	if err = json.Unmarshal(output, &result); err != nil {
		return b, err
	}
	b.Jobs = result.Rows
	// Verify the restored database's blob contents too, not merely its metadata.
	cmd = exec.CommandContext(ctx, binary, "fsck")
	cmd.Dir = filepath.Join(b.Path, "restored")
	cmd.Env = backupEnv
	if output, e := cmd.CombinedOutput(); e != nil {
		return b, fmt.Errorf("verify backup chunks: %w: %s", e, output)
	}
	b.Files, err = backupFiles(ctx, b.Path)
	if err != nil {
		return b, err
	}
	if len(b.Files) == 0 {
		return b, errors.New("empty backup")
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return b, err
	}
	err = writePrivate(filepath.Join(b.Path, "verified.json"), raw)
	if err == nil {
		err = d.backupQuotaAdmission(ctx, 0, 0)
	}
	return b, err
}
func (d *Daemon) readBackup(ctx context.Context, path string) (planeBackup, error) {
	var b planeBackup
	if filepath.Dir(path) != filepath.Join(d.layout.Root, "backup") || !strings.HasPrefix(filepath.Base(path), "plane-") {
		return b, errors.New("backup must be an owned plane backup")
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil || physical != path {
		return b, errors.New("backup must be physical")
	}
	raw, err := os.ReadFile(filepath.Join(path, "verified.json"))
	if err != nil {
		return b, err
	}
	if err = json.Unmarshal(raw, &b); err != nil {
		return b, err
	}
	if b.Path != path || b.Instance != d.config.Instance || b.Workspace != d.config.Workspace {
		return b, errors.New("foreign backup")
	}
	files, err := backupFiles(ctx, path)
	if err != nil {
		return b, err
	}
	if len(files) == 0 || len(files) != len(b.Files) {
		return b, errors.New("backup files changed")
	}
	for name, sum := range files {
		if b.Files[name] != sum {
			return b, errors.New("backup checksum mismatch")
		}
	}
	return b, nil
}
func backedUp(b planeBackup, j controlplane.JobSnapshot) bool {
	for _, saved := range b.Jobs {
		if saved.ID == j.ID {
			return saved.State == "Retired" && saved.Version == j.Version && saved.Digest == controlplane.Digest(j.Payload)
		}
	}
	return false
}

type cleanupItem struct {
	Task    string               `json:"task"`
	Plan    worktree.CleanupPlan `json:"plan"`
	Removed bool                 `json:"removed"`
}

func (d *Daemon) cleanupRetired(ctx context.Context, p CleanupParams) ([]cleanupItem, error) {
	if !d.backupMu.TryLock() {
		return nil, errors.New("backup operation in progress")
	}
	defer d.backupMu.Unlock()
	if p.Apply && p.DryRun {
		return nil, errors.New("apply and dry-run are mutually exclusive")
	}
	if p.PaceMS == 0 {
		p.PaceMS = 100
	}
	if p.PaceMS < 10 || p.PaceMS > 60000 {
		return nil, errors.New("pace_ms must be 10..60000")
	}
	var backup planeBackup
	var err error
	if p.Apply {
		if p.Backup == "" {
			return nil, errors.New("verified plane backup required")
		}
		backup, err = d.readBackup(ctx, p.Backup)
		if err != nil {
			return nil, err
		}
	}
	jobs, err := d.store.ListJobs(ctx, 100000)
	if err != nil {
		return nil, err
	}
	manager := &worktree.Manager{Repository: d.config.Workspace, Root: d.layout.Worktrees}
	items := []cleanupItem{}
	snapshots := map[string]controlplane.JobSnapshot{}
	// Plan the entire set before any archive or deletion. One unsafe retired task
	// refuses the batch, including missing backup coverage.
	for _, j := range jobs {
		if j.State != "Retired" {
			continue
		}
		var e envelope
		if err = json.Unmarshal(j.Payload, &e); err != nil {
			return nil, err
		}
		if e.Job.State != "Retired" || e.Job.TaskID != j.ID {
			return nil, errors.New("retired payload mismatch")
		}
		if e.Job.InFlight != nil || e.Job.OperatorStage != "" {
			return nil, errors.New("retired task still has unresolved execution")
		}
		d.mu.Lock()
		active := d.active[j.ID]
		d.mu.Unlock()
		if active {
			return nil, errors.New("retired task still executing")
		}
		if e.Job.ExecutionKind != "live" {
			continue
		}
		if p.Apply && !backedUp(backup, j) {
			return nil, fmt.Errorf("backup does not cover retired task %s", j.ID)
		}
		plan, err := manager.PlanCleanup(ctx, e.Admission.RunID, j.ID)
		if err != nil {
			return nil, fmt.Errorf("task %s: %w", j.ID, err)
		}
		items = append(items, cleanupItem{Task: j.ID, Plan: plan})
		snapshots[j.ID] = j
	}
	if !p.Apply {
		return items, nil
	}
	verifyItem := func(task string) func(context.Context) error {
		return func(ctx context.Context) error {
			expected := snapshots[task]
			current, e := d.store.GetJob(ctx, task)
			if e != nil {
				return e
			}
			if current.State != "Retired" || current.Version != expected.Version || controlplane.Digest(current.Payload) != controlplane.Digest(expected.Payload) {
				return errors.New("retired task changed after plan")
			}
			b, e := d.readBackup(ctx, p.Backup)
			if e != nil {
				return e
			}
			if !backedUp(b, current) {
				return errors.New("backup no longer covers task")
			}
			return nil
		}
	}
	for _, item := range items {
		if err = manager.ArchiveCleanup(ctx, item.Plan, verifyItem(item.Task)); err != nil {
			return items, err
		}
	}
	for i := range items {
		if i > 0 {
			timer := time.NewTimer(time.Duration(p.PaceMS) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return items, ctx.Err()
			case <-timer.C:
			}
		}
		item := &items[i]
		err = manager.Cleanup(ctx, item.Plan, verifyItem(item.Task))
		if err != nil {
			return items, err
		}
		item.Removed = true
	}
	return items, nil
}
func (d *Daemon) cleanupRPC(ctx context.Context, r api.Request) (any, error) {
	principal, ok := api.AuthenticatedPrincipal(ctx)
	if !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("cleanup and backup require instance operator")
	}
	var p CleanupParams
	if err := decode(r.Params, &p); err != nil {
		return nil, err
	}
	if r.Method == "PlaneBackup" || p.Apply {
		if err := d.store.AuthorizeReadOnlyEffect(ctx, p.AuthorizingMessage, principal.Actor, r.Method, "runtime", actionClasses()); err != nil {
			return nil, err
		}
	}
	if r.Method == "PlaneBackup" {
		return d.backupPlane(ctx)
	}
	return d.cleanupRetired(ctx, p)
}

type backupReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *backupReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
