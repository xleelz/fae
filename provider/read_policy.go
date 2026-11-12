package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/xleelz/fae/workflow"
)

// ReadRootView describes a verified external read root exposed to a role session (FAE-RT-016).
type ReadRootView struct {
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	Commit string   `json:"commit,omitempty"`
	Digest string   `json:"digest,omitempty"`
	Deny   []string `json:"deny,omitempty"`
}

// RefusedReadEvent records a single refused read attempt (FAE-SEC-008).
type RefusedReadEvent struct {
	Task string `json:"task"`
	Role string `json:"role"`
	Path string `json:"path"`
}

// ReadGuard enforces FAE-SEC-008 (ReadPolicy), FAE-SEC-009 (DenyPaths), and
// FAE-RT-016 (external ReadRoots confinement and read-only enforcement) across
// briefs, repository listings, agentic tools, and shell views.
type ReadGuard struct {
	OutputOwnershipRegistry string
	WritableWorktree        bool
	RuntimeCacheDirs        map[string]string
	RuntimeShellWithoutView bool
	HasShell                bool
	Command                 bool
	RuntimeReadDirs         []string
	WritableFiles           []string
	WritableViews           map[string]func(string) bool
	ForceIsolation          bool
	CanWrite                func(string) bool
	TaskID                  string
	Role                    string
	Worktree                string
	ReadRoots               map[string]ReadRootView
	Policy                  *workflow.ReadPolicy
	DenyPaths               []string
	LogDirectory            string
	OnRefusedRead           func(task, role, path string)
}

func (g *ReadGuard) auditRefusal(relPath string) {
	if g == nil {
		return
	}
	clean := strings.TrimPrefix(strings.ReplaceAll(strings.TrimSpace(relPath), "\\", "/"), "./")
	if g.OnRefusedRead != nil {
		g.OnRefusedRead(g.TaskID, g.Role, clean)
	}
	if g.LogDirectory != "" {
		raw, err := json.Marshal(RefusedReadEvent{Task: g.TaskID, Role: g.Role, Path: clean})
		if err == nil {
			f, err := os.OpenFile(filepath.Join(g.LogDirectory, "refused-reads.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err == nil {
				_, _ = f.Write(append(raw, '\n'))
				_ = f.Close()
			}
		}
	}
}

// ReadRefusedReadsLog reads any RefusedReadEvents recorded in logDir.
func ReadRefusedReadsLog(logDir string) []RefusedReadEvent {
	if logDir == "" {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(logDir, "refused-reads.jsonl"))
	if err != nil {
		return nil
	}
	var out []RefusedReadEvent
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ev RefusedReadEvent
		if json.Unmarshal([]byte(line), &ev) == nil && ev.Path != "" {
			out = append(out, ev)
		}
	}
	return out
}

// AllowPath reports whether relPath is permitted by Policy and DenyPaths (without auditing).
func (g *ReadGuard) AllowPath(relPath string) bool {
	if g == nil {
		return true
	}
	return workflow.AllowsPath(g.Policy, g.DenyPaths, relPath)
}

// AllowReadRootPath reports whether relPath inside named readRoot is permitted.
func (g *ReadGuard) AllowReadRootPath(rootName, relPath string) bool {
	if g == nil {
		return false
	}
	root, ok := g.ReadRoots[rootName]
	if !ok {
		return false
	}
	for _, d := range root.Deny {
		if workflow.MatchGlob(d, relPath) || workflow.MatchGlobFold(d, relPath) {
			return false
		}
	}
	if workflow.MatchDenyPathIndex(g.DenyPaths, relPath) >= 0 {
		return false
	}
	if g.Policy != nil {
		for _, d := range g.Policy.Deny {
			if workflow.MatchGlob(d, relPath) || workflow.MatchGlobFold(d, relPath) || workflow.MatchGlob(d, rootName+"/"+relPath) {
				return false
			}
		}
	}
	return g.Policy == nil || workflow.AllowsPath(g.Policy, g.DenyPaths, relPath) || workflow.AllowsPath(g.Policy, g.DenyPaths, rootName+"/"+relPath)
}

