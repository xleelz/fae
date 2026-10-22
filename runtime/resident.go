package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xleelz/fae/processowner"
)

// ResidentMultiplexer retains a real Herdr worker pane. Each request executes a
// fresh bridge in a separately registered process group; only the transport is
// resident. Reviews and fresh-context requests retain the one-shot transport.
type ResidentMultiplexer struct {
	Direct             *Direct
	Binary, Instance   string
	Idle               *OwnedIdleSessions
	BeforeStart        func(context.Context, string, string) error
	AfterStop          func(context.Context, string) error
	OneShotBeforeStart func(context.Context, string, string) error
	Classify           func(Launch) (global, preCode, protected bool)
	seen               map[string]bool
	runners            map[string]*Direct
	mu                 sync.Mutex
	entries            map[string]*residentEntry
	fallback           *Multiplexer
}
type residentAdmissionDeferred struct{ cause error }

func (e residentAdmissionDeferred) Error() string         { return e.cause.Error() }
func (e residentAdmissionDeferred) Unwrap() error         { return e.cause }
func (residentAdmissionDeferred) AdmissionDeferred() bool { return true }

type residentEntry struct {
	id, dir, socket, request string
	server                   *exec.Cmd
	records                  []*processowner.Record
	closed                   atomic.Bool
}
type residentRequest struct {
	Args, Env                        []string
	Directory, Brief, Output, Status string
}
type residentResponse struct {
	PID   int
	Error string
}

