package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func muxFixture(t *testing.T, mode string) (*Multiplexer, Launch) {
	t.Helper()
	binary, e := exec.LookPath("herdr")
	if e != nil {
		t.Skip("herdr not installed")
	}
	d, l := directFixture(t, mode)
	base := "/private/tmp"
	if _, e = os.Stat(base); e != nil {
		base = "/tmp"
	}
	root, e := os.MkdirTemp(base, "fae-mux-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		t.Fatal(e)
	}
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
			if time.Since(start) > 5*time.Second {
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
	cmd := exec.Command(binary, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + filepath.Dir(socket), "HERDR_SOCKET_PATH=" + socket}
	return cmd
}
func TestMultiplexerRefusesForeignWorkspace(t *testing.T) {
	m, l := muxFixture(t, "")
	dir := filepath.Join(m.Direct.Root, "foreign")
	if e := os.Mkdir(dir, 0700); e != nil {
		t.Fatal(e)
	}
	socket := filepath.Join(dir, "mux.sock")
	server := herdrCommand(m.Binary, socket, "server")
	if e := server.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { herdrCommand(m.Binary, socket, "server", "stop").Run(); server.Process.Kill(); server.Wait() })
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e := os.Stat(socket); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server startup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if out, e := herdrCommand(m.Binary, socket, "workspace", "create", "--cwd", dir, "--label", "foreign", "--no-focus").CombinedOutput(); e != nil {
		t.Fatalf("%s %v", out, e)
	}
	cmd := exec.Command("/bin/true")
	cmd.Dir = l.Worktree
	if hooks, e := m.prepare(context.Background(), cmd, dir); e == nil {
		hooks.Close()
		t.Fatal("adopted foreign server")
	}
	out, e := herdrCommand(m.Binary, socket, "workspace", "list").Output()
	if e != nil || !strings.Contains(string(out), `"label":"foreign"`) {
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
	root := os.Getenv("FAE_MUX_CRASH_ROOT")
	if root == "" {
		return
	}
	exe, _ := os.Executable()
	m := &Multiplexer{Direct: &Direct{Root: root, Command: []string{exe, "-test.run=^TestBridgeHelper$", "--", "{config}"}, Environment: map[string]string{"BRIDGE_HELPER": "1", "BRIDGE_MODE": "descendants", "GORACE": "atexit_sleep_ms=0"}}, Binary: os.Getenv("FAE_MUX_BINARY"), Instance: "crash-test"}
	_, e := m.Run(context.Background(), Launch{DispatchID: "crash", SessionID: "crash", TaskID: "crash", Role: "coder", Worktree: root, Mode: "local", Brief: []byte(`[]`)})
	if e != nil {
		t.Fatal(e)
	}
}
func TestMultiplexerOwnerSIGKILL(t *testing.T) {
	m, l := muxFixture(t, "")
	exe, _ := os.Executable()
	parent := exec.Command(exe, "-test.run=^TestMultiplexerCrashHelper$")
	parent.Env = append(os.Environ(), "FAE_MUX_CRASH_ROOT="+m.Direct.Root, "FAE_MUX_BINARY="+m.Binary, "GORACE=atexit_sleep_ms=0")
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
		if e := herdrCommand(m.Binary, socket, "workspace", "list").Run(); e == nil {
			t.Fatal("orphan herdr server survived")
		}
	}
}
