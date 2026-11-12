package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/engine"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// FormatTaskNotificationsSection renders task notifications inside a
// <task_notifications> container at an agentic tool boundary (FAE-OPS-033).
func FormatTaskNotificationsSection(notifications []engine.TaskNotification) string {
	if len(notifications) == 0 {
		return ""
	}
	return "<task_notifications>\n" + FormatNotificationsQuoted(notifications) + "\n</task_notifications>"
}

// AgenticToolCall represents one structured tool invocation during an agentic
// turn (FAE-RT-015, FAE-RT-017).
type AgenticToolCall struct {
	Tool         string          `json:"tool"`
	Path         string          `json:"path,omitempty"`
	StartLine    int             `json:"start_line,omitempty"`
	EndLine      int             `json:"end_line,omitempty"`
	Pattern      string          `json:"pattern,omitempty"`
	Target       string          `json:"target,omitempty"`
	Replacement  string          `json:"replacement,omitempty"`
	Replacements []Replacement   `json:"replacements,omitempty"`
	Content      string          `json:"content,omitempty"`
	Command      string          `json:"command,omitempty"`
	Output       json.RawMessage `json:"output,omitempty"`
}

// SearchMatch is a single line match returned by search_regex.
type SearchMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// AgenticToolResult captures the structured output of one agentic tool call.
type AgenticToolResult struct {
	Tool       string          `json:"tool"`
	Path       string          `json:"path,omitempty"`
	Content    string          `json:"content,omitempty"`
	StartLine  int             `json:"start_line,omitempty"`
	EndLine    int             `json:"end_line,omitempty"`
	TotalLines int             `json:"total_lines,omitempty"`
	Bytes      int64           `json:"bytes,omitempty"`
	Entries    []string        `json:"entries,omitempty"`
	Matches    []SearchMatch   `json:"matches,omitempty"`
	ExitCode   int             `json:"exit_code,omitempty"`
	Stdout     string          `json:"stdout,omitempty"`
	Stderr     string          `json:"stderr,omitempty"`
	Handoff    json.RawMessage `json:"handoff,omitempty"`
}

// AgenticToolbox enforces a role's declared tool profile and read/write scope
// boundaries during an agentic turn (FAE-RT-015, FAE-RT-017, FAE-HOST-011).
type AgenticToolbox struct {
	OutputOwnershipRegistry       string
	ExcludedOutputDirs            []string
	ToolAuditPath                 string
	toolEventMu                   sync.Mutex
	toolEventWindow               time.Time
	toolEventCount, toolEventRate int
	OutputDir, StagingDir         string
	StagingWrite, ArtifactsOnly   bool
	Scratch                       []string
	WriteScopes                   [][]string
	DeniedTestCommands            [][]string
	Worktree                      string
	Profile                       workflow.Profile
	ChangeProducer                bool
	Stage                         string
	WriteScope                    []string
	ReadOnlyFiles                 []string
	TestFile                      string
	OutputSchema                  json.RawMessage
	Env                           []string
	ClaudeSettings                string
	Guard                         *ReadGuard
	InstanceRoot                  string
	TaskID                        string
	DeliveredNotifications        []engine.TaskNotification

	mu            sync.Mutex
	handoffOutput json.RawMessage
}

// AllowedTools returns the canonical Fae tool capabilities enabled for this
// toolbox.
func (t *AgenticToolbox) AllowedTools() []string {
	changeProducer := t.ChangeProducer || t.Stage == "test-first" || t.Stage == "implement" || len(t.WriteScopes) > 0 || t.ArtifactsOnly || t.StagingWrite || len(t.Scratch) > 0
	return agentruntime.AllowedAgenticTools(t.Profile, changeProducer)
}

// HandoffOutput returns the validated structured output submitted through the
// handoff tool, or nil if handoff has not been called.
func (t *AgenticToolbox) HandoffOutput() json.RawMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	return slices.Clone(t.handoffOutput)
}

func normalizeToolCapability(name string) string {
	switch name {
	case "read", "Read", "read_range":
		return "read"
	case "list", "LS", "list_dir":
		return "list"
	case "search_glob", "Glob":
		return "search_glob"
	case "search_regex", "Grep":
		return "search_regex"
	case "edit", "Edit", "apply_patch":
		return "edit"
	case "write", "Write", "write_file":
		return "write"
	case "shell", "Bash", "run_shell":
		return "shell"
	case "handoff", "StructuredOutput":
		return "handoff"
	default:
		return name
	}
}

