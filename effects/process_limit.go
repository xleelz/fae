package effects

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// DefaultProcessLimit is the per-run task cap (cgroup v2 pids.max counts
// processes and threads). The spec gives no value (FAE-HOST-015); 512 tasks
// leaves a Go toolchain run with process-spawning tests ample room while
// stopping a fork storm long before it loads the host.
const DefaultProcessLimit = 512

// ProcessLimit is FAE-HOST-015's per-run process cap: each brokered command
// runs in its own transient systemd user scope whose TasksMax (pids.max)
// bounds every task of the run, descendants included. The scope keeps the
// command's PID, so process ownership and group retirement are unchanged.
type ProcessLimit struct {
	Tasks   int    `json:"tasks"`
	Runner  string `json:"runner"`
	Runtime string `json:"runtime"`
	// Rlimit marks the degraded cap of a host without delegated pids: the run
	// starts under prlimit with RLIMIT_NPROC at the user's current task count
	// plus Tasks, so it can create at most Tasks more (fewer when the user's
	// other tasks grow meanwhile). Degraded names why the scope was unavailable.
	Rlimit   bool   `json:"rlimit,omitempty"`
	Degraded string `json:"degraded,omitempty"`
}

// ProbeProcessLimit returns the per-run cap FAE-HOST-015 requires: a systemd
// scope when the user's manager has the pids controller delegated and a probe
// scope enforces it, else the RLIMIT_NPROC fallback. Only when neither can cap
// a run does it return nil and the reason.
func ProbeProcessLimit(ctx context.Context, tasks int) (*ProcessLimit, error) {
	p, err := probeScope(ctx, tasks)
	if err == nil {
		return p, nil
	}
	fallback, ferr := probeRlimit(ctx, tasks)
	if ferr != nil {
		return nil, errors.Join(err, ferr)
	}
	fallback.Degraded = err.Error()
	return fallback, nil
}

func probeScope(ctx context.Context, tasks int) (*ProcessLimit, error) {
	if runtime.GOOS != "linux" {
		return nil, errors.New("per-run process limit needs Linux cgroup v2")
	}
	if tasks < 8 {
		return nil, fmt.Errorf("process limit %d below 8 tasks", tasks)
	}
	uid := os.Getuid()
	control, err := os.ReadFile(fmt.Sprintf("/sys/fs/cgroup/user.slice/user-%d.slice/user@%d.service/cgroup.subtree_control", uid, uid))
	if err != nil {
		return nil, fmt.Errorf("cgroup v2 user delegation unavailable: %w", err)
	}
	if !strings.Contains(" "+strings.TrimSpace(string(control))+" ", " pids ") {
		return nil, errors.New("pids controller not delegated to the user manager")
	}
	runner, err := exec.LookPath("systemd-run")
	if err != nil {
		return nil, fmt.Errorf("systemd-run unavailable: %w", err)
	}
	runner, _ = filepath.Abs(runner)
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", uid)
	}
	if st, err := os.Stat(filepath.Join(dir, "bus")); err != nil || st.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("user manager bus unavailable under %s", dir)
	}
	p := &ProcessLimit{Tasks: tasks, Runner: runner, Runtime: dir}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	argv, env := p.wrap([]string{"/bin/sh", "-c", `cat "/sys/fs/cgroup$(cut -d: -f3 /proc/self/cgroup)/pids.max"`}, []string{"PATH=/usr/bin:/bin"})
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("probe scope failed: %w", err)
	}
	if got := strings.TrimSpace(string(bytes.TrimSpace(out))); got != strconv.Itoa(tasks) {
		return nil, fmt.Errorf("probe scope pids.max %q, want %d", got, tasks)
	}
	return p, nil
}

// probeRlimit checks that prlimit can cap RLIMIT_NPROC for a child. The limit
// never binds a privileged user, so root gets no fallback.
func probeRlimit(ctx context.Context, tasks int) (*ProcessLimit, error) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 {
		return nil, errors.New("RLIMIT_NPROC fallback needs an unprivileged Linux user")
	}
	if tasks < 8 {
		return nil, fmt.Errorf("process limit %d below 8 tasks", tasks)
	}
	runner, err := exec.LookPath("prlimit")
	if err != nil {
		return nil, fmt.Errorf("prlimit unavailable: %w", err)
	}
	runner, _ = filepath.Abs(runner)
	p := &ProcessLimit{Tasks: tasks, Runner: runner, Rlimit: true}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	n := rlimitFor(tasks)
	argv := []string{runner, fmt.Sprintf("--nproc=%d:%d", n, n), "--", "/bin/sh", "-c", `sed -n 's/^Max processes *\([0-9]*\).*/\1/p' /proc/self/limits`}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("prlimit probe failed: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != strconv.Itoa(n) || n < tasks {
		return nil, fmt.Errorf("prlimit probe limit %q, want %d", got, n)
	}
	return p, nil
}

// rlimitFor is the RLIMIT_NPROC that leaves room for tasks more tasks of this
// user, clamped to the current hard limit.
func rlimitFor(tasks int) int {
	n := userTasks() + tasks
	var hard syscall.Rlimit
	if syscall.Getrlimit(rlimitNproc, &hard) == nil && hard.Max != ^uint64(0) && uint64(n) > hard.Max {
		n = int(hard.Max)
	}
	return n
}

// rlimitNproc is RLIMIT_NPROC on Linux.
const rlimitNproc = 6

// userTasks counts the tasks (threads) of this real user, which RLIMIT_NPROC
// bounds.
func userTasks() int {
	uid := strconv.Itoa(os.Getuid())
	entries, _ := os.ReadDir("/proc")
	n := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		mine, threads := false, 0
		for _, line := range strings.Split(string(raw), "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[0] == "Uid:" {
				mine = f[1] == uid
			} else if len(f) >= 2 && f[0] == "Threads:" {
				threads, _ = strconv.Atoi(f[1])
			}
		}
		if mine {
			n += threads
		}
	}
	return n
}

// wrap runs argv inside a fresh scope capped at p.Tasks; the scope exec()s the
// command in place and is collected when it exits. The degraded cap execs it
// under prlimit with RLIMIT_NPROC set for this run.
func (p *ProcessLimit) wrap(argv, env []string) ([]string, []string) {
	if p.Rlimit {
		n := strconv.Itoa(rlimitFor(p.Tasks))
		return append([]string{p.Runner, "--nproc=" + n + ":" + n, "--"}, argv...), append([]string(nil), env...)
	}
	out := []string{p.Runner, "--user", "--scope", "--quiet", "--collect", "--property=TasksMax=" + strconv.Itoa(p.Tasks), "--"}
	return append(out, argv...), append(append([]string(nil), env...), "XDG_RUNTIME_DIR="+p.Runtime)
}
