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
	"github.com/xleelz/fae/worktree"
)

type CleanupParams struct {
	Backup string `json:"backup"`
	Apply  bool   `json:"apply"`
	DryRun bool   `json:"dry_run"`
	PaceMS int    `json:"pace_ms"`
	// Done selects Done tasks whose landing is in the integration ref
	// (`fae cleanup done`) in place of Retired tasks.
	Done bool `json:"done,omitempty"`
	// Limit bounds the tasks one run plans for removal (default
	// cleanupBatchDefault, at most cleanupBatchMax); tasks already cleaned and
	// skipped Done tasks are not counted. When more remain the result ends
	// with an item whose task is "" saying so.
	Limit              int    `json:"limit,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}

const (
	// worktreeCleanedEvent is the legacy event kind finished-task cleanup
	// checks when paging rows (FAE-CP-009). Cleanup itself writes nothing to
	// the plane (FAE-CTL-042) and recognizes cleaned tasks from their git
	// archive refs and removed worktrees and branches.
	worktreeCleanedEvent = "worktree-cleaned"
	cleanupBatchDefault  = 200
	cleanupBatchMax      = 10000
	// cleanupReadBound bounds each page read of the candidate scan on its own,
	// as the start's plane reads are bounded; the scan as a whole is bounded
	// by the batch and the RPC's budget.
	cleanupReadBound = 10 * time.Second
)

// cleanupScanPage is the rows one candidate page reads (a test seam).
var cleanupScanPage = controlplane.FinishedTasksPage

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
		release, err := d.live.guard.AcquireHostIO(ctx, sid)
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

// backedUp reports a backup holding the finished task j (Retired or Done)
// at its current version and payload.
func backedUp(b planeBackup, j controlplane.JobSnapshot) bool {
	for _, saved := range b.Jobs {
		if saved.ID == j.ID {
			return (saved.State == "Retired" || saved.State == "Done") && saved.State == j.State && saved.Version == j.Version && saved.Digest == controlplane.Digest(j.Payload)
		}
	}
	return false
}

// cleanupItem is one planned task. A trailing item with an empty Task and a
// Skipped reason says that more candidates remain beyond the run's limit.
type cleanupItem struct {
	Task    string               `json:"task"`
	Plan    worktree.CleanupPlan `json:"plan"`
	Removed bool                 `json:"removed"`
	// Landing is a Done task's own landing commit, in the integration ref.
	Landing string `json:"landing,omitempty"`
	// Skipped says why a Done task is left as it is; nothing of it is
	// archived or removed.
	Skipped string `json:"skipped,omitempty"`
}

// doneCleanupCandidate checks that a Done task's own landing commit
// (ownLandingSHA, from its Integrate artifact) is in the live integration
// ref, and returns it, or why the task is left alone.
func (d *Daemon) doneCleanupCandidate(ctx context.Context, manager *worktree.Manager, e envelope) (sha, skipped string, err error) {
	if sha = landingSHA(e); sha == "" {
		return "", "the Done task has no landing of its own (no live Integrate artifact)", nil
	}
	integrated, err := manager.Integrated(ctx, sha, d.live.config.IntegrationRef)
	if err != nil {
		return sha, "", err
	}
	if !integrated {
		return sha, fmt.Sprintf("its landing %s is not in %s", sha, d.live.config.IntegrationRef), nil
	}
	return sha, "", nil
}

// cleanupGitState summarizes the workspace repository's task branches,
// registered worktree branches and per-tip archive refs from one git read
// before the candidate scan, so already-cleaned tasks (and finished rows of
// unknown execution kind with no git or worktree presence) are passed over
// without reading their plane snapshots or writing to the plane (FAE-CTL-042,
// FAE-CP-009).
type cleanupGitState struct {
	worktrees      string
	branches       map[string]bool
	archived       map[string]bool
	invalidArchive map[string]bool
}

func loadCleanupGitState(ctx context.Context, repo, worktrees string) (cleanupGitState, bool) {
	s := cleanupGitState{
		worktrees:      worktrees,
		branches:       map[string]bool{},
		archived:       map[string]bool{},
		invalidArchive: map[string]bool{},
	}
	if repo == "" || worktrees == "" {
		return s, false
	}
	info, err := os.Lstat(worktrees)
	if err != nil || !info.IsDir() {
		return s, false
	}
	if physical, err := filepath.EvalSymlinks(worktrees); err != nil || physical != worktrees {
		return s, false
	}
	runGit := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(cleanGitEnv(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.Output()
		return string(out), err
	}
	refs, err := runGit("for-each-ref", "--format=%(refname)%00%(objectname)%00%(symref)", "refs/heads/fae/task/", "refs/fae/archive/")
	if err != nil {
		return s, false
	}
	for _, line := range strings.Split(strings.TrimSuffix(refs, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 {
			return s, false
		}
		ref, obj, sym := parts[0], parts[1], parts[2]
		if id, ok := strings.CutPrefix(ref, "refs/heads/fae/task/"); ok {
			s.branches[id] = true
			continue
		}
		if rest, ok := strings.CutPrefix(ref, "refs/fae/archive/"); ok {
			id, sha, ok := strings.Cut(rest, "/")
			if !ok || id == "" || sha == "" || strings.Contains(sha, "/") || sym != "" || obj != sha {
				if id != "" {
					s.invalidArchive[id] = true
				}
				continue
			}
			s.archived[id] = true
		}
	}
	registered, err := runGit("worktree", "list", "--porcelain")
	if err != nil {
		return s, false
	}
	for _, line := range strings.Split(registered, "\n") {
		if id, ok := strings.CutPrefix(line, "branch refs/heads/fae/task/"); ok {
			s.branches[id] = true
		}
	}
	return s, true
}

func (s cleanupGitState) alreadyCleaned(run, task string) bool {
	if run == "" || task == "" {
		return false
	}
	id := worktree.Identity(run, task)
	if !s.archived[id] || s.invalidArchive[id] || s.branches[id] {
		return false
	}
	_, err := os.Lstat(filepath.Join(s.worktrees, id))
	return os.IsNotExist(err)
}

func (s cleanupGitState) hasArtifacts(run, task string) bool {
	if run == "" || task == "" {
		return false
	}
	id := worktree.Identity(run, task)
	if s.branches[id] || s.archived[id] || s.invalidArchive[id] {
		return true
	}
	_, err := os.Lstat(filepath.Join(s.worktrees, id))
	return !os.IsNotExist(err)
}

func (d *Daemon) cleanupRefusal() error {
	if d.ctx != nil && d.ctx.Err() != nil {
		return shuttingDownError()
	}
	if held, err := d.maintenanceHeld(); held {
		return maintenanceError(err)
	}
	return nil
}

// cleanupRetired is FAE-CTL-042's cleanup of Retired tasks' worktrees and
// branches and, with p.Done (`fae cleanup done`), of Done tasks' once their
// own landing is in the integration ref. Both keep every commit (an archive
// ref per branch tip, verified before anything is deleted), need a verified
// plane backup that holds the task at its current version (FAE-CP-013),
// through the same verified cleanup (worktree.Manager.Cleanup,
// CanCleanupArchived), and perform only git operations and plane reads so
// they never collide with the drive. One unsafe Retired task refuses the
// batch; a Done task that is not integrated, or whose worktree or branch
// cannot be planned (dirty, moved, missing without an archive), is listed as
// skipped and left without counting toward the limit.
func (d *Daemon) cleanupRetired(ctx context.Context, p CleanupParams) ([]cleanupItem, error) {
	if err := d.cleanupRefusal(); err != nil {
		return nil, err
	}
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
	if p.Limit == 0 {
		p.Limit = cleanupBatchDefault
	}
	if p.Limit < 1 || p.Limit > cleanupBatchMax {
		return nil, fmt.Errorf("limit must be 1..%d", cleanupBatchMax)
	}
	state, kind := "Retired", "retired"
	if p.Done {
		if d.live == nil {
			return nil, errors.New("Done task cleanup needs a live configuration (its integration ref)")
		}
		state, kind = "Done", "Done"
	}
	manager := &worktree.Manager{Repository: d.config.Workspace, Root: d.layout.Worktrees}
	gitState, gitOK := loadCleanupGitState(ctx, d.config.Workspace, d.layout.Worktrees)
	items := []cleanupItem{}
	snapshots := map[string]controlplane.JobSnapshot{}
	landings := map[string]string{}
	// Only finished tasks of the chosen state are candidates. Their rows are
	// paged through the state index, each page read bounded on its own; a
	// task already cleaned (its worktree and branch removed with verified
	// archive evidence in git, or a legacy event row), or whose task row says
	// it was not live (or has an unknown execution kind and no git/worktree
	// artifacts), is passed over without reading its snapshot (FAE-CP-009).
	// A run plans at most p.Limit cleanable tasks; skipped Done tasks do not
	// consume the limit, so permanently skipped tasks never starve later ones.
	planned, more := 0, false
scan:
	for after := ""; ; {
		pageCtx, cancel := context.WithTimeout(ctx, cleanupReadBound)
		page, err := d.store.FinishedTasksAfter(pageCtx, state, after, worktreeCleanedEvent, cleanupScanPage)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%s task candidates: %w", kind, err)
		}
		for _, row := range page {
			if row.Marked || row.ExecutionKind != "" && row.ExecutionKind != "live" {
				continue
			}
			if gitOK {
				if gitState.alreadyCleaned(row.RunID, row.ID) {
					continue
				}
				if row.ExecutionKind == "" && !gitState.hasArtifacts(row.RunID, row.ID) {
					continue
				}
			}
			if planned == p.Limit {
				more = true
				break scan
			}
			// Plan the entire batch before any archive or deletion. One unsafe
			// retired task refuses the batch, including missing backup coverage.
			j, err := d.store.GetJob(ctx, row.ID)
			if err != nil {
				return nil, fmt.Errorf("task %s: %w", row.ID, err)
			}
			if j.State != state {
				continue
			}
			var e envelope
			if err = json.Unmarshal(j.Payload, &e); err != nil {
				return nil, err
			}
			if string(e.Job.State) != state || e.Job.TaskID != j.ID {
				return nil, fmt.Errorf("%s payload mismatch", kind)
			}
			if e.Job.InFlight != nil || e.Job.OperatorStage != "" {
				return nil, fmt.Errorf("%s task still has unresolved execution", kind)
			}
			d.mu.Lock()
			active := d.active[j.ID]
			d.mu.Unlock()
			if active {
				return nil, fmt.Errorf("%s task still executing", kind)
			}
			if e.Job.ExecutionKind != "live" {
				continue
			}
			if gitOK && gitState.alreadyCleaned(e.Admission.RunID, j.ID) {
				continue
			}
			item := cleanupItem{Task: j.ID}
			if p.Done {
				if item.Landing, item.Skipped, err = d.doneCleanupCandidate(ctx, manager, e); err != nil {
					return nil, fmt.Errorf("task %s: %w", j.ID, err)
				}
				if item.Skipped != "" {
					items = append(items, item)
					continue
				}
			}
			if p.Apply && !backedUp(backup, j) {
				return nil, fmt.Errorf("backup does not cover %s task %s", kind, j.ID)
			}
			if item.Plan, err = manager.PlanCleanup(ctx, e.Admission.RunID, j.ID); err != nil {
				if !p.Done {
					return nil, fmt.Errorf("task %s: %w", j.ID, err)
				}
				item.Skipped = "its worktree or branch cannot be cleaned: " + err.Error()
				items = append(items, item)
				continue
			}
			if item.Plan.AlreadyRemoved {
				continue
			}
			planned++
			items = append(items, item)
			snapshots[j.ID] = j
			landings[j.ID] = item.Landing
		}
		if len(page) < cleanupScanPage {
			break
		}
		after = page[len(page)-1].ID
	}
	if more {
		items = append(items, cleanupItem{Skipped: fmt.Sprintf("further %s tasks remain beyond this run's limit of %d: run again once these are cleaned", kind, p.Limit)})
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
			if current.State != state || current.Version != expected.Version || controlplane.Digest(current.Payload) != controlplane.Digest(expected.Payload) {
				return fmt.Errorf("%s task changed after plan", kind)
			}
			if sha := landings[task]; sha != "" {
				if integrated, e := manager.Integrated(ctx, sha, d.live.config.IntegrationRef); e != nil || !integrated {
					return errors.Join(errors.New("Done task's landing is no longer in the integration ref"), e)
				}
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
		if item.Skipped != "" {
			continue
		}
		if err = d.cleanupRefusal(); err != nil {
			return items, err
		}
		if err = manager.ArchiveCleanup(ctx, item.Plan, verifyItem(item.Task)); err != nil {
			return items, err
		}
	}
	removed := 0
	for i := range items {
		if items[i].Skipped != "" {
			continue
		}
		if err = d.cleanupRefusal(); err != nil {
			return items, err
		}
		if removed++; removed > 1 {
			timer := time.NewTimer(time.Duration(p.PaceMS) * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				if err = d.cleanupRefusal(); err != nil {
					return items, err
				}
				return items, ctx.Err()
			case <-timer.C:
			}
			if err = d.cleanupRefusal(); err != nil {
				return items, err
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