// Call dispatches a single tool invocation after verifying the role's declared
// tool profile and scope rules.
func (t *AgenticToolbox) Call(ctx context.Context, call AgenticToolCall) (result AgenticToolResult, callErr error) {
	defer func() { t.recordToolEvent(call, callErr == nil) }()
	capName := normalizeToolCapability(call.Tool)
	allowed := t.AllowedTools()
	if !slices.Contains(allowed, capName) {
		return AgenticToolResult{}, fmt.Errorf("tool %q is not allowed by role profile (allowed: %s)", call.Tool, strings.Join(allowed, ", "))
	}
	var (
		res AgenticToolResult
		err error
	)
	switch capName {
	case "read":
		res, err = t.readRange(call.Path, call.StartLine, call.EndLine)
	case "list":
		res, err = t.listDir(call.Path)
	case "search_glob":
		res, err = t.searchGlob(call.Pattern)
	case "search_regex":
		res, err = t.searchRegex(call.Path, call.Pattern)
	case "edit":
		res, err = t.applyEdit(call)
	case "write":
		res, err = t.applyWrite(call.Path, call.Content)
	case "shell":
		res, err = t.runShell(ctx, call.Command)
	case "handoff":
		res, err = t.completeHandoff(call.Output)
	default:
		return AgenticToolResult{}, fmt.Errorf("unsupported tool %q", call.Tool)
	}
	if err != nil {
		return res, err
	}
	if notifs := t.consumeLiveNotifications(); len(notifs) > 0 {
		section := "\n" + FormatTaskNotificationsSection(notifs)
		res.Content += section
		if capName == "shell" {
			res.Stdout += section
		}
	}
	return res, nil
}

func (t *AgenticToolbox) consumeLiveNotifications() []engine.TaskNotification {
	if t.TaskID == "" || t.InstanceRoot == "" {
		return nil
	}
	cleanRoot := filepath.Clean(t.InstanceRoot)
	candidates := []string{cleanRoot}
	base := filepath.Base(cleanRoot)
	if base == "provider-sessions" || base == "sessions" || strings.HasPrefix(base, "session-") {
		parent := filepath.Dir(cleanRoot)
		candidates = append(candidates, parent)
		if filepath.Base(parent) == "provider-sessions" || filepath.Base(parent) == "sessions" {
			candidates = append(candidates, filepath.Dir(parent))
		}
	}
	var all []engine.TaskNotification
	now := time.Now().UTC()
	for _, root := range candidates {
		notifs, _, err := ConsumeLiveNotifications(LiveNotificationPath(root, t.TaskID), now)
		if err == nil && len(notifs) > 0 {
			all = append(all, notifs...)
		}
	}
	if len(all) > 0 {
		t.mu.Lock()
		t.DeliveredNotifications = append(t.DeliveredNotifications, all...)
		t.mu.Unlock()
	}
	return all
}

// maxRangeReadBytes bounds the bytes returned by a single range read call while
// allowing arbitrarily large (> 1 MiB) files to be streamed by line range.
const maxRangeReadBytes = 1 << 20

