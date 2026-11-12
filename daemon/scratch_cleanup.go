package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/scratchfs"
	"github.com/xleelz/fae/worktree"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type ScratchCleanupParams struct {
	toolRoot                                       string
	Home, Instance, DoltBinary, AuthorizingMessage string
	Apply                                          bool
}
type ScratchCleanupResult struct {
	SourceFiles map[string]string      `json:"source_files,omitempty"`
	Identity    ScratchIdentity        `json:"identity"`
	ArchivePath string                 `json:"archive_path"`
	Prepared    bool                   `json:"prepared"`
	Completed   bool                   `json:"completed"`
	Plans       []worktree.CleanupPlan `json:"plans"`
	Files       map[string]string      `json:"files,omitempty"`
}

func scratchDolt(ctx context.Context, binary, dir, envRoot string, args ...string) ([]byte, error) {
	if binary == "" {
		binary = "dolt"
	}
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = scopedEnv(envRoot)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("scratch Dolt %s: %w: %s", args[0], err, out)
	}
	return out, nil
}
func scratchRows(ctx context.Context, p ScratchCleanupParams, dir, envRoot, query string, out any) error {
	raw, err := scratchDolt(ctx, p.DoltBinary, dir, envRoot, "sql", "--result-format=json", "-q", query)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
func scratchAuthority(ctx context.Context, p ScratchCleanupParams, l Layout, id ScratchIdentity) error {
	if p.AuthorizingMessage == "" {
		return errors.New("authorizing message required")
	}
	q := fmt.Sprintf(`SELECT m.author,m.created_at,m.consumed_by,HEX(c.data) AS data FROM operator_messages m JOIN blob_chunks c ON c.digest=m.text_digest WHERE m.message_id=CONVERT(0x%s USING utf8mb4) ORDER BY c.chunk_index`, hex.EncodeToString([]byte(p.AuthorizingMessage)))
	var result struct {
		Rows []struct {
			Author   string  `json:"author"`
			Created  string  `json:"created_at"`
			Consumed *string `json:"consumed_by"`
			Data     string  `json:"data"`
		} `json:"rows"`
	}
	if err := scratchRows(ctx, p, l.Data, p.toolRoot, q, &result); err != nil {
		return err
	}
	if len(result.Rows) == 0 {
		return errors.New("operator authorization not found")
	}
	row := result.Rows[0]
	if row.Author != "operator" {
		return errors.New("operator authorization required")
	}
	var created time.Time
	var err error
	for _, format := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05"} {
		created, err = time.Parse(format, row.Created)
		if err == nil {
			break
		}
	}
	if err != nil {
		return err
	}
	age := time.Since(created)
	maxAge := id.MessageMaxAge
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	if age < 0 || age > maxAge {
		return errors.New("scratch authorization outside maximum age")
	}
	var text []byte
	for _, r := range result.Rows {
		part, e := hex.DecodeString(r.Data)
		if e != nil {
			return e
		}
		text = append(text, part...)
	}
	var message controlplane.DirectedMessage
	_ = json.Unmarshal(text, &message)
	includes := func(xs []string, s string) bool {
		for _, v := range xs {
			if v == s {
				return true
			}
		}
		return false
	}
	if (len(message.Authorize.Verbs) > 0 || len(message.Authorize.Classes) > 0) && !includes(message.Authorize.Verbs, "CleanupScratch") && !includes(message.Authorize.Classes, "runtime") {
		return errors.New("operator message restricts scratch cleanup")
	}
	var consumed []string
	if row.Consumed != nil && *row.Consumed != "" {
		if err = json.Unmarshal([]byte(*row.Consumed), &consumed); err != nil {
			return err
		}
	}
	for _, class := range consumed {
		if class != "runtime" && !(includes(message.Authorize.Classes, class) && includes(message.Authorize.Classes, "runtime")) {
			return errors.New("scratch message consumed by incompatible class")
		}
	}
	return nil
}
func scratchCopy(ctx context.Context, source, dest string) error {
	return filepath.WalkDir(source, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if e.IsDir() {
			return os.MkdirAll(target, 0700)
		}
		if e.Type()&os.ModeSymlink != 0 {
			return errors.New("scratch symlink refused")
		}
		if !e.Type().IsRegular() {
			info, e := os.Lstat(path)
			if e != nil {
				return e
			}
			if _, e = scratchIPC(source, path, info); e != nil {
				return e
			}
			return scratchIPCMarkers(source, path)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, &backupReader{ctx: ctx, r: in})
		syncErr := out.Sync()
		closeErr := out.Close()
		return errors.Join(err, syncErr, closeErr)
	})
}
func scratchSave(home string, r ScratchCleanupResult) error {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	target := scratchJournalPath(home, r.Identity.Instance)
	f, err := os.CreateTemp(filepath.Dir(target), ".journal-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), target); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func scratchVerify(ctx context.Context, r ScratchCleanupResult) error {
	if len(r.Identity.ID) != 32 {
		return errors.New("invalid scratch journal identity")
	}
	if _, err := hex.DecodeString(r.Identity.ID); err != nil {
		return err
	}
	home := filepath.Dir(r.Identity.Root)
	expected, err := Paths(home, r.Identity.Instance)
	if err != nil || expected.Root != r.Identity.Root {
		return errors.New("invalid scratch journal root")
	}
	parent := filepath.Join(home, ".scratch-archive")
	if filepath.Dir(r.ArchivePath) != parent || !strings.HasPrefix(filepath.Base(r.ArchivePath), r.Identity.ID+"-") {
		return errors.New("foreign scratch backup path")
	}
	physical, err := filepath.EvalSymlinks(r.ArchivePath)
	if err != nil || physical != r.ArchivePath {
		return errors.New("nonphysical scratch backup")
	}
	files, err := scratchFiles(ctx, r.ArchivePath)
	if err != nil {
		return err
	}
	if len(files) == 0 || len(files) != len(r.Files) {
		return errors.New("scratch backup changed")
	}
	for p, hash := range files {
		if r.Files[p] != hash {
			return errors.New("scratch backup checksum mismatch")
		}
	}
	return nil
}
func scratchPlans(ctx context.Context, p ScratchCleanupParams, l Layout, id ScratchIdentity) ([]worktree.CleanupPlan, error) {
	manager := &worktree.Manager{Repository: id.Workspace, Root: l.Worktrees}
	plans := []worktree.CleanupPlan{}
	known := map[string]bool{}
	var jobs struct {
		Rows []struct {
			ID     string `json:"job_id"`
			Digest string `json:"payload_digest"`
		} `json:"rows"`
	}
	if err := scratchRows(ctx, p, l.Data, p.toolRoot, `SELECT job_id,payload_digest FROM jobs LIMIT 1001`, &jobs); err != nil {
		return nil, err
	}
	if len(jobs.Rows) > 1000 {
		return nil, errors.New("scratch cleanup job bound exceeded")
	}
	readBlob := func(digest string) ([]byte, error) {
		if len(digest) != 64 {
			return nil, errors.New("invalid blob digest")
		}
		if _, e := hex.DecodeString(digest); e != nil {
			return nil, e
		}
		var parts struct {
			Rows []struct {
				Data string `json:"data"`
			} `json:"rows"`
		}
		if e := scratchRows(ctx, p, l.Data, p.toolRoot, "SELECT HEX(data) AS data FROM blob_chunks WHERE digest='"+digest+"' ORDER BY chunk_index", &parts); e != nil {
			return nil, e
		}
		var raw []byte
		for _, part := range parts.Rows {
			x, e := hex.DecodeString(part.Data)
			if e != nil {
				return nil, e
			}
			raw = append(raw, x...)
		}
		if controlplane.Digest(raw) != digest {
			return nil, errors.New("scratch blob corrupt")
		}
		return raw, nil
	}
	add := func(run, task string) error {
		binding := manager.Binding(run, task)
		if known[binding.Path] {
			return nil
		}
		known[binding.Path] = true
		plan, e := manager.PlanCleanup(ctx, run, task)
		if e != nil {
			return e
		}
		plans = append(plans, plan)
		return nil
	}
	for _, j := range jobs.Rows {
		raw, e := readBlob(j.Digest)
		if e != nil {
			return nil, e
		}
		var env envelope
		if e = json.Unmarshal(raw, &env); e != nil {
			return nil, e
		}
		if env.Job.TaskID != j.ID {
			return nil, errors.New("scratch task binding mismatch")
		}
		if env.Job.ExecutionKind == "live" {
			if e = add(env.Admission.RunID, j.ID); e != nil {
				return nil, e
			}
		}
	}
	// Durable loop schedules own provider-loop worktrees even without ordinary jobs.
	var markers struct {
		Rows []struct {
			Digest string `json:"payload_digest"`
		} `json:"rows"`
	}
	if e := scratchRows(ctx, p, l.Data, p.toolRoot, "SELECT payload_digest FROM daemon_markers WHERE name='provider-loops'", &markers); e != nil {
		return nil, e
	}
	for _, m := range markers.Rows {
		raw, e := readBlob(m.Digest)
		if e != nil {
			return nil, e
		}
		var schedules map[string]struct {
			SessionID string `json:"session_id"`
		}
		if e = json.Unmarshal(raw, &schedules); e != nil {
			return nil, e
		}
		for _, schedule := range schedules {
			binding := manager.Binding("provider-loops", schedule.SessionID)
			if _, e = os.Lstat(binding.Path); e == nil {
				if e = add("provider-loops", schedule.SessionID); e != nil {
					return nil, e
				}
			}
		}
	}
	var reviewMarkers struct {
		Rows []struct {
			Digest string `json:"payload_digest"`
		} `json:"rows"`
	}
	if err := scratchRows(ctx, p, l.Data, p.toolRoot, "SELECT payload_digest FROM daemon_markers WHERE name LIKE 'review-workspace/%' LIMIT 1001", &reviewMarkers); err != nil {
		return nil, err
	}
	if len(reviewMarkers.Rows) > 1000 {
		return nil, errors.New("scratch review marker bound exceeded")
	}
	for _, marker := range reviewMarkers.Rows {
		raw, e := readBlob(marker.Digest)
		if e != nil {
			return nil, e
		}
		var record struct {
			Task      string `json:"task"`
			RunID     string `json:"run_id"`
			Workspace string `json:"workspace"`
			Branch    string `json:"branch"`
			State     string `json:"state"`
		}
		if e = json.Unmarshal(raw, &record); e != nil {
			return nil, e
		}
		expected := manager.Binding(record.RunID, record.Task)
		if expected.Path != record.Workspace || expected.Branch != record.Branch || (record.State != "pending" && record.State != "ready") {
			return nil, errors.New("scratch review workspace ownership mismatch")
		}
		_, pathErr := os.Lstat(expected.Path)
		_, branchErr := controller.Git(ctx, id.Workspace, "rev-parse", "--verify", "refs/heads/"+expected.Branch)
		if pathErr == nil || branchErr == nil {
			if e = add(record.RunID, record.Task); e != nil {
				return nil, e
			}
		} else if !os.IsNotExist(pathErr) {
			return nil, pathErr
		}
	}
	entries, err := os.ReadDir(l.Worktrees)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.Name() == ".manager.lock" {
			continue
		}
		if !known[filepath.Join(l.Worktrees, entry.Name())] {
			return nil, errors.New("unknown scratch worktree entry")
		}
	}
	return plans, nil
}

