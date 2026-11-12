package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func muxFixture(t *testing.T, mode string) (*Multiplexer, Launch) {
	t.Helper()
	binary, e := exec.LookPath("herdr-core")
	if e != nil {
		t.Skip("herdr-core not installed")
	}
	d, l := directFixture(t, mode)
	base := "/private/tmp"
	if _, e = os.Stat(base); e != nil {
		base = "/var/tmp"
	}
	root, e := os.MkdirTemp(base, "fae-mux-")
	if e != nil {
		t.Fatal(e)
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		t.Fatal(e)
	}
	// Runs last: a Herdr server left by a failed test must not outlive its
	// deleted root, where its dead socket fails every host census closed.
	t.Cleanup(func() { reapUnder(t, root); os.RemoveAll(root) })
	d.Root = root
	l.Worktree = root
	return &Multiplexer{Direct: d, Binary: binary, Instance: "fae-test"}, l
}
func TestMultiplexerConformance(t *testing.T) {
	for _, adapter := range []string{"direct", "multiplexer"} {
		t.Run(adapter, func(t *testing.T) {
			for mode, class := range map[string]string{"": "", "missing": "missing-handoff", "stdout": "missing-handoff", "malformed": "malformed-output", "idless": "id-less-handoff", "stale": "stale-handoff", "nonzero": "process-failed", "fifo": "malformed-output", "symlink": "missing-handoff", "large": "malformed-output"} {
				t.Run(mode, func(t *testing.T) {
					var run func(context.Context, Launch) (Collected, error)
					var l Launch
					if adapter == "direct" {
						d, launch := directFixture(t, mode)
						l = launch
						run = d.Run
					} else {
						m, launch := muxFixture(t, mode)
						l = launch
						run = m.Run
					}
					out, e := run(context.Background(), l)
					if class != "" {
						var te *TurnError
						if !errors.As(e, &te) || te.Class != class {
							t.Fatalf("want %s got %v", class, e)
						}
						return
					}
					if e != nil {
						t.Fatal(e)
					}
					if len(out.Tail) != 128 || !strings.Contains(string(out.Output), "9007199254740993") || !strings.Contains(string(out.Output), "$(touch nope); literal") {
						t.Fatalf("output=%s tail=%q", out.Output, out.Tail)
					}
					if _, e = run(context.Background(), l); e == nil {
						t.Fatal("reused session")
					}
				})
			}
		})
	}
}
func TestMultiplexerAmbientAndCancellation(t *testing.T) {
	t.Setenv("HC_SOCKET", "/hostile/socket,111,0")
	t.Setenv("HERDR_SOCKET_PATH", "/hostile/socket,111,0")
	t.Setenv("HERDR_PANE_ID", "%999")
	m, l := muxFixture(t, "")
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"wait", "descendants", "exit-with-child"} {
		t.Run(mode, func(t *testing.T) {
			m, l := muxFixture(t, mode)
			m.Direct.Timeout = 2 * time.Second
			start := time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "exit-with-child" {
				// A completing turn, not a cancellation: its bound only guards a
				// hang, so a loaded host (race build) does not time it out.
				m.Direct.Timeout = 30 * time.Second
			}
			if mode == "descendants" {
				m.Direct.Timeout = 5 * time.Second
				go func() {
					for {
						if _, e := os.Stat(filepath.Join(l.Worktree, "bridge-started")); e == nil {
							cancel()
							return
						}
						select {
						case <-ctx.Done():
							return
						case <-time.After(5 * time.Millisecond):
						}
					}
				}()
			}
			_, e := m.Run(ctx, l)
			if mode == "descendants" {
				if _, err := os.Stat(filepath.Join(l.Worktree, "bridge-started")); err != nil {
					t.Fatal("provider child never started", err)
				}
			}
			if mode != "exit-with-child" {
				var te *TurnError
				if !errors.As(e, &te) || te.Class != "interrupted" {
					t.Fatal(e)
				}
			} else if e != nil {
				t.Fatal(e)
			}
			if mode != "exit-with-child" && time.Since(start) > 5*time.Second {
				t.Fatal("unbounded cancellation")
			}
			time.Sleep(800 * time.Millisecond)
			if _, e = os.Stat(filepath.Join(l.Worktree, "child-survived")); !os.IsNotExist(e) {
				t.Fatal("descendant survived")
			}
			if len(m.Sessions()) != 0 {
				t.Fatal("live session retained")
			}
		})
	}
}

