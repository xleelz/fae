package provider

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	stdRuntime "runtime"
	"strings"
	"syscall"
	"time"

	"github.com/xleelz/fae/workflow"
)

type IsolationCapability struct {
	Available bool   `json:"available"`
	Step      string `json:"step"`
	Reason    string `json:"reason,omitempty"`
	Remedy    string `json:"remedy,omitempty"`
}

// ProbeSessionIsolation runs no provider code and probes a fresh physical view,
// including its actual canary mount plan. Each subprocess is time bounded.
func ProbeSessionIsolation(ctx context.Context, logRoot string) IsolationCapability {
	if stdRuntime.GOOS != "linux" {
		return IsolationCapability{Step: "platform", Reason: fmt.Sprintf("%s lacks the required Linux namespace isolation", stdRuntime.GOOS), Remedy: "run namespace-required stages on a Linux host with bubblewrap; collector-only stages do not require namespaces"}
	}
	result := IsolationCapability{Step: "namespace-tool", Remedy: "install bubblewrap and enable unprivileged user, PID and mount namespaces"}
	binary, err := exec.LookPath("bwrap")
	if err != nil {
		result.Reason = err.Error()
		return result
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result.Step = "user-pid-mount-namespaces"
	if err = exec.CommandContext(probeCtx, binary, "--unshare-user", "--unshare-pid", "--cap-drop", "ALL", "--proc", "/proc", "--ro-bind", "/", "/", "--", "true").Run(); err != nil {
		result.Reason = err.Error()
		return result
	}
	root, err := os.MkdirTemp(logRoot, "isolation-probe-")
	if err != nil {
		result.Step = "private-directory"
		result.Reason = err.Error()
		return result
	}
	defer os.RemoveAll(root)
	work := filepath.Join(root, "work")
	logs := filepath.Join(root, "logs")
	if err = os.Mkdir(work, 0700); err == nil {
		err = os.Mkdir(logs, 0700)
	}
	if err != nil {
		result.Step = "private-directory"
		result.Reason = err.Error()
		return result
	}
	cmd := exec.CommandContext(probeCtx, "/bin/true")
	cmd.Dir = work
	guard := ReadGuard{Worktree: work, ForceIsolation: true}
	view, err := guard.ConfigureSessionView(cmd, logs, nil)
	if view != nil {
		defer view.Close()
	}
	if err == nil {
		err = cmd.Run()
	}
	if err != nil {
		result.Step = "mount-plan-canary"
		result.Reason = err.Error()
		return result
	}
	return IsolationCapability{Available: true, Step: "ready"}
}

func ReadRootWritable(path string) bool {
	writable := false
	err := filepath.WalkDir(path, func(full string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if syscall.Access(full, 2) == nil {
			writable = true
			return fs.SkipAll
		}
		return nil
	})
	return writable || err != nil
}
func readPolicyNarrows(p *workflow.ReadPolicy) bool {
	return p != nil && (p.Allow != nil || len(p.Deny) > 0 || len(p.AllowSets) > 0)
}

// RequiresSessionIsolation implements SEC012: metadata/output directories alone
// do not require namespaces. Writable external read roots do when a command or
// shell can access them; a shell with a narrowed read policy always does.
func RequiresSessionIsolation(shell, command bool, policy *workflow.ReadPolicy, deny []string, roots map[string]ReadRootView) bool {
	if shell && (readPolicyNarrows(policy) || len(deny) > 0) {
		return true
	}
	if shell || command {
		for name, root := range roots {
			if strings.HasPrefix(name, "task-directory-") || name == "output" || name == "staging" {
				continue
			}
			if ReadRootWritable(root.Path) {
				return true
			}
		}
	}
	return false
}
func checkRuntimeMount(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(path) || !info.IsDir() {
		return fmt.Errorf("runtime mount must be an absolute directory: %s", path)
	}
	return nil
}

// RuntimeShellWithoutView is a deliberately explicit target opt-in. Every
// executable must be absolute and contained in a pinned read-only runtime mount.
func RuntimeShellWithoutView(profile workflow.Profile, mounts []string) bool {
	if profile.ShellAllow == nil || len(profile.ShellAllow) == 0 {
		return false
	}
	for _, shape := range profile.ShellAllow {
		if len(shape) == 0 || !filepath.IsAbs(shape[0]) {
			return false
		}
		executable, err := filepath.EvalSymlinks(shape[0])
		if err != nil {
			return false
		}
		inside := false
		for _, mount := range mounts {
			physical, e := filepath.EvalSymlinks(mount)
			if e == nil && physicalWithin(physical, executable) {
				inside = true
				break
			}
		}
		if !inside {
			return false
		}
	}
	return true
}

func validateProviderStateDirectories(environ []string) error {
	for _, entry := range environ {
		key, path, ok := strings.Cut(entry, "=")
		if !ok || path == "" || (key != "CLAUDE_CONFIG_DIR" && key != "CODEX_HOME") {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) == "/" {
			return fmt.Errorf("%s must name an absolute configuration directory", key)
		}
		if err := PhysicalTaskDirectory(path); err != nil {
			return fmt.Errorf("%s cannot be mounted: %w", key, err)
		}
		if syscall.Access(path, 2) != nil {
			return fmt.Errorf("%s configuration directory is not writable", key)
		}
	}
	return nil
}
