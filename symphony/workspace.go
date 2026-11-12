package symphony

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// MaxHookOutputBytes caps captured stdout/stderr from workspace lifecycle hooks.
const MaxHookOutputBytes = 64 * 1024

// WorkspaceKey produces a filesystem-safe directory name for an issue identifier.
// Only characters in [A-Za-z0-9._-] are permitted. If any character in identifier
// is replaced (or if identifier is empty, ".", or ".."), a 16-hex-char SHA-256 suffix
// is appended so distinct identifiers that sanitize to the same prefix never collide
// (Symphony §4.2 & §9.2).
func WorkspaceKey(identifier string) string {
	var sb strings.Builder
	sb.Grow(len(identifier))
	modified := false

	for i := 0; i < len(identifier); i++ {
		c := identifier[i]
		if isSafeWorkspaceKeyByte(c) {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('_')
			modified = true
		}
	}

	sanitized := sb.String()
	if identifier == "" || sanitized == "." || sanitized == ".." {
		modified = true
		sanitized = strings.Repeat("_", max(1, len(sanitized)))
	}

	if !modified {
		return sanitized
	}

	sum := sha256.Sum256([]byte(identifier))
	suffix := hex.EncodeToString(sum[:8])
	return sanitized + "-" + suffix
}

func isSafeWorkspaceKeyByte(c byte) bool {
	return (c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9') ||
		c == '.' || c == '_' || c == '-'
}

// ValidateWorkspaceContainment verifies that workspacePath is strictly inside workspaceRoot (Symphony §9.5 Invariant 2).
func ValidateWorkspaceContainment(workspaceRoot, workspacePath string) error {
	if strings.TrimSpace(workspaceRoot) == "" {
		return errors.New("workspace root is empty")
	}
	if strings.TrimSpace(workspacePath) == "" {
		return errors.New("workspace path is empty")
	}

	absRoot, err := filepath.Abs(filepath.Clean(workspaceRoot))
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	absPath, err := filepath.Abs(filepath.Clean(workspacePath))
	if err != nil {
		return fmt.Errorf("resolve workspace path: %w", err)
	}

	prefix := strings.TrimRight(absRoot, string(filepath.Separator)) + string(filepath.Separator)
	if absPath == absRoot || !strings.HasPrefix(absPath, prefix) {
		return fmt.Errorf("workspace path %q is outside workspace root %q", absPath, absRoot)
	}

	// If both exist on disk, also verify resolved symlinks stay inside root
	if realRoot, err := filepath.EvalSymlinks(absRoot); err == nil {
		if realPath, err := filepath.EvalSymlinks(absPath); err == nil {
			realPrefix := strings.TrimRight(realRoot, string(filepath.Separator)) + string(filepath.Separator)
			if realPath == realRoot || !strings.HasPrefix(realPath, realPrefix) {
				return fmt.Errorf("symlinked workspace path %q escapes workspace root %q", realPath, realRoot)
			}
		}
	}

	return nil
}

// ValidateAgentCwd enforces Symphony §9.5 safety invariants before launching the coding-agent subprocess:
// Invariant 1: cwd == expectedWorkspacePath
// Invariant 2: expectedWorkspacePath is strictly inside workspaceRoot
func ValidateAgentCwd(workspaceRoot, cwd, expectedWorkspacePath string) error {
	if err := ValidateWorkspaceContainment(workspaceRoot, expectedWorkspacePath); err != nil {
		return err
	}
	if strings.TrimSpace(cwd) == "" {
		return errors.New("agent cwd is empty")
	}
	absCwd, err := filepath.Abs(filepath.Clean(cwd))
	if err != nil {
		return fmt.Errorf("resolve agent cwd: %w", err)
	}
	absExpected, err := filepath.Abs(filepath.Clean(expectedWorkspacePath))
	if err != nil {
		return fmt.Errorf("resolve expected workspace path: %w", err)
	}
	if absCwd != absExpected {
		return fmt.Errorf("agent cwd %q does not match expected workspace path %q", absCwd, absExpected)
	}
	return nil
}

// WorkspaceManager manages per-issue workspaces and lifecycle hooks (Symphony §9).
type WorkspaceManager struct {
	Root   string
	Hooks  HooksConfig
	Logger func(format string, args ...any)
}

// NewWorkspaceManager creates a WorkspaceManager rooted at root with the given hooks configuration.
func NewWorkspaceManager(root string, hooks HooksConfig) (*WorkspaceManager, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root must not be empty")
	}
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, err
	}
	if hooks.TimeoutMS <= 0 {
		hooks.TimeoutMS = DefaultHooksTimeoutMS
	}
	return &WorkspaceManager{
		Root:  absRoot,
		Hooks: hooks,
	}, nil
}