// This private worker executes multiple bridge commands in the same owned pane.
// A bridge remains stopped until the daemon has registered its exact identity.
func init() {
	if len(os.Args) != 3 || os.Args[1] != "--fae-internal-resident-worker" {
		return
	}
	listener, e := net.Listen("unix", os.Args[2])
	if e != nil {
		os.Exit(125)
	}
	_ = os.Chmod(os.Args[2], 0600)
	for {
		c, e := listener.Accept()
		if e != nil {
			os.Exit(125)
		}
		residentServe(c)
	}
}
func residentServe(c net.Conn) {
	defer c.Close()
	var r residentRequest
	if json.NewDecoder(c).Decode(&r) != nil || len(r.Args) == 0 {
		return
	}
	out, e := os.OpenFile(r.Output, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return
	}
	defer out.Close()
	in, e := os.Open(r.Brief)
	if e != nil {
		return
	}
	defer in.Close()
	script := "kill -STOP $$; \"$@\"; result=$?; printf '%s\\n' \"$result\" > " + quote(r.Status+".tmp") + "; mv -- " + quote(r.Status+".tmp") + " " + quote(r.Status) + "; kill -STOP $$"
	cmd := exec.Command("/bin/sh", append([]string{"-c", script, "fae-resident-turn"}, r.Args...)...)
	cmd.Dir = r.Directory
	cmd.Env = r.Env
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e = cmd.Start(); e != nil {
		_ = json.NewEncoder(c).Encode(residentResponse{Error: e.Error()})
		return
	}
	// The worker owns this unreaped child until the daemon proves retirement.
	defer func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); _ = cmd.Wait() }()
	if e = waitStopped(cmd.Process.Pid); e != nil {
		return
	}
	if json.NewEncoder(c).Encode(residentResponse{PID: cmd.Process.Pid}) != nil {
		return
	}
	var ack string
	_ = json.NewDecoder(c).Decode(&ack)
}
func waitStopped(pid int) error {
	end := time.Now().Add(3 * time.Second)
	for time.Now().Before(end) {
		b, e := exec.Command("/bin/ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		if e != nil {
			return e
		}
		if strings.Contains(string(b), "T") {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return errors.New("resident process gate timeout")
}
func residentKey(l Launch) string {
	// Include capabilities and route to avoid reusing across changed contracts.
	task := l.TaskID
	if l.SessionPolicy == "workflow-global" {
		task = ""
	}
	b, _ := json.Marshal(struct {
		Workflow, Revision, Task, Role, Mode string
		Profile                              any
		Binding                              any
	}{l.WorkflowID, l.WorkflowRevision, task, l.Role, l.Mode, l.Profile, l.Binding})
	h := sha256.Sum256(b)
	return "resident-" + hex.EncodeToString(h[:12])
}
func (m *ResidentMultiplexer) Sessions() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]int{}
	for _, d := range m.runners {
		for id, pid := range d.Sessions() {
			out[id] = pid
		}
	}
	return out
}
func (m *ResidentMultiplexer) Run(ctx context.Context, l Launch) (Collected, error) {
	m.mu.Lock()
	locked := true
	defer func() {
		if locked {
			m.mu.Unlock()
		}
	}()
	if m.seen == nil {
		m.seen = map[string]bool{}
		m.runners = map[string]*Direct{}
	}
	if m.seen[l.SessionID] {
		return Collected{}, errors.New("resident session identity already used")
	}
	m.seen[l.SessionID] = true
	if m.Direct == nil || m.Idle == nil {
		return Collected{}, errors.New("resident direct and idle registry required")
	}
	if l.FreshContext {
		// Separate Direct instances prevent transport hook replacement races.
		d := m.copyDirect()
		fallback := Multiplexer{Direct: d, Binary: m.Binary, Instance: m.Instance, BeforeStart: m.OneShotBeforeStart}
		m.runners[l.SessionID] = d
		defer func() { m.mu.Lock(); delete(m.runners, l.SessionID); m.mu.Unlock() }()
		locked = false
		m.mu.Unlock()
		return fallback.Run(ctx, l)
	}
	if m.entries == nil {
		m.entries = map[string]*residentEntry{}
	}
	id := residentKey(l)
	entry := m.entries[id]
	if entry != nil {
		live := true
		for _, record := range entry.records {
			birth, e := processowner.Birth(record.PID)
			live = live && e == nil && birth == record.Birth
		}
		if !entry.closed.Load() && !CanReuseResident(entry.id == id, live, l.FreshContext) {
			return Collected{NonExecution: true, ProcessesRetired: true}, &TurnError{Class: "startup-not-executed", Role: l.Role, DispatchID: l.DispatchID, Cause: errors.New("resident identity unavailable; retirement required")}
		}
		if _, e := m.Idle.Begin(id); e != nil {
			if !entry.closed.Load() {
				cause := e
				if errors.Is(e, ErrIdleSessionActive) {
					cause = residentAdmissionDeferred{cause: e}
				}
				return Collected{NonExecution: true, ProcessesRetired: true}, &TurnError{Class: "startup-not-executed", Role: l.Role, DispatchID: l.DispatchID, Cause: cause}
			}
			entry = nil
			delete(m.entries, id)
		}
	}
	if entry == nil {
		var e error
		entry, e = m.start(ctx, id, l)
		if e != nil {
			return Collected{NonExecution: true, ProcessesRetired: !errors.Is(e, processowner.ErrRetirementUnproven)}, e
		}
		m.entries[id] = entry
		if _, e = m.Idle.Begin(id); e != nil {
			return Collected{}, e
		}
	}
	d := m.copyDirect()
	d.prepareProcess = func(ctx context.Context, cmd *exec.Cmd, dir string) (*processHooks, error) {
		return m.prepareTurn(ctx, entry, cmd, dir, l)
	}
	m.runners[l.SessionID] = d
	defer func() { m.mu.Lock(); delete(m.runners, l.SessionID); m.mu.Unlock() }()
	locked = false
	m.mu.Unlock()
	result, e := d.Run(ctx, l)
	if !result.ProcessesRetired {
		return result, errors.Join(e, errors.New("resident turn retirement unproven; active reference retained"))
	}
	handoff := result.Output
	if !json.Valid(handoff) {
		handoff = json.RawMessage(`{}`)
	}
	e = errors.Join(e, m.Idle.End(id, handoff, time.Now()))
	return result, e
}
func (m *ResidentMultiplexer) copyDirect() *Direct {
	d := m.Direct
	return &Direct{Command: append([]string(nil), d.Command...), Root: d.Root, Environment: d.Environment, Timeout: d.Timeout, StartTimeout: d.StartTimeout, StallTimeout: d.StallTimeout, StallPollInterval: d.StallPollInterval, MaxHandoffBytes: d.MaxHandoffBytes, TailBytes: d.TailBytes}
}
func (m *ResidentMultiplexer) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Idle == nil {
		return nil
	}
	return m.Idle.Close(ctx)
}
func (m *ResidentMultiplexer) start(ctx context.Context, id string, l Launch) (_ *residentEntry, err error) {
	if !filepath.IsAbs(m.Binary) || m.Instance == "" {
		return nil, errors.New("absolute Herdr and instance required")
	}
	dir, e := os.MkdirTemp(m.Direct.Root, "resident-")
	if e != nil {
		return nil, e
	}
	r := &residentEntry{id: id, dir: dir, socket: filepath.Join(dir, "mux.sock"), request: filepath.Join(dir, "request.sock")}
	if len(r.request) > 100 {
		return nil, errors.New("resident socket path exceeds platform bound")
	}
	if e = writeResidentOwner(m.Direct.Root, r); e != nil {
		return nil, e
	}
	if m.BeforeStart != nil {
		if e = m.BeforeStart(ctx, id, r.socket); e != nil {
			return nil, e
		}
	}
	defer func() {
		if err != nil {
			if cleanupErr := m.stop(r); cleanupErr != nil {
				err = errors.Join(err, processowner.ErrRetirementUnproven, cleanupErr)
			}
		}
	}()
	exe, e := os.Executable()
	if e != nil {
		return nil, e
	}
	wrapper := filepath.Join(dir, "worker.sh")
	if e = os.WriteFile(wrapper, []byte("#!/bin/sh\nexec "+quote(exe)+" --fae-internal-resident-worker "+quote(r.request)+"\n"), 0700); e != nil {
		return nil, e
	}
	config := filepath.Join(dir, "herdr.toml")
	if e = os.WriteFile(config, []byte("onboarding=false\n[terminal]\ndefault_shell="+strconv.Quote(wrapper)+"\nshell_mode=\"non_login\"\n[update]\nversion_check=false\n"), 0600); e != nil {
		return nil, e
	}
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "HERDR_CONFIG_PATH=" + config, "HERDR_SOCKET_PATH=" + r.socket}
	cmd := exec.Command(m.Binary, "server")
	cmd.Env = env
	cmd.Dir = l.Worktree
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r.server = cmd
	record, e := processowner.StartLinked(cmd, filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, id)
	if e != nil {
		return nil, e
	}
	r.records = append(r.records, record)
	query := func(args ...string) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		c := exec.CommandContext(cctx, m.Binary, args...)
		c.Env = env
		return c.Output()
	}
	deadline := time.Now().Add(startupLimit(m.Direct.StartTimeout))
	for {
		if _, e = query("workspace", "list"); e == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, errors.New("resident Herdr startup timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, e := query("workspace", "create", "--cwd", l.Worktree, "--label", m.Instance, "--no-focus")
	if e != nil {
		return nil, e
	}
	var created struct {
		Result struct {
			RootPane struct {
				ID string `json:"pane_id"`
			} `json:"root_pane"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &created) != nil || created.Result.RootPane.ID == "" {
		return nil, errors.New("invalid resident pane")
	}
	raw, e = query("pane", "process-info", "--pane", created.Result.RootPane.ID)
	if e != nil {
		return nil, e
	}
	var info struct {
		Result struct {
			Info struct {
				PID int `json:"shell_pid"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &info) != nil || info.Result.Info.PID <= 1 {
		return nil, errors.New("invalid resident PID")
	}
	record, e = processowner.RegisterLinked(info.Result.Info.PID, filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, id)
	if e != nil {
		return nil, e
	}
	r.records = append(r.records, record)
	if e = processowner.Watch(record); e != nil {
		return nil, e
	}
	for {
		if _, e = os.Stat(r.request); e == nil {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, errors.New("resident worker startup timeout")
		}
		time.Sleep(time.Millisecond)
	}
	global, preCode, protected := false, false, false
	if m.Classify != nil {
		global, preCode, protected = m.Classify(l)
	}
	registration := IdleRegistration{Global: global, PreCode: preCode, Protected: protected, Expire: true, ID: id, Socket: r.socket, Role: l.Role, Records: r.records, Context: json.RawMessage(`{}`), Reap: func() error { return m.stop(r) }}
	if e = m.Idle.Register(registration, time.Now()); e != nil {
		return nil, e
	}
	return r, nil
}
func (m *ResidentMultiplexer) stop(r *residentEntry) error {
	if r.closed.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := len(r.records) - 1; i >= 0; i-- {
		if e := processowner.Terminate(ctx, r.records[i], m.Direct.Root); e != nil {
			return e
		}
	}
	if r.server != nil && r.server.Process != nil {
		_ = r.server.Wait()
	}
	for _, record := range r.records {
		if e := processowner.Retire(record); e != nil {
			return e
		}
	}
	_ = os.Remove(r.socket)
	_ = os.Remove(r.request)
	if m.AfterStop != nil {
		if e := m.AfterStop(ctx, r.id); e != nil {
			return e
		}
	}
	r.closed.Store(true)
	return nil
}
func (m *ResidentMultiplexer) prepareTurn(ctx context.Context, r *residentEntry, cmd *exec.Cmd, dir string, l Launch) (*processHooks, error) {
	audit, _ := json.Marshal(map[string]any{"resident_id": r.id, "server_pid": r.records[0].PID, "pane_pid": r.records[1].PID, "dispatch_id": l.DispatchID, "session_id": l.SessionID})
	if e := os.WriteFile(filepath.Join(dir, "resident-session.json"), audit, 0600); e != nil {
		return nil, e
	}
	c, e := net.DialTimeout("unix", r.request, time.Second)
	if e != nil {
		return nil, e
	}
	_ = c.SetDeadline(time.Now().Add(startupLimit(m.Direct.StartTimeout)))
	status := filepath.Join(dir, "resident.status")
	output := filepath.Join(dir, "resident.output")
	request := residentRequest{Args: cmd.Args, Env: cmd.Env, Directory: cmd.Dir, Brief: filepath.Join(dir, "brief.json"), Output: output, Status: status}
	if e = json.NewEncoder(c).Encode(request); e != nil {
		c.Close()
		return nil, e
	}
	var response residentResponse
	if e = json.NewDecoder(c).Decode(&response); e != nil {
		c.Close()
		return nil, e
	}
	if response.Error != "" || response.PID <= 1 {
		c.Close()
		return nil, fmt.Errorf("resident launch: %s", response.Error)
	}
	record, e := processowner.RegisterLinked(response.PID, filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, l.SessionID)
	if e != nil {
		birth, _ := processowner.Birth(response.PID)
		return failedResidentPrepare(cmd, c, response.PID, birth, nil, e), nil
	}
	if e = processowner.Watch(record); e != nil {
		return failedResidentPrepare(cmd, c, response.PID, record.Birth, record, e), nil
	}
	released := false
	cmd.Path = "/bin/sh"
	cmd.Args = []string{"sh", "-c", "while [ ! -f \"$1\" ]; do sleep 0.01; done; cat \"$2\"; exit \"$(cat \"$1\")\"", "fae-resident-client", status, output}
	var once sync.Once
	var closeErr error
	closeTurn := func() error {
		once.Do(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			closeErr = processowner.Terminate(cleanup, record, m.Direct.Root)
			if closeErr == nil {
				closeErr = processowner.Retire(record)
			}
			_ = json.NewEncoder(c).Encode("retired")
			_ = c.Close()
		})
		return closeErr
	}
	return &processHooks{NonExecution: func() bool { return !released }, ActivityPID: func() int { return response.PID }, Started: func() error { released = true; return syscall.Kill(response.PID, syscall.SIGCONT) }, Finished: func(e error) error { return errors.Join(e, closeTurn()) }, Close: closeTurn}, nil
}

// A registration failure cannot accidentally launch the original bridge through
// Direct's fallback command. Keep cleanup uncertainty visible to lease release.
func failedResidentPrepare(cmd *exec.Cmd, c net.Conn, pid int, birth string, record *processowner.Record, cause error) *processHooks {
	cmd.Path = "/bin/false"
	cmd.Args = []string{"false"}
	var once sync.Once
	var cleanupErr error
	cleanup := func() error {
		once.Do(func() {
			_ = c.Close()
			deadline := time.Now().Add(3 * time.Second)
			for !processowner.ProvedDead(pid, birth) {
				if time.Now().After(deadline) {
					cleanupErr = processowner.ErrRetirementUnproven
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			if record != nil {
				cleanupErr = processowner.Retire(record)
			}
		})
		return cleanupErr
	}
	return &processHooks{NonExecution: func() bool { return true }, Started: func() error { return cause }, Finished: func(e error) error { return errors.Join(e, cleanup()) }, Close: cleanup}
}