func (t *AgenticToolbox) readRange(rel string, startLine, endLine int) (AgenticToolResult, error) {
	var (
		abs string
		err error
		st  os.FileInfo
	)
	if path, matched, pathErr := t.taskDirectoryPath(rel, false); matched || pathErr != nil {
		abs, err = path, pathErr
		if err == nil {
			st, err = os.Stat(abs)
		}
	} else if t.Guard != nil {
		abs, _, _, err = t.Guard.resolveReadTarget(rel)
		if err != nil {
			return AgenticToolResult{}, fmt.Errorf("read refused: %w", err)
		}
		st, err = os.Stat(abs)
	} else {
		if !workflow.DeclaredPathSafe(rel) {
			return AgenticToolResult{}, fmt.Errorf("read refused: unsafe path %q", rel)
		}
		abs, err = safePath(t.Worktree, rel)
		if err != nil {
			return AgenticToolResult{}, fmt.Errorf("read refused: %w", err)
		}
		st, err = os.Lstat(abs)
	}
	if err != nil {
		return AgenticToolResult{}, err
	}
	if !st.Mode().IsRegular() {
		return AgenticToolResult{}, fmt.Errorf("read refused: %q is not a regular file", rel)
	}
	if startLine <= 0 {
		startLine = 1
	}
	if endLine > 0 && endLine < startLine {
		return AgenticToolResult{}, fmt.Errorf("read refused: end_line %d < start_line %d", endLine, startLine)
	}
	f, err := os.Open(abs)
	if err != nil {
		return AgenticToolResult{}, err
	}
	defer f.Close()

	reader := bufio.NewReaderSize(f, 64*1024)
	var buf strings.Builder
	lineNo := 0
	lastIncluded := 0
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			if lineNo >= startLine && (endLine == 0 || lineNo <= endLine) {
				if buf.Len()+len(line) > maxRangeReadBytes {
					return AgenticToolResult{}, fmt.Errorf("read range on %q exceeds %d-byte per-call response bound; narrow start_line/end_line", rel, maxRangeReadBytes)
				}
				buf.WriteString(line)
				lastIncluded = lineNo
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return AgenticToolResult{}, readErr
		}
	}
	return AgenticToolResult{
		Tool:       "read",
		Path:       rel,
		Content:    buf.String(),
		StartLine:  startLine,
		EndLine:    lastIncluded,
		TotalLines: lineNo,
		Bytes:      st.Size(),
	}, nil
}