// NewWorkspaceManagerFromConfig creates a WorkspaceManager from a resolved ServiceConfig.
func NewWorkspaceManagerFromConfig(cfg *ServiceConfig) (*WorkspaceManager, error) {
	if cfg == nil {
		return nil, errors.New("service config is nil")
	}
	return NewWorkspaceManager(cfg.Workspace.Root, cfg.Hooks)
}

// PathForIssue returns the deterministic absolute workspace path and sanitized key for identifier.
func (wm *WorkspaceManager) PathForIssue(identifier string) (string, string, error) {
	if strings.TrimSpace(identifier) == "" {
		return "", "", errors.New("issue identifier must not be empty")
	}
	if strings.TrimSpace(wm.Root) == "" {
		return "", "", errors.New("workspace root is empty")
	}
	absRoot, err := filepath.Abs(filepath.Clean(wm.Root))
	if err != nil {
		return "", "", err
	}
	key := WorkspaceKey(identifier)
	absPath := filepath.Clean(filepath.Join(absRoot, key))
	if err := ValidateWorkspaceContainment(absRoot, absPath); err != nil {
		return "", "", err
	}
	return absPath, key, nil
}

// CreateForIssue ensures the workspace directory for identifier exists and runs the
// after_create hook when the directory is newly created (Symphony §§9.2, 9.3, 9.4).
func (wm *WorkspaceManager) CreateForIssue(ctx context.Context, identifier string) (Workspace, error) {
	absPath, key, err := wm.PathForIssue(identifier)
	if err != nil {
		return Workspace{}, err
	}
	absRoot, err := filepath.Abs(filepath.Clean(wm.Root))
	if err != nil {
		return Workspace{}, err
	}
	if err := os.MkdirAll(absRoot, 0o755); err != nil {
		return Workspace{}, fmt.Errorf("create workspace root: %w", err)
	}

	createdNow := false
	info, statErr := os.Lstat(absPath)
	if statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return Workspace{}, fmt.Errorf("workspace path %q is a symlink, not a directory", absPath)
		}
		if !info.IsDir() {
			return Workspace{}, fmt.Errorf("workspace path %q exists and is not a directory", absPath)
		}
	} else if os.IsNotExist(statErr) {
		if err := os.MkdirAll(absPath, 0o755); err != nil {
			return Workspace{}, fmt.Errorf("create workspace directory: %w", err)
		}
		createdNow = true
	} else {
		return Workspace{}, fmt.Errorf("stat workspace path %q: %w", absPath, statErr)
	}

	// Re-verify containment after directory exists on disk
	if err := ValidateWorkspaceContainment(absRoot, absPath); err != nil {
		if createdNow {
			_ = os.RemoveAll(absPath)
		}
		return Workspace{}, err
	}

	if createdNow && wm.Hooks.AfterCreate != nil && strings.TrimSpace(*wm.Hooks.AfterCreate) != "" {
		if err := wm.runHook(ctx, "after_create", *wm.Hooks.AfterCreate, absPath); err != nil {
			_ = os.RemoveAll(absPath)
			return Workspace{}, err
		}
	}

	return Workspace{
		Path:         absPath,
		WorkspaceKey: key,
		CreatedNow:   createdNow,
	}, nil
}