func herdrCommand(binary, socket string, args ...string) *exec.Cmd {
	dir := filepath.Dir(socket)
	cmd := exec.Command(binary, args...)
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + dir,
		"TMPDIR=" + dir,
		"HC_SOCKET=" + socket,
		"HC_SESSION=foreign",
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "cfg"),
		"XDG_RUNTIME_DIR=" + filepath.Join(dir, "run"),
		"XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"XDG_DATA_HOME=" + filepath.Join(dir, "data"),
		"HERDR_CONFIG_PATH=" + filepath.Join(dir, "cfg", "herdr-core", "config.toml"),
		"HERDR_SOCKET_PATH=" + socket,
	}
	return cmd
}
func TestMultiplexerRefusesForeignWorkspace(t *testing.T) {
	m, l := muxFixture(t, "")
	dir := filepath.Join(m.Direct.Root, "foreign")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	socket := filepath.Join(dir, "mux.sock")
	if _, _, e := writeHerdrCoreConfig(dir, socket, "foreign", "/bin/sh"); e != nil {
		t.Fatal(e)
	}
	server := herdrCommand(m.Binary, socket, "serve")
	if e := server.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { herdrCommand(m.Binary, socket, "server", "stop").Run(); server.Process.Kill(); server.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if socketReady(socket) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server startup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out, e := herdrCommand(m.Binary, socket, "run", "--bg", "--no-parent", "--no-notify", "--name", "foreign", "--cwd", dir, "--json").CombinedOutput(); e != nil {
		t.Fatalf("%s %v", out, e)
	}
	cmd := exec.Command("/bin/true")
	cmd.Dir = l.Worktree
	if hooks, e := m.prepare(context.Background(), cmd, dir); e == nil {
		hooks.Close()
		t.Fatal("adopted foreign server")
	}
	out, e := herdrCommand(m.Binary, socket, "ls", "--json").Output()
	if e != nil || !strings.Contains(string(out), `"name":"foreign"`) {
		t.Fatalf("foreign workspace disturbed: %s %v", out, e)
	}
}
func TestMultiplexerConcurrentSessions(t *testing.T) {
	m, l := muxFixture(t, "")
	type result struct {
		out Collected
		err error
	}
	results := make(chan result, 3)
	for _, id := range []string{"one", "two", "three"} {
		launch := l
		launch.SessionID = id
		launch.DispatchID = id
		go func() { out, e := m.Run(context.Background(), launch); results <- result{out, e} }()
	}
	dirs := map[string]bool{}
	for range 3 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		if dirs[r.out.LogDirectory] {
			t.Fatal("shared session directory")
		}
		dirs[r.out.LogDirectory] = true
	}
	if len(m.Sessions()) != 0 {
		t.Fatal("live session leak")
	}
}