func (t *AgenticToolbox) listDir(rel string) (AgenticToolResult, error) {
	if target, matched, pathErr := t.taskDirectoryPath(rel, false); matched || pathErr != nil {
		if pathErr != nil {
			return AgenticToolResult{}, pathErr
		}
		guard := ReadGuard{}
		if t.Guard != nil {
			guard = *t.Guard
		}
		guard.ReadRoots = nil
		guard.Worktree = t.OutputDir
		if target == t.StagingDir || strings.HasPrefix(target, t.StagingDir+string(filepath.Separator)) && t.StagingDir != "" {
			guard.Worktree = t.StagingDir
		}
		entries, err := guard.ListDir(target)
		if err != nil {
			return AgenticToolResult{}, err
		}
		return AgenticToolResult{Tool: "list", Path: rel, Entries: entries}, nil
	}

	if t.Guard != nil {
		entries, err := t.Guard.ListDir(rel)
		if err != nil {
			return AgenticToolResult{}, err
		}
		return AgenticToolResult{Tool: "list", Path: rel, Entries: entries}, nil
	}
	target := t.Worktree
	if rel != "" && rel != "." {
		if !workflow.DeclaredPathSafe(rel) {
			return AgenticToolResult{}, fmt.Errorf("list refused: unsafe path %q", rel)
		}
		p, err := safePath(t.Worktree, rel)
		if err != nil {
			return AgenticToolResult{}, err
		}
		target = p
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		return AgenticToolResult{}, err
	}
	var names []string
	for _, e := range entries {
		if e.Name() == ".git" {
			continue
		}
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return AgenticToolResult{Tool: "list", Path: rel, Entries: names}, nil
}

func (t *AgenticToolbox) searchGlob(pattern string) (AgenticToolResult, error) {
	if pattern == "" {
		return AgenticToolResult{}, errors.New("search_glob requires pattern")
	}
	if t.Guard != nil {
		matches, err := t.Guard.SearchGlob(pattern)
		if err != nil {
			return AgenticToolResult{}, err
		}
		return AgenticToolResult{Tool: "search_glob", Entries: matches}, nil
	}
	var matches []string
	err := filepath.WalkDir(t.Worktree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == t.Worktree {
			return nil
		}
		rel, err := filepath.Rel(t.Worktree, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Name() == ".git" || strings.HasPrefix(rel, ".git/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ok := workflow.MatchGlob(pattern, rel)
		if !ok {
			ok = workflow.MatchGlob(pattern, filepath.Base(rel))
		}
		if ok {
			matches = append(matches, rel)
		}
		return nil
	})
	if err != nil {
		return AgenticToolResult{}, err
	}
	sort.Strings(matches)
	return AgenticToolResult{Tool: "search_glob", Entries: matches}, nil
}

func (t *AgenticToolbox) searchRegex(rel, pattern string) (AgenticToolResult, error) {
	if pattern == "" {
		return AgenticToolResult{}, errors.New("search_regex requires pattern")
	}
	if t.Guard != nil {
		matches, err := t.Guard.SearchRegex(pattern, rel)
		if err != nil {
			return AgenticToolResult{}, err
		}
		if len(matches) > 200 {
			matches = matches[:200]
		}
		return AgenticToolResult{Tool: "search_regex", Matches: matches}, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return AgenticToolResult{}, fmt.Errorf("invalid regex %q: %w", pattern, err)
	}
	var files []string
	if rel != "" {
		if !workflow.DeclaredPathSafe(rel) {
			return AgenticToolResult{}, fmt.Errorf("search_regex refused: unsafe path %q", rel)
		}
		if _, err := safePath(t.Worktree, rel); err != nil {
			return AgenticToolResult{}, err
		}
		files = []string{rel}
	} else {
		globRes, err := t.searchGlob("*")
		if err != nil {
			return AgenticToolResult{}, err
		}
		files = globRes.Entries
	}
	var matches []SearchMatch
	for _, f := range files {
		abs, err := safePath(t.Worktree, f)
		if err != nil {
			continue
		}
		fh, err := os.Open(abs)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(fh)
		scanner.Buffer(make([]byte, 64*1024), 1<<20)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			text := scanner.Text()
			if re.MatchString(text) {
				matches = append(matches, SearchMatch{Path: f, Line: lineNo, Text: text})
				if len(matches) >= 200 {
					break
				}
			}
		}
		fh.Close()
		if len(matches) >= 200 {
			break
		}
	}
	return AgenticToolResult{Tool: "search_regex", Matches: matches}, nil
}

func (t *AgenticToolbox) checkWritablePath(rel string) (string, error) {
	if path, matched, err := t.taskDirectoryPath(rel, true); matched || err != nil {
		return path, err
	}
	if t.ArtifactsOnly {
		return "", fmt.Errorf("write outside artifact output directory refused: %s", rel)
	}

	if t.Guard != nil {
		if err := t.Guard.CheckWrite(rel); err != nil {
			return "", err
		}
	}
	if !workflow.DeclaredPathSafe(rel) {
		return "", fmt.Errorf("edit refused: unsafe path %q", rel)
	}
	if workflow.MatchesAnyGlob(rel, t.ReadOnlyFiles) {
		return "", fmt.Errorf("edit refused: %q is in read_only_inputs and cannot be modified", rel)
	}
	if why, protected := workflow.ProtectedPath(rel); protected {
		return "", fmt.Errorf("edit refused: protected path %q (%s)", rel, why)
	}
	scoped := workflow.MatchesAnyGlob(rel, t.WriteScope)
	if len(t.WriteScopes) > 0 {
		scoped = workflow.AllowsWriteScopes(rel, t.WriteScopes)
	}
	if !workflow.MatchesAnyGlob(rel, t.Scratch) && !scoped {
		return "", fmt.Errorf("edit refused: %q is outside write_scope", rel)
	}
	switch {
	case t.Stage == "test-first" && t.TestFile != "" && rel != t.TestFile:
		return "", errors.New("RED may edit only the task's test file")
	case t.Stage == "test-first" && t.TestFile == "" && !workflow.TestSourceFile(rel, ""):
		return "", errors.New("RED may edit only a Go test file")
	case t.Stage == "implement" && t.TestFile != "" && rel == t.TestFile:
		return "", errors.New("implementation cannot edit frozen test")
	}
	return safePath(t.Worktree, rel)
}

func (t *AgenticToolbox) applyEdit(call AgenticToolCall) (AgenticToolResult, error) {
	abs, err := t.checkWritablePath(call.Path)
	if err != nil {
		return AgenticToolResult{}, err
	}
	if _, matched, readErr := t.taskDirectoryPath(abs, false); matched || readErr != nil {
		if readErr != nil {
			return AgenticToolResult{}, readErr
		}
	} else if t.Guard != nil {
		if _, _, _, readErr = t.Guard.resolveReadTarget(call.Path); readErr != nil {
			return AgenticToolResult{}, readErr
		}
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return AgenticToolResult{}, err
	}
	current := string(data)
	repls := slices.Clone(call.Replacements)
	if call.Target != "" {
		repls = append(repls, Replacement{Old: call.Target, New: call.Replacement})
	}
	if len(repls) > 0 {
		for i, r := range repls {
			if r.Old == "" {
				return AgenticToolResult{}, fmt.Errorf("edit %q replacement %d has empty target", call.Path, i)
			}
			first := strings.Index(current, r.Old)
			if first < 0 {
				return AgenticToolResult{}, fmt.Errorf("edit %q replacement %d target not found", call.Path, i)
			}
			if strings.Index(current[first+len(r.Old):], r.Old) >= 0 {
				return AgenticToolResult{}, fmt.Errorf("edit %q replacement %d target is ambiguous", call.Path, i)
			}
			current = current[:first] + r.New + current[first+len(r.Old):]
		}
	} else if call.StartLine > 0 && call.EndLine >= call.StartLine {
		lines := strings.SplitAfter(current, "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		if call.StartLine > len(lines) || call.EndLine > len(lines) {
			return AgenticToolResult{}, fmt.Errorf("edit %q line range %d..%d out of bounds (total %d)", call.Path, call.StartLine, call.EndLine, len(lines))
		}
		var b strings.Builder
		for i := 0; i < call.StartLine-1; i++ {
			b.WriteString(lines[i])
		}
		b.WriteString(call.Replacement)
		for i := call.EndLine; i < len(lines); i++ {
			b.WriteString(lines[i])
		}
		current = b.String()
	} else {
		return AgenticToolResult{}, fmt.Errorf("edit %q requires target/replacement, replacements, or start_line/end_line", call.Path)
	}
	if err := writeAtomicWorktreeFile(abs, []byte(current)); err != nil {
		return AgenticToolResult{}, err
	}
	return AgenticToolResult{Tool: "edit", Path: call.Path, Bytes: int64(len(current))}, nil
}

func (t *AgenticToolbox) applyWrite(rel, content string) (AgenticToolResult, error) {
	abs, err := t.checkWritablePath(rel)
	if err != nil {
		return AgenticToolResult{}, err
	}
	if err := writeAtomicWorktreeFile(abs, []byte(content)); err != nil {
		return AgenticToolResult{}, err
	}
	return AgenticToolResult{Tool: "write", Path: rel, Bytes: int64(len(content))}, nil
}

func writeAtomicWorktreeFile(abs string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".fae-edit-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		return err
	}
	return os.Rename(tmpName, abs)
}