// FilterInlineFiles filters inline brief files through the effective read policy
// and audits any refused file (FAE-SEC-008).
func (g *ReadGuard) FilterInlineFiles(files []string) []string {
	if g == nil || (g.Policy == nil && len(g.DenyPaths) == 0) {
		return files
	}
	var kept []string
	for _, f := range files {
		if !g.AllowPath(f) {
			g.auditRefusal(f)
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// FilterRepositoryPaths filters repository tree listing paths so denied files
// are omitted from the <repository> block (FAE-SEC-008).
func (g *ReadGuard) FilterRepositoryPaths(paths []string) []string {
	if g == nil || (g.Policy == nil && len(g.DenyPaths) == 0) {
		return paths
	}
	var kept []string
	for _, p := range paths {
		if g.AllowPath(p) {
			kept = append(kept, p)
		}
	}
	return kept
}

// FilterRepositorySection filters any <repository>...</repository> section in
// a prompt string through the effective read policy (FAE-SEC-008).
func (g *ReadGuard) FilterRepositorySection(prompt string) string {
	if g == nil || (g.Policy == nil && len(g.DenyPaths) == 0) {
		return prompt
	}
	start := strings.Index(prompt, "<repository>")
	end := strings.Index(prompt, "</repository>")
	if start < 0 || end <= start {
		return prompt
	}
	body := prompt[start+len("<repository>") : end]
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || g.AllowPath(trimmed) {
			kept = append(kept, line)
		}
	}
	return prompt[:start+len("<repository>")] + strings.Join(kept, "\n") + prompt[end:]
}

// resolveReadTarget resolves a tool target path (relative worktree path,
// "<rootName>:/rel", or absolute path inside Worktree or an allowed ReadRoot),
// verifies physical containment (no symlink escape), and checks read policies.
func (g *ReadGuard) resolveReadTarget(target string) (absPath, auditPath string, rootView *ReadRootView, err error) {
	if g == nil {
		return "", "", nil, errors.New("read guard required")
	}
	t := strings.TrimSpace(target)
	if t == "" {
		return "", "", nil, errors.New("empty path")
	}
	// Check if target references a named read root via "name:rel" or absolute path inside a read root.
	if name, rest, ok := strings.Cut(t, ":"); ok && name != "" && !strings.Contains(name, "/") {
		rv, exists := g.ReadRoots[name]
		if !exists {
			g.auditRefusal(t)
			return "", t, nil, fmt.Errorf("read root %q is not granted to role %q", name, g.Role)
		}
		return g.resolveWithinRoot(rv.Path, rest, &rv)
	}
	if filepath.IsAbs(t) {
		cleanAbs := filepath.Clean(t)
		for name, rv := range g.ReadRoots {
			rootClean := filepath.Clean(rv.Path)
			if cleanAbs == rootClean || strings.HasPrefix(cleanAbs, rootClean+string(filepath.Separator)) {
				rel, _ := filepath.Rel(rootClean, cleanAbs)
				rvCopy := rv
				rvCopy.Name = name
				return g.resolveWithinRoot(rootClean, rel, &rvCopy)
			}
		}
		if g.Worktree != "" {
			wtClean := filepath.Clean(g.Worktree)
			if cleanAbs == wtClean || strings.HasPrefix(cleanAbs, wtClean+string(filepath.Separator)) {
				rel, _ := filepath.Rel(wtClean, cleanAbs)
				return g.resolveWithinRoot(wtClean, rel, nil)
			}
		}
		g.auditRefusal(t)
		return "", t, nil, fmt.Errorf("path %q is outside worktree and granted read roots", t)
	}
	return g.resolveWithinRoot(g.Worktree, t, nil)
}

func (g *ReadGuard) resolveWithinRoot(baseDir, rel string, rv *ReadRootView) (string, string, *ReadRootView, error) {
	if baseDir == "" {
		return "", rel, rv, errors.New("root directory not configured")
	}
	canonBase, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		canonBase = filepath.Clean(baseDir)
	}
	relSlash := filepath.ToSlash(filepath.Clean(rel))
	if relSlash == "." {
		return canonBase, ".", rv, nil
	}
	cleanRel, ok := workflow.CleanRelPath(relSlash)
	if !ok {
		g.auditRefusal(rel)
		return "", rel, rv, fmt.Errorf("unsafe path %q", rel)
	}
	auditKey := cleanRel
	if rv != nil && rv.Name != "" {
		auditKey = cleanRel
	}
	// Check policy before touching disk so even non-existent denied files are audited and refused.
	if rv != nil {
		if !g.AllowReadRootPath(rv.Name, cleanRel) {
			g.auditRefusal(auditKey)
			return "", auditKey, rv, fmt.Errorf("read refused by policy: %s", auditKey)
		}
	} else {
		if !g.AllowPath(cleanRel) {
			g.auditRefusal(auditKey)
			return "", auditKey, nil, fmt.Errorf("read refused by policy: %s", auditKey)
		}
	}
	candidate := filepath.Join(canonBase, filepath.FromSlash(cleanRel))
	// Verify no symlink along the path escapes canonBase (FAE-RT-016(d)).
	if err := checkNoEscapingSymlink(canonBase, candidate); err != nil {
		g.auditRefusal(auditKey)
		return "", auditKey, rv, err
	}
	return candidate, auditKey, rv, nil
}

func checkNoEscapingSymlink(canonBase, target string) error {
	rel, err := filepath.Rel(canonBase, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("path escapes root")
	}
	cur := canonBase
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				break
			}
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return fmt.Errorf("broken symlink %q: %w", cur, err)
			}
			rRel, err := filepath.Rel(canonBase, resolved)
			if err != nil || rRel == ".." || strings.HasPrefix(rRel, ".."+string(filepath.Separator)) {
				return fmt.Errorf("symlink %q points outside root %q", cur, canonBase)
			}
		}
	}
	return nil
}