// CleanupScratch is a local controller operation. The lifecycle lock is outside
// the removable root and serializes recreation with every cleanup/retry.
func CleanupScratch(ctx context.Context, p ScratchCleanupParams) (ScratchCleanupResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var r ScratchCleanupResult
	l, err := Paths(p.Home, p.Instance)
	if err != nil {
		return r, err
	}
	serialize := lifecycleLock
	if !p.Apply {
		serialize = lifecyclePreviewLock // a preview creates no lifecycle state
	}
	unlock, err := serialize(ctx, p.Home, p.Instance)
	if err != nil {
		return r, err
	}
	defer unlock()
	journal := scratchJournalPath(p.Home, p.Instance)
	if _, err = os.Lstat(l.Root); os.IsNotExist(err) {
		if err = checkPrivateRegular(journal); err != nil {
			return r, err
		}
		raw, e := os.ReadFile(journal)
		if e != nil {
			return r, e
		}
		if e = json.Unmarshal(raw, &r); e != nil {
			return r, e
		}
		if !r.Prepared || r.Identity.Root != l.Root {
			return r, errors.New("missing scratch root without prepared evidence")
		}
		if e = scratchVerify(ctx, r); e != nil {
			return r, e
		}
		if p.Apply {
			err = scratchFinish(ctx, p, r)
			r.Completed = err == nil
		}
		return r, err
	} else if err != nil {
		return r, err
	}
	id, err := scratchIdentity(l)
	if err != nil {
		return r, fmt.Errorf("scratch instance required: %w", err)
	}
	r.Identity = id
	if !p.Apply {
		return r, nil
	}
	archiveRoot := filepath.Join(p.Home, ".scratch-archive")
	if err = mkdirPrivate(archiveRoot); err != nil {
		return r, err
	}
	p.toolRoot, err = os.MkdirTemp(archiveRoot, ".tools-")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(p.toolRoot)
	// Resume only an exact identity journal; never adopt old-name backups.
	if raw, e := os.ReadFile(journal); e == nil {
		if e = checkPrivateRegular(journal); e != nil {
			return r, e
		}
		var prior ScratchCleanupResult
		if e = json.Unmarshal(raw, &prior); e != nil {
			return r, e
		}
		if prior.Identity.ID == id.ID {
			r = prior
			if r.Completed {
				return r, errors.New("completed scratch identity reappeared")
			}
		} else if !prior.Completed {
			return r, errors.New("another scratch cleanup identity pending")
		}
	} else if !os.IsNotExist(e) {
		return r, e
	}
	// Shutdown transport validates the operator's runtime authority. An unreachable
	// daemon is acceptable only when its exclusive lock can be acquired below.
	if token, e := os.ReadFile(l.Credential); e == nil {
		params, _ := json.Marshal(map[string]string{"authorizing_message": p.AuthorizingMessage})
		_, authErr := api.Call(ctx, l.Socket, token, api.Request{Version: 1, Method: "ScratchCleanupAuthorize", Params: params})
		if authErr != nil {
			var connectionErr *net.OpError
			if !errors.As(authErr, &connectionErr) {
				return r, authErr
			}
		}
		_, callErr := api.Call(ctx, l.Socket, token, api.Request{Version: 1, Method: "Shutdown", Params: params})
		if callErr != nil {
			var rpcErr *api.RPCError
			if errors.As(callErr, &rpcErr) {
				return r, callErr
			}
		}
	}
	var lock *instanceLock
	deadline := time.Now().Add(30 * time.Second)
	for {
		lock, err = acquire(filepath.Join(l.Run, "lock"))
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return r, fmt.Errorf("scratch daemon still owned: %w", err)
		}
		select {
		case <-ctx.Done():
			return r, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	defer lock.close()
	if err = scratchAuthority(ctx, p, l, id); err != nil {
		return r, err
	}
	for _, owned := range [][2]string{{filepath.Join(l.Run, "plane-processes"), l.Data}, {filepath.Join(l.Run, "effect-processes"), l.Worktrees}, {filepath.Join(l.Root, "sessions", ".processes"), filepath.Join(l.Root, "sessions")}} {
		if _, err = processowner.Recover(ctx, owned[0], owned[1]); err != nil {
			return r, err
		}
	}
	// No unknown symlinks or device files may be traversed or removed.
	err = filepath.WalkDir(l.Root, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Type()&os.ModeSymlink != 0 {
			return errors.New("scratch symlink refused")
		}
		return nil
	})
	if err != nil {
		return r, err
	}
	if !r.Prepared {
		r.ArchivePath, err = os.MkdirTemp(archiveRoot, id.ID+"-")
		if err != nil {
			return r, err
		}
		if err = scratchSave(p.Home, r); err != nil {
			return r, err
		}
		r.Plans, err = scratchPlans(ctx, p, l, id)
		if err != nil {
			return r, err
		}
		remote := filepath.Join(r.ArchivePath, "plane-remote")
		if _, err = scratchDolt(ctx, p.DoltBinary, l.Data, p.toolRoot, "backup", "sync-url", (&url.URL{Scheme: "file", Path: remote}).String()); err != nil {
			return r, err
		}
		for _, pair := range [][2]string{{"user.name", "Fae scratch backup"}, {"user.email", "fae@localhost"}} {
			if _, err = scratchDolt(ctx, p.DoltBinary, r.ArchivePath, r.ArchivePath, "config", "--global", "--add", pair[0], pair[1]); err != nil {
				return r, err
			}
		}
		if _, err = scratchDolt(ctx, p.DoltBinary, r.ArchivePath, r.ArchivePath, "backup", "restore", (&url.URL{Scheme: "file", Path: remote}).String(), "plane-restored"); err != nil {
			return r, err
		}
		if _, err = scratchDolt(ctx, p.DoltBinary, filepath.Join(r.ArchivePath, "plane-restored"), r.ArchivePath, "fsck"); err != nil {
			return r, err
		}
		if err = scratchCopy(ctx, l.Root, filepath.Join(r.ArchivePath, "instance-files")); err != nil {
			return r, err
		}
		r.SourceFiles, err = scratchFiles(ctx, l.Root)
		if err != nil {
			return r, err
		}
		endpoints := map[string]string{}
		for path, value := range r.SourceFiles {
			if strings.HasPrefix(value, "ipc:v1:") {
				endpoints[path] = value
			}
		}
		metadata, e := json.MarshalIndent(endpoints, "", "  ")
		if e != nil {
			return r, e
		}
		if e = os.WriteFile(filepath.Join(r.ArchivePath, "runtime-endpoints.json"), metadata, 0600); e != nil {
			return r, e
		}
		endpointFile, e := os.OpenFile(filepath.Join(r.ArchivePath, "runtime-endpoints.json"), os.O_RDWR, 0)
		if e != nil {
			return r, e
		}
		e = endpointFile.Sync()
		closeErr := endpointFile.Close()
		if e != nil || closeErr != nil {
			return r, errors.Join(e, closeErr)
		}
		r.Files, err = scratchFiles(ctx, r.ArchivePath)
		if err != nil {
			return r, err
		}
		r.Prepared = true
		if err = scratchSave(p.Home, r); err != nil {
			return r, err
		}
	}
	if err = scratchVerify(ctx, r); err != nil {
		return r, err
	}
	manager := &worktree.Manager{Repository: id.Workspace, Root: l.Worktrees}
	verify := func(ctx context.Context) error {
		current, e := scratchIdentity(l)
		if e != nil {
			return e
		}
		if current != id {
			return errors.New("scratch identity changed")
		}
		if e = scratchVerify(ctx, r); e != nil {
			return e
		}
		return scratchSourceUnchanged(ctx, l, r)
	}
	plans := []worktree.CleanupPlan{}
	for _, old := range r.Plans {
		now, e := manager.PlanCleanup(ctx, old.Run, old.Task)
		if e != nil {
			return r, e
		}
		if !now.AlreadyRemoved && now.Head != old.Head {
			return r, errors.New("scratch branch changed since backup")
		}
		plans = append(plans, now)
	}
	for _, plan := range plans {
		if err = manager.ArchiveCleanup(ctx, plan, verify); err != nil {
			return r, err
		}
	}
	for _, plan := range plans {
		if err = manager.Cleanup(ctx, plan, verify); err != nil {
			return r, err
		}
	}
	if err = verify(ctx); err != nil {
		return r, err
	}
	if !CanRemoveScratch(true, r.Prepared) {
		return r, errors.New("scratch backup prerequisite missing")
	}
	// The verified sibling archive is never below the removable root.
	stagingRoot := filepath.Join(p.Home, ".scratch-removing")
	if err = mkdirPrivate(stagingRoot); err != nil {
		return r, err
	}
	staging := filepath.Join(stagingRoot, id.ID)
	if _, e := os.Lstat(staging); !os.IsNotExist(e) {
		return r, errors.New("scratch staging path already exists")
	}
	if err = os.Rename(l.Root, staging); err != nil {
		return r, err
	}
	if err = scratchFinish(ctx, p, r); err != nil {
		return r, err
	}
	r.Completed = true
	return r, nil
}