func (t *AgenticToolbox) runShell(ctx context.Context, command string) (AgenticToolResult, error) {
	if strings.TrimSpace(command) == "" {
		return AgenticToolResult{}, errors.New("shell command required")
	}
	literalArgv, policyErr := workflow.CheckShellCommand(t.Profile, command, t.DeniedTestCommands)
	if policyErr != nil {
		return AgenticToolResult{}, fmt.Errorf("shell command %q refused: %w", command, policyErr)
	}
	if t.Guard != nil {
		if err := t.Guard.checkShellCommand(command); err != nil {
			return AgenticToolResult{}, err
		}
	}
	if t.ClaudeSettings != "" || len(t.Env) > 0 || t.Guard == nil {
		if err := agentruntime.VerifyAgenticShellLaunch(t.Env, t.ClaudeSettings); err != nil {
			return AgenticToolResult{}, err
		}
	}
	shimDir := ""
	for _, kv := range t.Env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			if parts := filepath.SplitList(v); len(parts) > 0 {
				shimDir = parts[0]
			}
		}
	}
	if shimDir != "" {
		if reason, bad := commandshim.BypassesShim(command, shimDir); bad {
			return AgenticToolResult{}, fmt.Errorf("PreToolUse hook denied shell command: %s", reason)
		}
	}
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	if t.Profile.ShellAllow != nil && len(literalArgv) > 0 {
		cmd = exec.CommandContext(ctx, literalArgv[0], literalArgv[1:]...)
	}
	cmd.Dir = t.scopedShellCWD()
	cmd.Env = t.Env
	var view *SessionView
	if t.Guard != nil {
		var e error
		isolated := *t.Guard
		isolated.HasShell = true
		isolated.WritableWorktree = t.ChangeProducer || len(t.Scratch) > 0
		isolated.CanWrite = func(rel string) bool { _, err := t.checkWritablePath(rel); return err == nil }

		view, e = isolated.ConfigureSessionView(cmd, t.Guard.LogDirectory, nil)
		if view != nil {
			defer view.Close()
		}
		if e != nil {
			return AgenticToolResult{}, e
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if e := view.Collect(); e != nil {
		return AgenticToolResult{}, e
	}
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			return AgenticToolResult{}, err
		}
	}
	return AgenticToolResult{
		Tool:     "shell",
		ExitCode: exitCode,
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}, nil
}

