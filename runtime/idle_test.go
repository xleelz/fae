package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/processowner"
)

func idleFixture(t *testing.T) (*OwnedIdleSessions, string) {
	t.Helper()
	base := "/tmp"
	if _, e := os.Stat("/private/tmp"); e == nil {
		base = "/private/tmp"
	}
	root, e := os.MkdirTemp(base, "fi-")
	if e != nil {
		t.Fatal(e)
	}
	root, e = filepath.EvalSymlinks(root)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	p, e := NewOwnedIdleSessions(filepath.Join(root, "handoffs"), root)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = p.Close(ctx)
	})
	return p, root
}
func registerIdleProcess(t *testing.T, p *OwnedIdleSessions, root, id string, protected bool, now time.Time) string {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r, e := processowner.StartLinked(cmd, filepath.Join(root, ".processes"), root, id)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = processowner.Terminate(ctx, r, root)
		_ = cmd.Wait()
		_ = processowner.Retire(r)
	})
	socket := filepath.Join(root, id+".sock")
	e = p.Register(IdleRegistration{ID: id, Socket: socket, Role: "planner", Global: true, PreCode: true, Protected: protected, Records: []*processowner.Record{r}, Context: json.RawMessage(`{"task":"old","summary":"retained"}`), Reap: func() error {
		e := cmd.Wait()
		var exit *exec.ExitError
		if errors.As(e, &exit) {
			return nil
		}
		return e
	}}, now)
	if e != nil {
		t.Fatal(e)
	}
	return socket
}
func TestIdleShedsOneAndRetainsHandoff(t *testing.T) {
	p, root := idleFixture(t)
	now := time.Now()
	a := registerIdleProcess(t, p, root, "a", false, now)
	b := registerIdleProcess(t, p, root, "b", false, now)
	for round := 0; round < 3; round++ {
		acts, e := p.Round(context.Background(), map[string]int{a: 1, b: 1}, 2, 0, now)
		if e != nil {
			t.Fatal(e)
		}
		if round < 2 && len(acts) != 0 {
			t.Fatal(acts)
		}
		if round == 2 && (len(acts) != 1 || acts[0].Session != "a" || acts[0].Kind != "idle-session-shed") {
			t.Fatal(acts)
		}
	}
	if len(p.sessions) != 1 {
		t.Fatal("did not shed exactly one")
	}
	h, e := p.Handoff("a")
	if e != nil || !json.Valid(h) {
		t.Fatal(string(h), e)
	}
	reopened, e := NewOwnedIdleSessions(p.root, root)
	if e != nil {
		t.Fatal(e)
	}
	h2, e := reopened.Handoff("a")
	if e != nil || string(h2) != string(h) {
		t.Fatal("handoff not retained across restart", e)
	}
}
func TestIdleForeignSaturationNeverEscalates(t *testing.T) {
	p, root := idleFixture(t)
	now := time.Now()
	a := registerIdleProcess(t, p, root, "a", true, now)
	for i := 0; i < 12; i++ {
		acts, e := p.Round(context.Background(), map[string]int{a: 1, "foreign": 2}, 3, 0, now)
		if e != nil || len(acts) != 0 {
			t.Fatal(acts, e)
		}
	}
}
func TestIdleProtectedSaturationEscalates(t *testing.T) {
	p, root := idleFixture(t)
	now := time.Now()
	a := registerIdleProcess(t, p, root, "a", true, now)
	for i := 0; i < 3; i++ {
		acts, e := p.Round(context.Background(), map[string]int{a: 1}, 1, 0, now)
		if e != nil {
			t.Fatal(e)
		}
		if i == 2 && (len(acts) != 1 || acts[0].Kind != "PaneCapStarvation") {
			t.Fatal(acts)
		}
	}
	if len(p.sessions) != 1 {
		t.Fatal("protected session shed")
	}
}
func TestIdleActivityResetsRoundsAndTTLPreservesNewContext(t *testing.T) {
	p, root := idleFixture(t)
	now := time.Now()
	a := registerIdleProcess(t, p, root, "a", false, now)
	for i := 0; i < 2; i++ {
		_, e := p.Round(context.Background(), map[string]int{a: 1}, 1, 0, now)
		if e != nil {
			t.Fatal(e)
		}
	}
	h, e := p.Begin("a")
	if e != nil || !json.Valid(h) {
		t.Fatal(e)
	}
	if _, e = p.Begin("a"); e == nil {
		t.Fatal("double dispatch")
	}
	acts, e := p.Round(context.Background(), map[string]int{a: 1}, 1, 1, now.Add(time.Hour))
	if e != nil || len(acts) != 0 {
		t.Fatal("active retired", acts, e)
	}
	if e = p.Close(context.Background()); e == nil {
		t.Fatal("active close permitted")
	}
	if e = p.End("a", json.RawMessage(`{"task":"new"}`), now.Add(time.Hour)); e != nil {
		t.Fatal(e)
	}
	acts, e = p.Round(context.Background(), map[string]int{a: 1}, 1, 0, now.Add(time.Hour))
	if e != nil || len(acts) != 0 {
		t.Fatal("rounds not reset", acts, e)
	}
	acts, e = p.Round(context.Background(), map[string]int{a: 1}, 1, 0, now.Add(2*time.Hour))
	if e != nil || len(acts) != 1 || acts[0].Kind != "idle-session-retired" {
		t.Fatal(acts, e)
	}
	h, e = p.Handoff("a")
	if e != nil || string(h) != `{"task":"new"}` {
		t.Fatal(string(h), e)
	}
}
func TestIdleRejectsInvalidRegistrationAndHandoff(t *testing.T) {
	p, root := idleFixture(t)
	if e := p.Register(IdleRegistration{ID: "../escape"}, time.Now()); e == nil {
		t.Fatal("invalid registration")
	}
	if _, e := p.Handoff("../escape"); e == nil {
		t.Fatal("path escaped")
	}
	if e := os.Symlink("/etc/passwd", filepath.Join(p.root, "symlink.json")); e != nil {
		t.Fatal(e)
	}
	if _, e := p.Handoff("symlink"); e == nil {
		t.Fatal("symlink read")
	}
	if _, e := NewOwnedIdleSessions(filepath.Join(root, "handoffs"), "relative"); e == nil {
		t.Fatal("relative owner")
	}
	if _, e := p.Round(context.Background(), map[string]int{"foreign": -1}, 1, 0, time.Now()); e == nil {
		t.Fatal("negative census")
	}
}