func scratchSourceUnchanged(ctx context.Context, l Layout, r ScratchCleanupResult) error {
	current, err := scratchFiles(ctx, l.Root)
	if err != nil {
		return err
	}
	filtered := func(files map[string]string) map[string]string {
		out := map[string]string{}
		for p, h := range files {
			if p == "run/lock" || strings.HasPrefix(p, "worktrees/") {
				continue
			}
			out[p] = h
		}
		return out
	}
	want, got := filtered(r.SourceFiles), filtered(current)
	if len(want) != len(got) {
		return errors.New("scratch source files changed after backup")
	}
	for p, h := range got {
		if want[p] != h {
			return errors.New("scratch source file changed after backup")
		}
	}
	return nil
}
func scratchFinish(ctx context.Context, p ScratchCleanupParams, r ScratchCleanupResult) error {
	if err := scratchVerify(ctx, r); err != nil {
		return err
	}
	if err := scratchArchiveGitProof(ctx, r); err != nil {
		return err
	}
	stageRoot := filepath.Join(p.Home, ".scratch-removing")
	stage := filepath.Join(stageRoot, r.Identity.ID)
	info, err := os.Lstat(stage)
	if err == nil {
		physical, e := filepath.EvalSymlinks(stageRoot)
		if e != nil || physical != stageRoot {
			return errors.New("scratch staging root changed")
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.IsDir() || uint64(st.Ino) != r.Identity.Inode || uint64(st.Dev) != r.Identity.Device || st.Uid != uint32(os.Getuid()) {
			return errors.New("scratch staging identity changed")
		}
		remaining, e := scratchFiles(ctx, stage)
		if e != nil {
			return e
		}
		for path, hash := range remaining {
			if path == "run/lock" {
				continue
			}
			if r.SourceFiles[path] != hash {
				return fmt.Errorf("staged scratch content changed after backup: %s", path)
			}
		}
		// A test's read-only tree (Go's module cache) in the staged
		// instance is removed too, never through a link out of it.
		if err = scratchfs.RemoveAll(stage); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	r.Completed = true
	return scratchSave(p.Home, r)
}

// Plane backup manifests omit their own verified.json; a scratch snapshot has no
// such exception, because every instance file must remain recoverable.
func scratchFiles(ctx context.Context, root string) (map[string]string, error) {
	files := map[string]string{}
	var endpoints []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			value, e := scratchIPC(root, path, info)
			if e != nil {
				return e
			}
			files[rel] = value
			endpoints = append(endpoints, path)
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, &backupReader{ctx: ctx, r: f})
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr)
		}
		files[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = scratchIPCUnused(ctx, endpoints); err != nil {
		return nil, err
	}
	return files, nil
}