func (t *AgenticToolbox) completeHandoff(output json.RawMessage) (AgenticToolResult, error) {
	if len(output) == 0 {
		return AgenticToolResult{}, errors.New("handoff output required")
	}
	if len(t.OutputSchema) > 0 {
		if err := workflow.ValidateArtifact(t.OutputSchema, output); err != nil {
			return AgenticToolResult{}, fmt.Errorf("handoff output failed schema validation: %w", err)
		}
	}
	t.mu.Lock()
	t.handoffOutput = slices.Clone(output)
	t.mu.Unlock()
	return AgenticToolResult{Tool: "handoff", Handoff: output}, nil
}

// SaveTurnCheckpointFile atomically writes an agentic turn checkpoint to
// checkpoint.json in logDir (FAE-RT-019).
func SaveTurnCheckpointFile(logDir string, cp agentruntime.TurnCheckpoint) error {
	if logDir == "" {
		return nil
	}
	if cp.UpdatedAt == "" {
		cp.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	return writeAtomicWorktreeFile(filepath.Join(logDir, "checkpoint.json"), raw)
}

type fallbackTreeSnapshot struct {
	Files map[string][]byte
	Modes map[string]os.FileMode
}

var (
	fallbackTreesMu sync.Mutex
	fallbackTrees   = map[string]fallbackTreeSnapshot{}
)

func systemGitBin() string {
	if git, err := repositoryGit(os.Getenv("PATH"), ""); err == nil && git != "" {
		return git
	}
	return "git"
}

func isGitWorktree(worktree string) bool {
	cmd := exec.Command(systemGitBin(), "-C", worktree, "rev-parse", "--show-toplevel")
	cmd.Env = cleanGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	physical, err := filepath.EvalSymlinks(worktree)
	if err != nil {
		return false
	}
	return filepath.Clean(strings.TrimSpace(string(out))) == physical
}

func cleanGitEnv(extra ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			var parts []string
			for _, dir := range filepath.SplitList(v) {
				if filepath.Base(dir) == "command-shims" {
					continue
				}
				parts = append(parts, dir)
			}
			env = append(env, "PATH="+strings.Join(parts, string(os.PathListSeparator)))
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}

// SnapshotWorktreeTree records the git tree hash (or deterministic fallback
// tree hash) of worktree without mutating the worktree's live git index
// (FAE-RT-015, FAE-RT-019).
func SnapshotWorktreeTree(ctx context.Context, worktree string) (string, error) {
	if isGitWorktree(worktree) {
		return gitSnapshotTree(ctx, worktree, "", nil)
	}
	return fallbackSnapshotTree(worktree, "", nil)
}

// SnapshotInScopeTree records a tree hash that combines baseTree with the
// current worktree state of inScope files only, leaving out-of-scope paths at
// baseTree (FAE-RT-019).
func SnapshotInScopeTree(ctx context.Context, worktree, baseTree string, inScope []string) (string, error) {
	if baseTree == "" {
		var err error
		baseTree, err = SnapshotWorktreeTree(ctx, worktree)
		if err != nil {
			return "", err
		}
	}
	if isGitWorktree(worktree) {
		return gitSnapshotTree(ctx, worktree, baseTree, inScope)
	}
	return fallbackSnapshotTree(worktree, baseTree, inScope)
}

func gitSnapshotTree(ctx context.Context, worktree, baseTree string, onlyPaths []string) (string, error) {
	tmpDir, err := os.MkdirTemp("", "fae-git-idx-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	idxPath := filepath.Join(tmpDir, "index")
	env := cleanGitEnv("GIT_INDEX_FILE=" + idxPath)

	runGit := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, systemGitBin(), append([]string{"-C", worktree}, args...)...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}

	if baseTree != "" {
		if _, err := runGit("read-tree", baseTree); err != nil {
			return "", err
		}
		if onlyPaths != nil {
			for _, rel := range onlyPaths {
				if !workflow.DeclaredPathSafe(rel) {
					continue
				}
				abs := filepath.Join(worktree, rel)
				if _, err := os.Lstat(abs); os.IsNotExist(err) {
					_, _ = runGit("rm", "--cached", "--ignore-unmatch", "--", rel)
				} else {
					if _, err := runGit("add", "-f", "--", rel); err != nil {
						return "", err
					}
				}
			}
			return runGit("write-tree")
		}
	} else {
		_, _ = runGit("read-tree", "HEAD")
	}
	if _, err := runGit("add", "-A", "-f", "--", "."); err != nil {
		return "", err
	}
	return runGit("write-tree")
}

func scanWorktreeFiles(worktree string) (map[string][]byte, map[string]os.FileMode, error) {
	files := map[string][]byte{}
	modes := map[string]os.FileMode{}
	err := filepath.WalkDir(worktree, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == worktree {
			return nil
		}
		rel, err := filepath.Rel(worktree, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.Name() == ".git" || strings.HasPrefix(rel, ".git/") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !st.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = data
		modes[rel] = st.Mode().Perm()
		return nil
	})
	return files, modes, err
}

func fallbackSnapshotTree(worktree, baseTree string, onlyPaths []string) (string, error) {
	curFiles, curModes, err := scanWorktreeFiles(worktree)
	if err != nil {
		return "", err
	}
	snap := fallbackTreeSnapshot{
		Files: map[string][]byte{},
		Modes: map[string]os.FileMode{},
	}
	fallbackTreesMu.Lock()
	baseSnap, hasBase := fallbackTrees[baseTree]
	fallbackTreesMu.Unlock()

	if baseTree != "" && hasBase && onlyPaths != nil {
		for k, v := range baseSnap.Files {
			snap.Files[k] = slices.Clone(v)
			snap.Modes[k] = baseSnap.Modes[k]
		}
		for _, p := range onlyPaths {
			if data, ok := curFiles[p]; ok {
				snap.Files[p] = slices.Clone(data)
				snap.Modes[p] = curModes[p]
			} else {
				delete(snap.Files, p)
				delete(snap.Modes, p)
			}
		}
	} else {
		for k, v := range curFiles {
			snap.Files[k] = slices.Clone(v)
			snap.Modes[k] = curModes[k]
		}
	}

	var keys []string
	for k := range snap.Files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00%d\x00", k, len(snap.Files[k]))
		h.Write(snap.Files[k])
	}
	hash := hex.EncodeToString(h.Sum(nil))[:40]
	fallbackTreesMu.Lock()
	fallbackTrees[hash] = snap
	fallbackTreesMu.Unlock()
	return hash, nil
}