// RunBeforeRun executes the before_run hook in workspacePath if configured.
// Any failure or timeout returns an error and aborts the current attempt (Symphony §9.4).
func (wm *WorkspaceManager) RunBeforeRun(ctx context.Context, workspacePath string) error {
	if err := ValidateWorkspaceContainment(wm.Root, workspacePath); err != nil {
		return err
	}
	if wm.Hooks.BeforeRun == nil || strings.TrimSpace(*wm.Hooks.BeforeRun) == "" {
		return nil
	}
	return wm.runHook(ctx, "before_run", *wm.Hooks.BeforeRun, workspacePath)
}

// RunAfterRun executes the after_run hook in workspacePath if configured.
// Failures or timeouts are logged and ignored per Symphony §9.4.
func (wm *WorkspaceManager) RunAfterRun(ctx context.Context, workspacePath string) error {
	if err := ValidateWorkspaceContainment(wm.Root, workspacePath); err != nil {
		wm.logf("after_run containment error: %v", err)
		return nil
	}
	if wm.Hooks.AfterRun == nil || strings.TrimSpace(*wm.Hooks.AfterRun) == "" {
		return nil
	}
	if err := wm.runHook(ctx, "after_run", *wm.Hooks.AfterRun, workspacePath); err != nil {
		wm.logf("after_run hook warning (ignored): %v", err)
	}
	return nil
}

// CleanupIssueWorkspace executes before_remove (ignoring hook failure/timeout) and removes
// the workspace directory for identifier if it exists (Symphony §9.4).
func (wm *WorkspaceManager) CleanupIssueWorkspace(ctx context.Context, identifier string) error {
	absPath, _, err := wm.PathForIssue(identifier)
	if err != nil {
		return err
	}
	info, statErr := os.Lstat(absPath)
	if os.IsNotExist(statErr) {
		return nil
	}
	if statErr != nil {
		return statErr
	}

	if info.IsDir() && wm.Hooks.BeforeRemove != nil && strings.TrimSpace(*wm.Hooks.BeforeRemove) != "" {
		if hookErr := wm.runHook(ctx, "before_remove", *wm.Hooks.BeforeRemove, absPath); hookErr != nil {
			wm.logf("before_remove hook warning (ignored): %v", hookErr)
		}
	}

	return os.RemoveAll(absPath)
}

func (wm *WorkspaceManager) hookTimeout() time.Duration {
	if wm.Hooks.TimeoutMS > 0 {
		return time.Duration(wm.Hooks.TimeoutMS) * time.Millisecond
	}
	return time.Duration(DefaultHooksTimeoutMS) * time.Millisecond
}

func (wm *WorkspaceManager) runHook(parentCtx context.Context, hookName string, script string, workspacePath string) error {
	if parentCtx == nil {
		parentCtx = context.Background()
	}
	timeout := wm.hookTimeout()
	wm.logf("running hook %s in %s (timeout=%v)", hookName, workspacePath, timeout)
	ctx, cancel := context.WithTimeout(parentCtx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "sh", "-lc", script)
	cmd.Dir = workspacePath
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 100 * time.Millisecond

	var outBuf limitedBuffer
	outBuf.limit = MaxHookOutputBytes
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf

	err := cmd.Run()
	if err != nil {
		output := strings.TrimSpace(outBuf.String())
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			wm.logf("hook %s timed out after %v in %s", hookName, timeout, workspacePath)
			if output != "" {
				return fmt.Errorf("hook %s timed out after %v: %s", hookName, timeout, output)
			}
			return fmt.Errorf("hook %s timed out after %v", hookName, timeout)
		}
		wm.logf("hook %s failed in %s: %v", hookName, workspacePath, err)
		if output != "" {
			return fmt.Errorf("hook %s failed: %w (output: %s)", hookName, err, output)
		}
		return fmt.Errorf("hook %s failed: %w", hookName, err)
	}
	return nil
}

func (wm *WorkspaceManager) logf(format string, args ...any) {
	if wm.Logger != nil {
		wm.Logger(format, args...)
	}
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	total := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buf.Write(p)
	}
	return total, nil
}

func (b *limitedBuffer) String() string {
	return b.buf.String()
}