func TestMultiplexerCrashHelper(t *testing.T) {
	root := os.Getenv("MUX_CRASH_ROOT")
	if root == "" {
		return
	}
	exe, _ := os.Executable()
	m := &Multiplexer{Direct: &Direct{Root: root, Command: []string{exe, "-test.run=^TestBridgeHelper$", "--", "{config}"}, Environment: map[string]string{"BRIDGE_HELPER": "1", "BRIDGE_MODE": "descendants", "GORACE": "atexit_sleep_ms=0"}}, Binary: os.Getenv("MUX_BINARY"), Instance: "crash-test"}
	_, e := m.Run(context.Background(), Launch{DispatchID: "crash", SessionID: "crash", TaskID: "crash", Role: "coder", Worktree: root, Mode: "local", Brief: []byte(`[]`)})
	if e != nil {
		t.Fatal(e)
	}
}
func TestMultiplexerOwnerSIGKILL(t *testing.T) {
	m, l := muxFixture(t, "")
	exe, _ := os.Executable()
	parent := exec.Command(exe, "-test.run=^TestMultiplexerCrashHelper$")
	parent.Env = append(os.Environ(), "MUX_CRASH_ROOT="+m.Direct.Root, "MUX_BINARY="+m.Binary, "GORACE=atexit_sleep_ms=0")
	if e := parent.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { parent.Process.Kill(); parent.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(l.Worktree, "bridge-started")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	sockets, _ := filepath.Glob(filepath.Join(m.Direct.Root, "session-*", "mux.sock"))
	if len(sockets) != 1 {
		t.Fatalf("expected one private socket: %v", sockets)
	}
	if e := parent.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = parent.Wait()
	time.Sleep(time.Second)
	if _, e := os.Stat(filepath.Join(l.Worktree, "child-survived")); !os.IsNotExist(e) {
		t.Fatal("orphan provider child survived owner SIGKILL")
	}
	for _, socket := range sockets {
		if socketReady(socket) {
			t.Fatal("orphan herdr server survived")
		}
	}
}

// reapUnder kills this user's processes whose working directory or HOME,
// TMPDIR or Herdr socket lies under a test's private root. Only the test's own
// unique root is matched, never an operator workspace.
func reapUnder(t *testing.T, root string) {
	t.Helper()
	under := func(p string) bool {
		p = strings.TrimSuffix(p, " (deleted)")
		return p == root || strings.HasPrefix(p, root+"/")
	}
	find := func() []int {
		var pids []int
		entries, _ := os.ReadDir("/proc")
		for _, entry := range entries {
			pid, e := strconv.Atoi(entry.Name())
			if e != nil || pid == os.Getpid() {
				continue
			}
			dir := filepath.Join("/proc", entry.Name())
			cwd, _ := os.Readlink(filepath.Join(dir, "cwd"))
			match := cwd != "" && under(cwd)
			environ, _ := os.ReadFile(filepath.Join(dir, "environ"))
			for _, v := range strings.Split(string(environ), "\x00") {
				name, value, _ := strings.Cut(v, "=")
				if (name == "HOME" || name == "TMPDIR" || name == "HERDR_SOCKET_PATH" || name == "HC_SOCKET" || name == "XDG_CONFIG_HOME") && under(value) {
					match = true
				}
			}
			if stat, e := os.ReadFile(filepath.Join(dir, "stat")); match && e == nil && !strings.Contains(string(stat), ") Z ") {
				pids = append(pids, pid)
			}
		}
		return pids
	}
	left := find()
	for deadline := time.Now().Add(5 * time.Second); len(left) > 0 && time.Now().Before(deadline); left = find() {
		time.Sleep(50 * time.Millisecond)
	}
	for _, pid := range left {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		if t.Failed() {
			t.Logf("reaped leftover process %d under %s", pid, root)
		} else {
			t.Errorf("leftover process %d under %s", pid, root)
		}
	}
}

func TestMultiplexerLabelsAndOSC7501CleanLogs(t *testing.T) {
	m, l := muxFixture(t, "")
	l.TaskID = "task-label-42"
	l.Stage = "implement"
	m.Labels = map[string]string{
		"fae.molecule": "mol-7",
		"herdr.tab":    "mol-7",
	}
	gotLabels := buildHerdrLabels(m.Instance, "multiplexer-turn", &l, m.Labels)
	want := []string{
		"fae.instance=fae-test",
		"fae.molecule=mol-7",
		"fae.role=multiplexer-turn",
		"fae.stage=implement",
		"fae.task=task-label-42",
		"herdr.space=fae-test",
		"herdr.tab=mol-7",
	}
	if strings.Join(gotLabels, ",") != strings.Join(want, ",") {
		t.Fatalf("buildHerdrLabels = %v, want %v", gotLabels, want)
	}
	out, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out.Tail, "7501") {
		t.Fatalf("OSC 7501 leaked into output tail: %q", out.Tail)
	}
	wrapperRaw, e := os.ReadFile(filepath.Join(out.LogDirectory, "pane.sh"))
	if e != nil {
		t.Fatal(e)
	}
	wrapper := string(wrapperRaw)
	if !strings.Contains(wrapper, "7501;state=%s;app=fae") || !strings.Contains(wrapper, "osc7501 working") || !strings.Contains(wrapper, "osc7501 done") || !strings.Contains(wrapper, "osc7501 blocked") {
		t.Fatalf("pane.sh missing OSC 7501 lifecycle hooks:\n%s", wrapper)
	}
	for _, logFile := range []string{"stdout.log", "stderr.log"} {
		raw, err := os.ReadFile(filepath.Join(out.LogDirectory, logFile))
		if err == nil && strings.Contains(string(raw), "7501") {
			t.Fatalf("OSC 7501 leaked into %s: %q", logFile, string(raw))
		}
	}
}