// VerifyAndRevertWorktreeScope diffs worktree against startTree at turn
// collection (FAE-RT-015, FAE-RT-017). Any modified, created, or deleted file
// outside editable (or in readOnly, or on a non-ChangeProducer stage) is
// reverted back to startTree and returned in offending so the caller refuses
// the turn as output-refused naming the offending paths.
func VerifyAndRevertWorktreeScope(ctx context.Context, worktree, startTree string, editable, readOnly []string, changeProducer bool) ([]string, error) {
	if startTree == "" {
		return nil, nil
	}
	allowed := func(rel string) bool {
		if !changeProducer {
			return false
		}
		if workflow.MatchesAnyGlob(rel, readOnly) {
			return false
		}
		return workflow.MatchesAnyGlob(rel, editable)
	}
	if isGitWorktree(worktree) {
		return gitVerifyAndRevert(ctx, worktree, startTree, allowed)
	}
	return fallbackVerifyAndRevert(worktree, startTree, allowed)
}

func gitVerifyAndRevert(ctx context.Context, worktree, startTree string, allowed func(string) bool) ([]string, error) {
	currentTree, err := gitSnapshotTree(ctx, worktree, "", nil)
	if err != nil {
		return nil, err
	}
	if currentTree == startTree {
		return nil, nil
	}
	env := cleanGitEnv()
	diffCmd := exec.CommandContext(ctx, systemGitBin(), "-C", worktree, "diff-tree", "-r", "--name-only", "-z", startTree, currentTree)
	diffCmd.Env = env
	out, err := diffCmd.Output()
	if err != nil {
		return nil, err
	}
	var offending []string
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		rel := string(raw)
		if !allowed(rel) {
			offending = append(offending, rel)
		}
	}
	sort.Strings(offending)
	for _, rel := range offending {
		if err := gitRestorePathFromTree(ctx, worktree, startTree, rel); err != nil {
			return offending, err
		}
	}
	return offending, nil
}

