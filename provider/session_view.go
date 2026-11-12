package provider

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/scratchfs"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

type SessionView struct {
	ownedPrivate    string
	lastChanged     []string
	lastDigest      string
	children        []*SessionView
	mu              sync.Mutex
	guard           *ReadGuard
	source, private string
	before          DirectorySnapshot
}

// ConfigureSessionView creates only private files. Mounts are installed in the
// child's namespace; a failed launch or killed daemon never alters shared data.
func (g *ReadGuard) ConfigureSessionView(cmd *exec.Cmd, logDir string, writable []string) (*SessionView, error) {
	started := time.Now()
	view := &SessionView{guard: g}
	defer func() {
		if logDir != "" {
			stats := map[string]any{"construction_ms": time.Since(started).Milliseconds(), "copied_bytes": viewCopiedBytes(view.ownedPrivate), "namespace": view.ownedPrivate != ""}
			raw, _ := json.Marshal(stats)
			_ = os.WriteFile(filepath.Join(logDir, "filesystem-view.json"), raw, 0600)
		}
	}()
	if g == nil || g.Worktree == "" {
		return view, nil
	}
	restricted := readPolicyNarrows(g.Policy) || len(g.DenyPaths) > 0 || g.WritableWorktree
	if !g.ForceIsolation && (g.RuntimeShellWithoutView || !RequiresSessionIsolation(g.HasShell, g.Command, g.Policy, g.DenyPaths, g.ReadRoots)) {
		return view, nil
	}
	for _, entry := range cmd.Env {
		if path, ok := strings.CutPrefix(entry, "PATH="); ok {
			parts := filepath.SplitList(path)
			if len(parts) > 0 && filepath.Base(parts[0]) == "command-shims" {
				policy, policyErr := commandshim.ReadRoleShims(filepath.Join(parts[0], ".policy.json"))
				if policyErr != nil && !os.IsNotExist(policyErr) {
					return nil, fmt.Errorf("session filesystem isolation unavailable: invalid shim policy: %w", policyErr)
				}
				if policyErr == nil && !policy.Accounted && policy.LeaseProxy == "" && (policy.Build || policy.Test || policy.Format) {
					return nil, fmt.Errorf("session filesystem isolation unavailable: heavy-operation host ownership in PID namespaces is not supported")
				}
			}
		}
	}
	binary, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, fmt.Errorf("session filesystem isolation unavailable: bubblewrap is required for restricted reads or read roots")
	}
	if err = exec.Command(binary, "--unshare-user", "--unshare-pid", "--cap-drop", "ALL", "--proc", "/proc", "--ro-bind", "/", "/", "--", "true").Run(); err != nil {
		return nil, fmt.Errorf("session filesystem isolation unavailable: user/mount namespaces: %w", err)
	}
	if logDir == "" || !filepath.IsAbs(logDir) {
		return nil, fmt.Errorf("session filesystem isolation unavailable: an absolute on-disk session log directory is required")
	}
	private, err := os.MkdirTemp(logDir, "filesystem-view-")
	if err != nil {
		return nil, err
	}
	if err = scratchfs.RegisterSessionCopy(private, logDir); err != nil {
		_ = scratchfs.RemoveSessionCopy(private)
		return nil, err
	}
	view.ownedPrivate = private
	built := false
	defer func() {
		if !built {
			_ = view.Close()
		}
	}()
	mounts := map[string]string{}
	if restricted {
		view.guard = g
		view.source = g.Worktree
		view.private = filepath.Join(private, "worktree")
		if err = copySessionTree(g.Worktree, view.private, g.AllowPath); err != nil {
			return nil, err
		}
		if isGitWorktree(g.Worktree) {
			if err = createSessionRepository(view.private); err != nil {
				return nil, err
			}
		}
		view.before, err = snapshotViewDirectory(view.private)
		if err != nil {
			return nil, err
		}
		mounts[g.Worktree] = view.private
	} else {
		mounts[g.Worktree] = g.Worktree
	}
	writeViews := map[string]string{}
	for path, canWrite := range g.WritableViews {
		key := fmt.Sprintf("%x", sha256.Sum256([]byte(path)))
		target := filepath.Join(private, "writable", key)
		if err = copySessionTree(path, target, g.AllowPath); err != nil {
			return nil, err
		}
		snapshot, snapshotErr := snapshotViewDirectory(target)
		if snapshotErr != nil {
			return nil, snapshotErr
		}
		childGuard := *g
		childGuard.Worktree = path
		childGuard.CanWrite = canWrite
		childGuard.WritableViews = nil
		child := &SessionView{guard: &childGuard, source: path, private: target, before: snapshot}
		view.children = append(view.children, child)
		writeViews[path] = target
	}
	roots := map[string]string{}
	for name, root := range g.ReadRoots {
		rootKey := fmt.Sprintf("%x", sha256.Sum256([]byte(name)))
		target := filepath.Join(private, "roots", rootKey)
		if !readPolicyNarrows(g.Policy) && len(g.DenyPaths) == 0 && len(root.Deny) == 0 {
			roots[root.Path] = root.Path
			continue
		}
		if err = copySessionTree(root.Path, target, func(rel string) bool { return g.AllowReadRootPath(name, rel) }); err != nil {
			return nil, err
		}
		roots[root.Path] = target
	}
	// A fresh root prevents shell access to other task and host data. Only the
	// OS runtime, session logs, the worktree and explicitly granted roots exist.
	args := []string{"--unshare-user", "--unshare-pid", "--cap-drop", "ALL", "--die-with-parent", "--new-session", "--proc", "/proc", "--dev", "/dev", "--tmpfs", "/tmp"}
	for _, path := range []string{"/usr/bin", "/usr/sbin", "/usr/lib", "/usr/lib64", "/bin", "/sbin", "/lib", "/lib64"} {
		if _, e := os.Stat(path); e == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	for _, path := range []string{"/etc/ssl", "/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/passwd", "/etc/group", "/etc/localtime"} {
		if _, e := os.Stat(path); e == nil {
			args = append(args, "--ro-bind", path, path)
		}
	}
	// Hide the source repository's shared object store even if an operator
	// keeps the checkout underneath a system runtime mount.
	if isGitWorktree(g.Worktree) {
		git := exec.Command(systemGitBin(), "-C", g.Worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
		git.Env = cleanGitEnv()
		if output, e := git.Output(); e == nil {
			common := filepath.Clean(strings.TrimSpace(string(output)))
			if filepath.IsAbs(common) && common != "/" {
				args = append(args, "--tmpfs", common)
			}
		}
	}
	bind := func(source, target string, readOnly bool) {
		flag := "--bind"
		if readOnly {
			flag = "--ro-bind"
		}
		args = append(args, flag, source, target)
	}
	if home, homeErr := os.UserHomeDir(); homeErr == nil && filepath.IsAbs(home) && home != "/" {
		privateHome := filepath.Join(private, "provider-home")
		if err = os.MkdirAll(privateHome, 0700); err != nil {
			return nil, err
		}
		bind(privateHome, home, false)
	}
	for _, path := range g.RuntimeReadDirs {
		if path != "" {
			bind(path, path, true)
		}
	}
	for target, source := range mounts {
		bind(source, target, !restricted && target == g.Worktree)
	}
	if view.private != "" {
		gitDir := filepath.Join(view.private, ".git")
		if info, e := os.Stat(gitDir); e == nil && info.IsDir() {
			bind(gitDir, filepath.Join(g.Worktree, ".git"), true)
		}
	}

	rootTargets := []string{}
	for target := range roots {
		rootTargets = append(rootTargets, target)
	}
	sort.Slice(rootTargets, func(i, j int) bool { return len(rootTargets[i]) < len(rootTargets[j]) })
	for _, target := range rootTargets {
		bind(roots[target], target, true)
	}
	for path, privatePath := range writeViews {
		bind(privatePath, path, false)
	}
	for _, path := range writable {
		if path != "" {
			if _, private := writeViews[path]; !private {
				bind(path, path, false)
			}
		}
	}
	if logDir != "" {
		bind(logDir, logDir, true)
		for _, name := range []string{"tool-events.jsonl", "shell-refusals.jsonl", "refused-reads.jsonl"} {
			path := filepath.Join(logDir, name)
			file, e := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if e != nil {
				return nil, e
			}
			if e = file.Close(); e != nil {
				return nil, e
			}
			bind(path, path, false)
		}
	}
	// Install writable session state after the read-only log mount. The runtime
	// owns these dedicated children; never grant an ambient HOME directory.
	if err = bindSessionState(cmd, logDir, private, bind); err != nil {
		return nil, err
	}
	for configured, privateCache := range g.RuntimeCacheDirs {
		bind(privateCache, configured, false)
		bind(privateCache, privateCache, false)
	}
	// The already-registered host shim policy names its exact operational
	// resources; grant those paths, never their parent instance directories.
	for _, entry := range cmd.Env {
		if path, ok := strings.CutPrefix(entry, "PATH="); ok {
			parts := filepath.SplitList(path)
			if len(parts) > 0 && filepath.Base(parts[0]) == "command-shims" {
				shimDir := parts[0]
				bind(shimDir, shimDir, true)
				if policy, e := commandshim.ReadRoleShims(filepath.Join(shimDir, ".policy.json")); e == nil {
					if policy.Broker != "" {
						bind(policy.Broker, policy.Broker, true)
					}
					if policy.LeaseProxy != "" {
						bind(policy.LeaseProxy, policy.LeaseProxy, false)
					} else if policy.Ledger != "" {
						bind(policy.Ledger, policy.Ledger, false)
					}
					// The GOROOT mirror contains links to the pinned real toolchain.
					// Mount that runtime as well; binding the mirror alone leaves
					// its compiler, standard library and tools unreachable.
					if root := commandshim.GoRoot(policy.Tools["go"], nil); root != "" {
						bind(root, root, true)
					}
					for _, tool := range policy.Tools {
						if resolved, e := filepath.EvalSymlinks(tool); e == nil {
							bind(resolved, tool, true)
						}
					}
					if policy.NestedGo != nil {
						if policy.NestedGo.GoRoot != "" {
							bind(policy.NestedGo.GoRoot, policy.NestedGo.GoRoot, true)
						}
						for _, path := range policy.NestedGo.Env {
							if filepath.IsAbs(path) {
								if info, e := os.Stat(path); e == nil && info.IsDir() {
									bind(path, path, false)
								}
							}
						}
					}
				}
			}
		}
	}
	for _, arg := range cmd.Args {
		if path, ok := strings.CutPrefix(arg, "--settings="); ok && filepath.IsAbs(path) {
			bind(path, path, true)
		}
	}
	for _, path := range g.WritableFiles {
		if path != "" {
			file, e := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if e != nil {
				return nil, e
			}
			if e = file.Close(); e != nil {
				return nil, e
			}
			bind(path, path, false)
		}
	}
	executable, err := exec.LookPath(cmd.Path)
	if err != nil {
		return nil, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	bind(executable, executable, true)
	// Exercise the actual mount plan with a host-only canary before any
	// provider code runs, including /proc root aliases of PID 1 and self.
	canary, canaryErr := os.CreateTemp(filepath.Dir(logDir), ".namespace-canary-")
	if canaryErr != nil {
		return nil, canaryErr
	}
	canaryPath := canary.Name()
	if err = canary.Close(); err != nil {
		return nil, err
	}
	defer os.Remove(canaryPath)
	probeArgs := append([]string{}, args...)
	probeArgs = append(probeArgs, "--chdir", cmd.Dir, "--", "/bin/sh", "-c", `if [ -e "$1" ] || [ -e "/proc/1/root$1" ] || [ -e "/proc/self/root$1" ]; then exit 77; fi`, "namespace-probe", canaryPath)
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelProbe()
	if err = exec.CommandContext(probeCtx, binary, probeArgs...).Run(); err != nil {
		return nil, fmt.Errorf("session filesystem isolation unavailable: actual mount plan did not hide host filesystem canary: %w", err)
	}
	args = append(args, "--chdir", cmd.Dir, "--", executable)
	args = append(args, cmd.Args[1:]...)
	cmd.Path = binary
	cmd.Args = append([]string{binary}, args...)
	built = true
	return view, nil
}

func copySessionTree(source, target string, allow func(string) bool) error {
	if err := PhysicalTaskDirectory(source); err != nil {
		return err
	}
	if err := os.MkdirAll(target, 0700); err != nil {
		return err
	}
	rooted, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer rooted.Close()
	return fs.WalkDir(rooted.FS(), ".", func(rel string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.Name() == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !allow(filepath.ToSlash(rel)) {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			resolved, e := filepath.EvalSymlinks(filepath.Join(source, rel))
			if e != nil {
				return e
			}
			inside, e := filepath.Rel(source, resolved)
			if e != nil || !physicalWithin(source, resolved) || !allow(filepath.ToSlash(inside)) {
				return fmt.Errorf("session view refuses escaping or denied symlink %s", rel)
			}
			dest := filepath.Join(target, rel)
			if e = os.MkdirAll(filepath.Dir(dest), 0700); e != nil {
				return e
			}
			rebased, e := filepath.Rel(filepath.Dir(dest), filepath.Join(target, inside))
			if e != nil {
				return e
			}
			return os.Symlink(rebased, dest)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("session view refuses special file %s", rel)
		}
		input, err := rooted.OpenFile(rel, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		defer input.Close()
		opened, err := input.Stat()
		if err != nil {
			return err
		}
		if !os.SameFile(info, opened) {
			return fmt.Errorf("session view source changed while opening %s", rel)
		}
		dest := filepath.Join(target, filepath.FromSlash(rel))
		if err = os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
			return err
		}
		output, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(output, input)
		closeErr := output.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
}

// Collect merges only this session's changes, refusing a concurrently changed
// source. Scope collection still independently checks every merged path.
func (v *SessionView) Collect() error {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	registry := ""
	if v.guard != nil {
		registry = v.guard.OutputOwnershipRegistry
	}
	unlock, lockErr := LockOutputOwnership(registry)
	if lockErr != nil {
		return lockErr
	}
	defer unlock()
	for _, child := range v.children {
		if err := child.Collect(); err != nil {
			return err
		}
	}
	if v.private == "" {
		return nil
	}
	after, err := snapshotViewDirectory(v.private)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for name := range v.before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	ordered := []string{}
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	for _, name := range ordered {
		before, was := v.before[name]
		next, exists := after[name]
		if was == exists && viewFileDigest(before) == viewFileDigest(next) && before.mode == next.mode {
			continue
		}
		if next.mode&os.ModeSymlink != 0 {
			return fmt.Errorf("session created symlink: %s", name)
		}
		if v.guard.CanWrite == nil || !v.guard.CanWrite(name) {
			return fmt.Errorf("session write outside effective scope: %s", name)
		}
		path, e := safePath(v.source, name)
		if e != nil {
			return e
		}
		current, e := fileDigest(path)
		if was {
			info, statErr := os.Stat(path)
			if statErr != nil || info.Mode().Perm() != before.mode.Perm() {
				return fmt.Errorf("session view source mode changed concurrently: %s", name)
			}
		}
		if was && (e != nil || current != viewFileDigest(before)) {
			return fmt.Errorf("session view source changed concurrently: %s", name)
		}
		if !was && e == nil {
			return fmt.Errorf("session view source created concurrently: %s", name)
		}
		if !exists {
			if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
				return e
			}
			continue
		}
		if e = EnsurePhysicalTaskDirectory(filepath.Dir(path)); e != nil {
			return e
		}
		if e = copyCollectedFile(filepath.Join(v.private, name), path, next.mode, viewFileDigest(next)); e != nil {
			return e
		}
	}
	changed, digest, e := digestViewChanges(v.private, v.before, after, ordered)
	if e != nil {
		return e
	}
	v.lastChanged, v.lastDigest = changed, digest
	v.before = after
	return nil
}

func createSessionRepository(root string) error {
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "--all", "--force"}, {"-c", "user.name=Fae session view", "-c", "user.email=session@fae.invalid", "commit", "--quiet", "--allow-empty", "-m", "session-visible snapshot"}} {
		cmd := exec.Command(systemGitBin(), append([]string{"-C", root, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false"}, args...)...)
		cmd.Env = cleanGitEnv("GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("session-visible git snapshot: %w: %s", err, out)
		}
	}
	return nil
}

func (v *SessionView) DirectoryEvidence(path string) ([]string, string, bool) {
	if v == nil {
		return nil, "", false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, child := range v.children {
		if child.source == path {
			return append([]string(nil), child.lastChanged...), child.lastDigest, true
		}
	}
	return nil, "", false
}

// bindSessionState keeps the runtime-owned HOME and temporary directory durable
// across checkpoints without making the session log/config files writable.
func bindSessionState(cmd *exec.Cmd, logDir, private string, bind func(string, string, bool)) error {
	stateHome := ""
	for _, entry := range cmd.Env {
		if value, ok := strings.CutPrefix(entry, "FAE_PROVIDER_STATE_HOME="); ok {
			stateHome = value
		}
	}

	for _, entry := range cmd.Env {
		key, path, ok := strings.Cut(entry, "=")
		if !ok || path == "" {
			continue
		}
		switch key {
		case "HOME", "TMPDIR":
			if !filepath.IsAbs(path) || (path != stateHome && (filepath.Dir(filepath.Clean(path)) != filepath.Clean(logDir) || (filepath.Base(path) != "home" && filepath.Base(path) != "tmp"))) {
				continue
			}
			info, err := os.Lstat(path)
			if err != nil {
				return err
			}
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("invalid session state directory %s", path)
			}
			bind(path, path, false)
		case "CLAUDE_CONFIG_DIR", "CODEX_HOME":
			if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
				return fmt.Errorf("provider configuration must name an absolute configuration directory")
			}
			if err := PhysicalTaskDirectory(path); err != nil {
				return fmt.Errorf("Claude configuration: %w", err)
			}
			if syscall.Access(path, 2) != nil {
				return fmt.Errorf("provider configuration directory is not writable")
			}
			bind(path, path, false)
		}
	}
	return nil
}

// Close removes only the private view created by this launch, never its sources.
func (v *SessionView) Close() error {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ownedPrivate == "" {
		return nil
	}
	path := v.ownedPrivate
	v.ownedPrivate = ""
	return scratchfs.RemoveSessionCopy(path)
}

func viewCopiedBytes(root string) int64 {
	var count int64
	if root != "" {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if entry.Type().IsRegular() {
				if info, e := entry.Info(); e == nil {
					count += info.Size()
				}
			}
			return nil
		})
	}
	return count
}

func (v *SessionView) Isolated() bool { return v != nil && v.ownedPrivate != "" }