// Read implements the policy-enforced file/range read tool (FAE-SEC-008, FAE-RT-016).
func (g *ReadGuard) Read(target string, startLine, endLine int) (string, error) {
	abs, _, _, err := g.resolveReadTarget(target)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	content := string(data)
	if startLine <= 0 && endLine <= 0 {
		return content, nil
	}
	lines := strings.Split(content, "\n")
	if startLine < 1 {
		startLine = 1
	}
	if endLine <= 0 || endLine > len(lines) {
		endLine = len(lines)
	}
	if startLine > len(lines) || startLine > endLine {
		return "", nil
	}
	return strings.Join(lines[startLine-1:endLine], "\n"), nil
}

// ListDir is an alias for List implementing the policy-enforced directory listing tool.
func (g *ReadGuard) ListDir(targetDir string) ([]string, error) {
	return g.List(targetDir)
}

func (g *ReadGuard) hasAllowedDescendant(dirAbs, dirRel string) bool {
	found := false
	_ = filepath.WalkDir(dirAbs, func(full string, d fs.DirEntry, err error) error {
		if err != nil || full == dirAbs {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dirAbs, full)
		if err != nil {
			return nil
		}
		candidate := dirRel + "/" + filepath.ToSlash(rel)
		if g.AllowPath(candidate) {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// List implements the policy-enforced directory listing tool (FAE-SEC-008, FAE-RT-016).
func (g *ReadGuard) List(targetDir string) ([]string, error) {
	if strings.TrimSpace(targetDir) == "" {
		targetDir = "."
	}
	abs, relDir, rv, err := g.resolveReadTarget(targetDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	rootBase := g.Worktree
	if rv != nil {
		rootBase = rv.Path
	}
	canonRoot, err := filepath.EvalSymlinks(rootBase)
	if err != nil {
		canonRoot = filepath.Clean(rootBase)
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" {
			continue
		}
		childRel := name
		if relDir != "." && relDir != "" {
			childRel = relDir + "/" + name
		}
		if rv != nil {
			if !g.AllowReadRootPath(rv.Name, childRel) || (entry.IsDir() && !g.AllowReadRootPath(rv.Name, childRel+"/_")) {
				continue
			}
		} else if entry.IsDir() {
			if !g.AllowPath(childRel) && !g.hasAllowedDescendant(filepath.Join(abs, name), childRel) {
				continue
			}
			if !g.AllowPath(childRel+"/_") && !g.hasAllowedDescendant(filepath.Join(abs, name), childRel) {
				continue
			}
		} else if !g.AllowPath(childRel) {
			continue
		}
		if err := checkNoEscapingSymlink(canonRoot, filepath.Join(abs, name)); err != nil {
			continue
		}
		out = append(out, childRel)
	}
	slices.Sort(out)
	return out, nil
}

// SearchGlob implements the policy-enforced glob search tool (FAE-SEC-008, FAE-RT-016).
func (g *ReadGuard) SearchGlob(pattern string) ([]string, error) {
	if g == nil {
		return nil, errors.New("read guard required")
	}
	p := strings.TrimSpace(pattern)
	if p == "" {
		return nil, errors.New("empty glob pattern")
	}
	walkRoot := g.Worktree
	var rv *ReadRootView
	if name, rest, ok := strings.Cut(p, ":"); ok && name != "" && !strings.Contains(name, "/") {
		r, exists := g.ReadRoots[name]
		if !exists {
			g.auditRefusal(p)
			return nil, fmt.Errorf("read root %q is not granted to role %q", name, g.Role)
		}
		rCopy := r
		rCopy.Name = name
		rv = &rCopy
		walkRoot = r.Path
		p = strings.TrimPrefix(rest, "/")
	}
	canonRoot, err := filepath.EvalSymlinks(walkRoot)
	if err != nil {
		canonRoot = filepath.Clean(walkRoot)
	}
	var matched []string
	var deniedHit string
	walkErr := filepath.WalkDir(canonRoot, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if full == canonRoot || d.IsDir() {
			return nil
		}
		if checkNoEscapingSymlink(canonRoot, full) != nil {
			return nil
		}
		rel, err := filepath.Rel(canonRoot, full)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if workflow.MatchGlob(p, relSlash) || workflow.MatchGlob(p, filepath.Base(relSlash)) {
			allowed := false
			if rv != nil {
				allowed = g.AllowReadRootPath(rv.Name, relSlash)
			} else {
				allowed = g.AllowPath(relSlash)
			}
			if !allowed {
				if deniedHit == "" {
					deniedHit = relSlash
				}
				return nil
			}
			matched = append(matched, relSlash)
		}
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	if len(matched) == 0 && (deniedHit != "" || (rv == nil && !g.AllowPath(p)) || (rv != nil && !g.AllowReadRootPath(rv.Name, p))) {
		if deniedHit == "" {
			deniedHit = p
		}
		g.auditRefusal(deniedHit)
		return nil, fmt.Errorf("search_glob refused by read policy: %s", deniedHit)
	}
	if deniedHit != "" {
		g.auditRefusal(deniedHit)
	}
	slices.Sort(matched)
	return matched, nil
}

// SearchRegex implements the policy-enforced regex search tool (FAE-SEC-008, FAE-RT-016).
func (g *ReadGuard) SearchRegex(expr, targetPath string) ([]SearchMatch, error) {
	if g == nil {
		return nil, errors.New("read guard required")
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(targetPath) == "" {
		targetPath = "."
	}
	abs, relBase, rv, err := g.resolveReadTarget(targetPath)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	var matches []SearchMatch
	searchFile := func(full, relSlash string) {
		data, err := os.ReadFile(full)
		if err != nil {
			return
		}
		for i, line := range strings.Split(string(data), "\n") {
			if re.MatchString(line) {
				matches = append(matches, SearchMatch{Path: relSlash, Line: i + 1, Text: line})
			}
		}
	}
	if !st.IsDir() {
		searchFile(abs, relBase)
		return matches, nil
	}
	rootBase := g.Worktree
	if rv != nil {
		rootBase = rv.Path
	}
	_ = filepath.WalkDir(abs, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(rootBase, full)
		if err != nil {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if rv != nil {
			if !g.AllowReadRootPath(rv.Name, relSlash) {
				return nil
			}
		} else if !g.AllowPath(relSlash) {
			return nil
		}
		searchFile(full, relSlash)
		return nil
	})
	return matches, nil
}

// CheckWrite refuses any write to an external read root (FAE-RT-016) or outside
// the worktree or to a path matching DenyPaths (FAE-SEC-009).
func (g *ReadGuard) CheckWrite(target string) error {
	if g == nil {
		return errors.New("read guard required")
	}
	t := strings.TrimSpace(target)
	if name, _, ok := strings.Cut(t, ":"); ok && name != "" && !strings.Contains(name, "/") {
		if _, isRoot := g.ReadRoots[name]; isRoot {
			return fmt.Errorf("read root %q is read-only; writes are forbidden", name)
		}
	}
	if filepath.IsAbs(t) {
		cleanAbs := filepath.Clean(t)
		for name, rv := range g.ReadRoots {
			rootClean := filepath.Clean(rv.Path)
			if cleanAbs == rootClean || strings.HasPrefix(cleanAbs, rootClean+string(filepath.Separator)) {
				return fmt.Errorf("read root %q (%s) is read-only; writes are forbidden", name, rootClean)
			}
		}
		wtClean := filepath.Clean(g.Worktree)
		if cleanAbs != wtClean && !strings.HasPrefix(cleanAbs, wtClean+string(filepath.Separator)) {
			return fmt.Errorf("write target %q is outside worktree", t)
		}
		rel, err := filepath.Rel(wtClean, cleanAbs)
		if err != nil {
			return err
		}
		t = filepath.ToSlash(rel)
	}
	cleanRel, ok := workflow.CleanRelPath(t)
	if !ok {
		return fmt.Errorf("unsafe write path %q", target)
	}
	if idx := workflow.MatchDenyPathIndex(g.DenyPaths, cleanRel); idx >= 0 {
		return fmt.Errorf("write refused by deny_paths[%d]", idx)
	}
	return nil
}

type maskedFile struct {
	rel  string
	orig string
	temp string
	mode fs.FileMode
}

// MaskWorktree is retired: shared filesystem masking is unsafe for parallel
// sessions. Call ConfigureSessionView on the child command instead.
func (g *ReadGuard) MaskWorktree() (func() error, error) {
	noop := func() error { return nil }
	if g != nil && (g.Policy != nil || len(g.DenyPaths) > 0 || len(g.ReadRoots) > 0) {
		return noop, fmt.Errorf("shared filesystem masking is unsupported; configure a private session filesystem view")
	}
	return noop, nil
}

func (g *ReadGuard) checkShellCommand(command string) error {
	if g == nil {
		return nil
	}
	tokens := strings.FieldsFunc(command, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '|' || r == '&' || r == '>' || r == '<' || r == '\'' || r == '"' || r == '(' || r == ')'
	})
	hasWriteOp := strings.Contains(command, ">") || slices.ContainsFunc(tokens, func(tok string) bool {
		switch tok {
		case "rm", "mv", "cp", "touch", "mkdir", "rmdir", "tee", "truncate", "chmod", "chown", "dd":
			return true
		}
		return false
	})
	for _, token := range tokens {
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		if name, rest, ok := strings.Cut(token, ":"); ok && name != "" && !strings.Contains(name, "/") {
			if _, exists := g.ReadRoots[name]; exists {
				if hasWriteOp {
					return fmt.Errorf("read root %q is read-only; shell writes are forbidden", name)
				}
				cleanRel, ok := workflow.CleanRelPath(strings.TrimPrefix(rest, "/"))
				if !ok || !g.AllowReadRootPath(name, cleanRel) {
					g.auditRefusal(cleanRel)
					return fmt.Errorf("read refused by policy: %s", cleanRel)
				}
				continue
			}
		}
		for name, rv := range g.ReadRoots {
			if rv.Path == "" {
				continue
			}
			rootClean := filepath.Clean(rv.Path)
			if filepath.IsAbs(token) && (filepath.Clean(token) == rootClean || strings.HasPrefix(filepath.Clean(token), rootClean+string(filepath.Separator))) {
				if hasWriteOp {
					return fmt.Errorf("read root %q (%s) is read-only; shell writes are forbidden", name, rootClean)
				}
				rel, _ := filepath.Rel(rootClean, filepath.Clean(token))
				relSlash := filepath.ToSlash(rel)
				if relSlash != "." && !g.AllowReadRootPath(name, relSlash) {
					g.auditRefusal(relSlash)
					return fmt.Errorf("read refused by policy: %s", relSlash)
				}
			}
		}
		cand := token
		if filepath.IsAbs(cand) && g.Worktree != "" {
			wtClean := filepath.Clean(g.Worktree)
			if filepath.Clean(cand) == wtClean || strings.HasPrefix(filepath.Clean(cand), wtClean+string(filepath.Separator)) {
				rel, err := filepath.Rel(wtClean, filepath.Clean(cand))
				if err == nil {
					cand = filepath.ToSlash(rel)
				}
			}
		}
		if cleanRel, ok := workflow.CleanRelPath(cand); ok && cleanRel != "." {
			if st, err := os.Lstat(filepath.Join(g.Worktree, filepath.FromSlash(cleanRel))); err == nil && !st.IsDir() {
				if !g.AllowPath(cleanRel) {
					g.auditRefusal(cleanRel)
					return fmt.Errorf("shell access to denied path %q refused by read policy", cleanRel)
				}
			} else if (strings.Contains(cleanRel, "/") || workflow.MatchDenyPathIndex(g.DenyPaths, cleanRel) >= 0 || (g.Policy != nil && len(g.Policy.Deny) > 0 && workflow.MatchesAnyGlob(cleanRel, g.Policy.Deny))) && !g.AllowPath(cleanRel) {
				g.auditRefusal(cleanRel)
				return fmt.Errorf("shell access to denied path %q refused by read policy", cleanRel)
			}
		}
	}
	return nil
}

// RunShell executes a command inside the worktree with denied files masked from
// the filesystem view and audits any argument that attempts to read a denied
// path or write to a read root (FAE-SEC-008, FAE-RT-016).
func (g *ReadGuard) RunShell(ctx context.Context, argv []string, env []string) (string, error) {
	if g == nil {
		return "", errors.New("read guard required")
	}
	if len(argv) == 0 {
		return "", errors.New("empty command")
	}
	if err := g.checkShellCommand(strings.Join(argv[1:], " ")); err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = g.Worktree
	if len(env) > 0 {
		cmd.Env = env
	}
	isolated := *g
	isolated.HasShell = true
	view, err := isolated.ConfigureSessionView(cmd, g.LogDirectory, nil)
	if view != nil {
		defer view.Close()
	}
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	runErr := cmd.Run()
	if err := view.Collect(); err != nil {
		return out.String(), err
	}
	return out.String(), runErr
}

func (g *ReadGuard) lockReadRoots() func() { return func() {} } // read-only mounts replace shared chmod.