func gitRestorePathFromTree(ctx context.Context, worktree, tree, rel string) error {
	env := cleanGitEnv()
	gitBin := systemGitBin()
	abs := filepath.Join(worktree, rel)
	checkCmd := exec.CommandContext(ctx, gitBin, "-C", worktree, "cat-file", "-e", tree+":"+rel)
	checkCmd.Env = env
	if checkCmd.Run() != nil {
		_ = os.Remove(abs)
		rmCmd := exec.CommandContext(ctx, gitBin, "-C", worktree, "rm", "--cached", "--ignore-unmatch", "-q", "--", rel)
		rmCmd.Env = env
		_ = rmCmd.Run()
		return nil
	}
	showCmd := exec.CommandContext(ctx, gitBin, "-C", worktree, "cat-file", "-p", tree+":"+rel)
	showCmd.Env = env
	data, err := showCmd.Output()
	if err != nil {
		return err
	}
	return writeAtomicWorktreeFile(abs, data)
}

func fallbackVerifyAndRevert(worktree, startTree string, allowed func(string) bool) ([]string, error) {
	fallbackTreesMu.Lock()
	baseSnap, ok := fallbackTrees[startTree]
	fallbackTreesMu.Unlock()
	if !ok {
		return nil, nil
	}
	curFiles, _, err := scanWorktreeFiles(worktree)
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for k, v := range curFiles {
		orig, exists := baseSnap.Files[k]
		if !exists || !bytes.Equal(orig, v) {
			changed[k] = true
		}
	}
	for k := range baseSnap.Files {
		if _, exists := curFiles[k]; !exists {
			changed[k] = true
		}
	}
	var offending []string
	for rel := range changed {
		if !allowed(rel) {
			offending = append(offending, rel)
		}
	}
	sort.Strings(offending)
	for _, rel := range offending {
		abs := filepath.Join(worktree, rel)
		if orig, exists := baseSnap.Files[rel]; exists {
			if err := writeAtomicWorktreeFile(abs, orig); err != nil {
				return offending, err
			}
		} else {
			_ = os.Remove(abs)
		}
	}
	return offending, nil
}

// RestoreWorktreeCheckpoint restores worktree to match checkpointTree, keeping
// in-scope edits recorded in checkpointTree and reverting any out-of-scope or
// uncheckpointed changes (FAE-RT-019).
func RestoreWorktreeCheckpoint(ctx context.Context, worktree, checkpointTree string) error {
	if checkpointTree == "" {
		return nil
	}
	if isGitWorktree(worktree) {
		currentTree, err := gitSnapshotTree(ctx, worktree, "", nil)
		if err != nil {
			return err
		}
		if currentTree == checkpointTree {
			return nil
		}
		env := cleanGitEnv()
		diffCmd := exec.CommandContext(ctx, systemGitBin(), "-C", worktree, "diff-tree", "-r", "--name-only", "-z", checkpointTree, currentTree)
		diffCmd.Env = env
		out, err := diffCmd.Output()
		if err != nil {
			return err
		}
		for _, raw := range bytes.Split(out, []byte{0}) {
			if len(raw) == 0 {
				continue
			}
			if err := gitRestorePathFromTree(ctx, worktree, checkpointTree, string(raw)); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := fallbackVerifyAndRevert(worktree, checkpointTree, func(string) bool { return false })
	return err
}