func scratchArchiveGitProof(ctx context.Context, r ScratchCleanupResult) error {
	for _, plan := range r.Plans {
		branch := "refs/heads/" + plan.Binding.Branch
		if _, e := controller.Git(ctx, r.Identity.Workspace, "rev-parse", "--verify", branch); e == nil {
			return errors.New("scratch task branch still exists before final deletion")
		}
		if plan.AlreadyRemoved {
			prefix := "refs/fae/archive/" + worktree.Identity(plan.Run, plan.Task) + "/"
			refs, e := controller.Git(ctx, r.Identity.Workspace, "for-each-ref", "--format=%(refname)", prefix)
			if e != nil || refs == "" {
				return errors.New("scratch archive evidence missing")
			}
			for _, ref := range strings.Split(refs, "\n") {
				sha := ref[strings.LastIndex(ref, "/")+1:]
				if _, e = controller.Git(ctx, r.Identity.Workspace, "symbolic-ref", "-q", ref); e == nil {
					return errors.New("symbolic scratch archive")
				}
				got, e := controller.Git(ctx, r.Identity.Workspace, "rev-parse", "--verify", ref+"^{commit}")
				if e != nil || got != sha {
					return errors.New("scratch archive mismatch")
				}
			}
		} else {
			if _, e := controller.Git(ctx, r.Identity.Workspace, "symbolic-ref", "-q", plan.Archive); e == nil {
				return errors.New("symbolic scratch archive")
			}
			got, e := controller.Git(ctx, r.Identity.Workspace, "rev-parse", "--verify", plan.Archive+"^{commit}")
			if e != nil || got != plan.Head {
				return errors.New("scratch archive changed")
			}
		}
	}
	return nil
}