// This exercises a real headless Herdr server and its separately registered pane,
// not a simulated pane counter. It uses the same public registration capability
// that a persistent trusted adapter invokes after collection.
func TestIdleNativeHerdrRetirement(t *testing.T) {
	binary, e := exec.LookPath("herdr")
	if e != nil {
		t.Skip("herdr unavailable")
	}
	p, root := idleFixture(t)
	socket := filepath.Join(root, "h.sock")
	config := filepath.Join(root, "h.toml")
	shell := filepath.Join(root, "shell.sh")
	if e = os.WriteFile(shell, []byte("#!/bin/sh\nexec /bin/sleep 300\n"), 0700); e != nil {
		t.Fatal(e)
	}
	raw := "onboarding=false\n[terminal]\ndefault_shell=" + quoteTOML(shell) + "\nshell_mode=\"non_login\"\n[update]\nversion_check=false\n"
	if e = os.WriteFile(config, []byte(raw), 0600); e != nil {
		t.Fatal(e)
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + root, "HERDR_CONFIG_PATH=" + config, "HERDR_SOCKET_PATH=" + socket}
	cmd := exec.Command(binary, "server")
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	server, e := processowner.StartLinked(cmd, filepath.Join(root, ".processes"), root, "herdr-idle")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = processowner.Terminate(ctx, server, root)
		_ = cmd.Wait()
		_ = processowner.Retire(server)
	})
	query := func(args ...string) ([]byte, error) {
		ctx, c := context.WithTimeout(context.Background(), time.Second)
		defer c()
		q := exec.CommandContext(ctx, binary, args...)
		q.Env = env
		return q.Output()
	}
	ready := false
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if _, e = query("workspace", "list"); e == nil {
			ready = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !ready {
		t.Fatal("Herdr startup timed out")
	}
	data, e := query("workspace", "create", "--cwd", root, "--label", "idle-test", "--no-focus")
	if e != nil {
		t.Fatal(e)
	}
	var created struct {
		Result struct {
			RootPane struct {
				ID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if e = json.Unmarshal(data, &created); e != nil || created.Result.RootPane.ID == "" {
		t.Fatal(string(data), e)
	}
	data, e = query("pane", "process-info", "--pane", created.Result.RootPane.ID)
	if e != nil {
		t.Fatal(e)
	}
	var info struct {
		Result struct {
			Info struct {
				PID int `json:"shell_pid"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if e = json.Unmarshal(data, &info); e != nil || info.Result.Info.PID < 1 {
		t.Fatal(string(data), e)
	}
	pane, e := processowner.RegisterLinked(info.Result.Info.PID, filepath.Join(root, ".processes"), root, "herdr-idle")
	if e != nil {
		t.Fatal(e)
	}
	if e = processowner.Watch(pane); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		_ = processowner.Terminate(ctx, pane, root)
		_ = processowner.Retire(pane)
	})
	e = p.Register(IdleRegistration{ID: "herdr-idle", Socket: socket, Role: "planner", Global: true, PreCode: true, Records: []*processowner.Record{pane, server}, Context: json.RawMessage(`{"summary":"native Herdr"}`), Reap: func() error {
		e := cmd.Wait()
		var ex *exec.ExitError
		if errors.As(e, &ex) {
			return nil
		}
		return e
	}}, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 3; i++ {
		acts, e := p.Round(context.Background(), map[string]int{socket: 1}, 1, 0, time.Now())
		if e != nil {
			t.Fatal(e)
		}
		if i == 2 && (len(acts) != 1 || acts[0].Kind != "idle-session-shed") {
			t.Fatal(acts)
		}
	}
	if !processowner.ProvedDead(pane.PID, pane.Birth) || !processowner.ProvedDead(server.PID, server.Birth) {
		t.Fatal("owned Herdr process survived shedding")
	}
	if h, e := p.Handoff("herdr-idle"); e != nil || string(h) != `{"summary":"native Herdr"}` {
		t.Fatal(string(h), e)
	}
}
func quoteTOML(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestIdleReservedRoleProtectionAndInFlightReset(t *testing.T) {
	p, root := idleFixture(t)
	now := time.Now()
	socket := registerIdleProcess(t, p, root, "protected", false, now)
	// Simulate a final-judgement adapter registration without the extra flag.
	p.mu.Lock()
	entry := p.sessions["protected"]
	registration := entry.IdleRegistration
	delete(p.sessions, "protected")
	p.mu.Unlock()
	registration.Role = "qa"
	registration.Protected = false
	if e := p.Register(registration, now); e != nil {
		t.Fatal(e)
	}
	for i := 0; i < 2; i++ {
		if a, e := p.Round(context.Background(), map[string]int{socket: 1}, 1, 0, now); e != nil || len(a) > 0 {
			t.Fatal(a, e)
		}
	}
	if a, e := p.Round(context.Background(), map[string]int{socket: 1}, 1, 1, now); e != nil || len(a) > 0 {
		t.Fatal(a, e)
	}
	for i := 0; i < 3; i++ {
		a, e := p.Round(context.Background(), map[string]int{socket: 1}, 1, 0, now)
		if e != nil {
			t.Fatal(e)
		}
		if i < 2 && len(a) > 0 {
			t.Fatal("round counter not reset")
		}
		if i == 2 && (len(a) != 1 || a[0].Kind != "PaneCapStarvation") {
			t.Fatal(a)
		}
	}
}
func TestIdleRejectsRecordMutationAndForeignSocket(t *testing.T) {
	p, root := idleFixture(t)
	now := time.Now()
	registerIdleProcess(t, p, root, "owned", false, now)
	p.mu.Lock()
	r := p.sessions["owned"].IdleRegistration
	delete(p.sessions, "owned")
	p.mu.Unlock()
	origSocket := r.Socket
	r.Socket = "/tmp/foreign-herdr.sock"
	if e := p.Register(r, now); e == nil {
		t.Fatal("foreign socket accepted")
	}
	r.Socket = origSocket
	original := r.Records[0]
	bad := *original
	bad.Birth = "forged"
	r.Records = []*processowner.Record{&bad}
	if e := p.Register(r, now); e == nil {
		t.Fatal("modified durable record accepted")
	}
	r.Records = []*processowner.Record{original}
	if e := p.Register(r, now); e != nil {
		t.Fatal(e)
	}
	original.Owner = "mutated by caller"
	p.mu.Lock()
	retained := p.sessions["owned"].Records[0].Owner
	p.mu.Unlock()
	if retained != root {
		t.Fatal("caller mutation changed owned record")
	}
}

func TestIdleRetirementFailureRetainsOwnershipAndContext(t *testing.T) {
	p, root := idleFixture(t)
	now := time.Now()
	socket := registerIdleProcess(t, p, root, "guarded", false, now)
	if _, e := p.Round(context.Background(), map[string]int{}, 1, 0, now); e == nil {
		t.Fatal("missing owned socket ignored")
	}
	p.mu.Lock()
	record := p.sessions["guarded"].Records[0]
	birth := record.Birth
	record.Birth = "unproven-reused-birth"
	p.mu.Unlock()
	for i := 0; i < 3; i++ {
		a, e := p.Round(context.Background(), map[string]int{socket: 1}, 1, 0, now)
		if i < 2 && e != nil {
			t.Fatal(e)
		}
		if i == 2 && (e == nil || len(a) != 0) {
			t.Fatal("unproved retirement released capacity", a, e)
		}
	}
	if p.Count() != 1 {
		t.Fatal("failed retirement lost ownership")
	}
	if _, e := p.Handoff("guarded"); e != nil {
		t.Fatal(e)
	}
	p.mu.Lock()
	record.Birth = birth
	p.mu.Unlock()
}
