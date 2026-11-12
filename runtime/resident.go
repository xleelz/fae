package runtime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xleelz/fae/processowner"
)

// ResidentSessionSpec carries optional Herdr terminal metadata for a resident session.
type ResidentSessionSpec struct {
	Labels map[string]string
}

// ResidentMultiplexer retains a real Herdr worker pane. Each request executes a
// fresh bridge in a separately registered process group; only the transport is
// resident. Reviews and fresh-context requests retain the one-shot transport.
type ResidentMultiplexer struct {
	Direct             *Direct
	Binary, Instance   string
	Labels             map[string]string
	SessionSpec        ResidentSessionSpec
	Idle               *OwnedIdleSessions
	BeforeStart        func(context.Context, string, string) error
	AfterStop          func(context.Context, string) error
	OneShotBeforeStart func(context.Context, string, string) error
	Classify           func(Launch) (global, preCode, protected bool)
	seen               map[string]bool
	runners            map[string]*Direct
	mu                 sync.Mutex
	entries            map[string]*residentEntry
	launching          map[string]*residentLaunch
	// busy names the identities a Run holds, from reservation until it
	// returns; a held identity is never given to another turn.
	busy     map[string]bool
	closed   bool
	fallback *Multiplexer

	// ReconciliationDeferred, when set, is told of a turn deferred because
	// the retirement of its slot's transport stayed unproven, with the
	// turn's launch context and the slot's identity, so that the caller can
	// withhold the turn's task for that cause (FAE-SCH-010).
	ReconciliationDeferred func(ctx context.Context, residentID string, cause error)
}

// residentLaunch reserves one resident identity while a Run prepares it
// without the multiplexer lock; Close cancels it and waits for done.
type residentLaunch struct {
	cancel context.CancelFunc
	done   chan struct{}
}

var errResidentClosed = errors.New("resident multiplexer closed")

type residentAdmissionDeferred struct{ cause error }

func (e residentAdmissionDeferred) Error() string         { return e.cause.Error() }
func (e residentAdmissionDeferred) Unwrap() error         { return e.cause }
func (residentAdmissionDeferred) AdmissionDeferred() bool { return true }

type residentEntry struct {
	id, dir, socket, request string
	token, pane              string
	// base is the turn contract the transport serves (residentKey) and slot
	// its index among that contract's concurrent identities.
	base    string
	slot    int
	resumed bool
	server  *exec.Cmd
	records []*processowner.Record
	holder  *processowner.Record
	closed  atomic.Bool
	// unretired marks a transport whose retirement was not proven, after its
	// turn or when a launch reconciled its slot: it is never reused, and its
	// slot is skipped, whether or not its processes are already stopped,
	// until the idle registry proves it retired (OnRetired clears the mark).
	// Only a provider loop, whose iterations keep one identity, retries it
	// at launch.
	unretired atomic.Bool
}
type residentRequest struct {
	Token                            string
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
	os.Exit(residentWorker(os.Args[2]))
}

// FAE-SEC-001: the request socket executes commands, so it is owner-only from
// creation and every request must carry the per-worker secret. The daemon
// delivers the secret in a private file beside the socket, never on argv; the
// worker consumes the file before listening.
func residentWorker(socket string) int {
	keyPath := filepath.Join(filepath.Dir(socket), "worker.key")
	key, e := readResidentKey(keyPath)
	if removeErr := os.Remove(keyPath); e == nil && removeErr != nil {
		e = removeErr
	}
	if e != nil {
		return 125
	}
	mask := syscall.Umask(0177)
	listener, e := net.Listen("unix", socket)
	syscall.Umask(mask)
	if e != nil {
		return 125
	}
	if info, e := os.Lstat(socket); e != nil || info.Mode().Perm() != 0600 {
		return 125
	}
	for {
		c, e := listener.Accept()
		if e != nil {
			return 125
		}
		residentServe(c, key)
	}
}
func writeResidentKey(dir string) (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	key := hex.EncodeToString(b)
	f, e := os.OpenFile(filepath.Join(dir, "worker.key"), os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return "", e
	}
	_, e = f.WriteString(key)
	return key, errors.Join(e, f.Close())
}
func readResidentKey(path string) ([]byte, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	key := make([]byte, 65)
	n, _ := f.Read(key)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || n != 64 {
		return nil, errors.New("invalid resident worker secret")
	}
	return key[:n], nil
}
func emitResidentOSC7501(state string) {
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		_, _ = fmt.Fprintf(os.Stdout, "\033]7501;state=%s;app=fae\033\\", state)
	}
}
func residentServe(c net.Conn, key []byte) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
	var r residentRequest
	if json.NewDecoder(c).Decode(&r) != nil || subtle.ConstantTimeCompare([]byte(r.Token), key) != 1 || len(r.Args) == 0 {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
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
	emitResidentOSC7501("working")
	if json.NewEncoder(c).Encode(residentResponse{PID: cmd.Process.Pid}) != nil {
		return
	}
	var ack string
	_ = json.NewDecoder(c).Decode(&ack)
	if raw, err := os.ReadFile(r.Status); err == nil {
		if strings.TrimSpace(string(raw)) == "0" {
			emitResidentOSC7501("done")
		} else {
			emitResidentOSC7501("blocked")
		}
	}
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
	if l.BranchInstance != "" {
		task += "/branch/" + l.BranchInstance
	}
	b, _ := json.Marshal(struct {
		Workflow, Revision, Task, Role, Mode string
		Profile                              any
		Binding                              any
	}{l.WorkflowID, l.WorkflowRevision, task, l.Role, l.Mode, l.Profile, l.Binding})
	h := sha256.Sum256(b)
	return "resident-" + hex.EncodeToString(h[:12])
}

// residentSlotID names slot n of a turn contract; slot 0 keeps the contract
// key itself, so its identity and recorded handoff are unchanged.
func residentSlotID(base string, slot int) string {
	if slot == 0 {
		return base
	}
	return base + "-" + strconv.Itoa(slot)
}

// reserveSlot picks and holds, under m.mu, the identity a turn runs on
// (FAE-SCH-005, -009). Concurrent turns of one contract, such as one
// workflow-global role's turns from different tasks, get distinct identities:
// the lowest-slot idle transport of that contract is reused, else the lowest
// free slot is started. The identities in use at once are as many as the
// contract's concurrent turns, which the role lease bounds by the role's cap.
// A slot whose retirement is unproven is skipped (FAE-RT-012), including one
// whose processes are stopped (closed) but whose registry retirement failed,
// so a later turn of the contract runs on another slot instead of deferring
// on it; maintenance retries it. A provider loop keeps one identity
// (FAE-RT-014): an overlapping iteration gets none and defers.
func (m *ResidentMultiplexer) reserveSlot(l Launch) (base, id string, slot int, ok bool) {
	base = residentKey(l)
	if loopResident(l) {
		if m.busy[base] {
			return base, "", 0, false
		}
		m.busy[base] = true
		return base, base, 0, true
	}
	slot = -1
	for key, e := range m.entries {
		if e.base == base && !m.busy[key] && !e.closed.Load() && !e.unretired.Load() && (slot < 0 || e.slot < slot) {
			id, slot = key, e.slot
		}
	}
	for n := 0; id == ""; n++ {
		candidate := residentSlotID(base, n)
		if e := m.entries[candidate]; !m.busy[candidate] && (e == nil || !e.unretired.Load()) {
			id, slot = candidate, n
		}
	}
	m.busy[id] = true
	return base, id, slot, true
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
func (m *ResidentMultiplexer) Live() []LiveSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []LiveSession
	for _, d := range m.runners {
		out = append(out, d.Live()...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (m *ResidentMultiplexer) Retire(ctx context.Context, session string) error {
	m.mu.Lock()
	d := m.runners[session]
	m.mu.Unlock()
	if d == nil {
		return ErrUnknownSession
	}
	return d.Retire(ctx, session)
}
func (m *ResidentMultiplexer) Run(ctx context.Context, l Launch) (Collected, error) {
	m.mu.Lock()
	if m.seen == nil {
		m.seen = map[string]bool{}
		m.runners = map[string]*Direct{}
	}
	if m.seen[l.SessionID] {
		m.mu.Unlock()
		return Collected{}, errors.New("resident session identity already used")
	}
	m.seen[l.SessionID] = true
	if m.Direct == nil || m.Idle == nil {
		m.mu.Unlock()
		return Collected{}, errors.New("resident direct and idle registry required")
	}
	if l.FreshContext {
		// Separate Direct instances prevent transport hook replacement races.
		d := m.copyDirect()
		customLabels := m.Labels
		if len(customLabels) == 0 && len(m.SessionSpec.Labels) > 0 {
			customLabels = m.SessionSpec.Labels
		}
		fallback := Multiplexer{Direct: d, Binary: m.Binary, Instance: m.Instance, Labels: customLabels, BeforeStart: m.OneShotBeforeStart}
		m.runners[l.SessionID] = d
		m.mu.Unlock()
		defer func() { m.mu.Lock(); delete(m.runners, l.SessionID); m.mu.Unlock() }()
		return fallback.Run(ctx, l)
	}
	if m.entries == nil {
		m.entries = map[string]*residentEntry{}
		m.launching = map[string]*residentLaunch{}
		m.busy = map[string]bool{}
	}
	// FAE-SCH-009: the lock only reserves and publishes an identity. Proving
	// its slot live, retiring a removed slot, the pane reservation's pressure
	// back-off and the Herdr startup all run without it, and concurrent turns
	// of one contract run on distinct identities (reserveSlot), so one task's
	// launch never delays another task's turn.
	if m.closed {
		m.mu.Unlock()
		return residentNotExecuted(l, errResidentClosed)
	}
	base, id, slot, ok := m.reserveSlot(l)
	if !ok {
		m.mu.Unlock()
		return residentNotExecuted(l, ErrIdleSessionActive)
	}
	defer func() { m.mu.Lock(); delete(m.busy, id); m.mu.Unlock() }()
	prepare, cancel := context.WithCancel(ctx)
	defer cancel()
	launch := &residentLaunch{cancel: cancel, done: make(chan struct{})}
	m.launching[id] = launch
	old := m.entries[id]
	m.mu.Unlock()
	entry, fresh, began, result, e := m.acquire(prepare, id, old, l)
	m.mu.Lock()
	if e == nil && m.closed {
		// Close began during the launch: retire the transport instead of
		// publishing it, before Close may proceed (no pane or server leaks).
		m.mu.Unlock()
		re := m.abandon(id, fresh, began)
		m.mu.Lock()
		result, e = residentNotExecuted(l, errors.Join(errResidentClosed, re))
		result.ProcessesRetired = re == nil
	} else if e == nil && fresh {
		entry.base, entry.slot = base, slot
		m.entries[id] = entry
		if began, e = m.Idle.Begin(id); e != nil {
			result = Collected{}
		}
	}
	delete(m.launching, id)
	close(launch.done)
	if e != nil {
		m.mu.Unlock()
		return result, e
	}
	// FAE-SCH-014: the first turn of a relaunched session resumes from the
	// handoff its retired predecessor recorded.
	if fresh && entry.resumed {
		l.Brief = resumeBrief(l.Brief, began)
	}
	d := m.copyDirect()
	d.prepareProcess = func(ctx context.Context, cmd *exec.Cmd, dir string) (*processHooks, error) {
		return m.prepareTurn(ctx, entry, cmd, dir, l)
	}
	m.runners[l.SessionID] = d
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.runners, l.SessionID); m.mu.Unlock() }()
	result, e = d.Run(ctx, l)
	return m.collect(id, entry, l, result, e)
}

// collect ends a resident turn so its identity is never left active: a
// persistent transport records the handoff and returns idle; a task-bound one
// retires with its collected turn (FAE-RT-011). A transport whose turn
// retirement is unproven, or whose handoff cannot be recorded, is no longer
// trusted: the turn ends and the transport is retired through the retirement
// path, or, if that is unproven too, refused for reuse until a maintenance
// round proves it retired (FAE-RT-012; a provider loop's next iteration also
// retries it). A later turn runs on another slot meanwhile, and the identity
// can be relaunched once its retirement is proven.
func (m *ResidentMultiplexer) collect(id string, entry *residentEntry, l Launch, result Collected, e error) (Collected, error) {
	cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	retire := m.Idle.Abandon
	if !persistentResident(l) {
		retire = m.Idle.Release
	}
	if !result.ProcessesRetired {
		re := retire(cleanup, id)
		if re != nil {
			entry.unretired.Store(true)
		}
		return result, errors.Join(e, errors.New("resident turn retirement unproven; transport refused for reuse and retired"), re)
	}
	if !persistentResident(l) {
		if re := retire(cleanup, id); re != nil {
			entry.unretired.Store(true)
			result.ProcessesRetired = false
			return result, errors.Join(e, re)
		}
		m.mu.Lock()
		if m.entries[id] == entry {
			delete(m.entries, id)
		}
		m.mu.Unlock()
		return result, e
	}
	handoff := result.Output
	if !json.Valid(handoff) {
		handoff = json.RawMessage(`{}`)
	}
	if ee := m.Idle.End(id, handoff, time.Now()); ee != nil {
		re := retire(cleanup, id)
		if re != nil {
			entry.unretired.Store(true)
		}
		return result, errors.Join(e, ee, re)
	}
	return result, e
}

func residentNotExecuted(l Launch, cause error) (Collected, error) {
	if _, deferred := cause.(residentAdmissionDeferred); !deferred && errors.Is(cause, ErrIdleSessionActive) {
		cause = residentAdmissionDeferred{cause: cause}
	}
	return Collected{NonExecution: true, ProcessesRetired: true}, &TurnError{Class: "startup-not-executed", Role: l.Role, DispatchID: l.DispatchID, Cause: cause}
}

// acquire prepares the reserved identity without the multiplexer lock: a live
// idle slot is begun; a slot whose pane was removed, or whose retirement is
// pending, is first reconciled through the retirement path; a missing one is
// started. fresh reports a started transport its caller has not yet begun.
func (m *ResidentMultiplexer) acquire(ctx context.Context, id string, old *residentEntry, l Launch) (entry *residentEntry, fresh bool, began json.RawMessage, result Collected, err error) {
	if old != nil {
		if !old.closed.Load() && CanReuseResident(old.id == id, residentLive(old.records), l.FreshContext) {
			began, err = m.Idle.Begin(id)
			if err == nil {
				return old, false, began, Collected{}, nil
			}
			if errors.Is(err, ErrIdleSessionActive) {
				result, err = residentNotExecuted(l, err)
				return nil, false, nil, result, err
			}
		}
		// FAE-RT-012: a slot whose pane was removed, or whose retirement is
		// pending, is reconciled through the retirement path, which first
		// releases its reference, and then relaunched. A slot owning an
		// in-flight turn is never closed. A reconciliation left unproven sets
		// the slot aside like any unretired transport: reserveSlot skips it
		// and maintenance retries it. The turn launched nothing and the
		// failure is local, so it defers without consuming a startup retry
		// or charging the provider binding; its next attempt takes another
		// slot (a loop's next iteration retries this one).
		if err = m.Idle.Retire(ctx, id); err != nil {
			cause := errors.Join(errors.New("resident identity unavailable; retirement required"), err)
			if !errors.Is(err, ErrIdleSessionActive) {
				old.unretired.Store(true)
				if m.ReconciliationDeferred != nil {
					m.ReconciliationDeferred(ctx, id, cause)
				}
			}
			result, err = residentNotExecuted(l, residentAdmissionDeferred{cause: cause})
			return nil, false, nil, result, err
		}
		m.mu.Lock()
		if m.entries[id] == old {
			delete(m.entries, id)
		}
		m.mu.Unlock()
	}
	if entry, err = m.start(ctx, id, l); err != nil {
		return nil, false, nil, Collected{NonExecution: true, ProcessesRetired: !errors.Is(err, processowner.ErrRetirementUnproven)}, err
	}
	return entry, true, nil, Collected{}, nil
}

// abandon retires a transport prepared for a launch that Close overtook. A
// reused slot is first returned idle with its unchanged context.
func (m *ResidentMultiplexer) abandon(id string, fresh bool, began json.RawMessage) error {
	cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !fresh {
		if e := m.Idle.End(id, began, time.Now()); e != nil {
			return e
		}
	}
	return m.Idle.Retire(cleanup, id)
}

// persistentResident reports whether a transport outlives its turn: long-lived
// workflow-global sessions (FAE-SCH-014) and provider loop sessions, which
// re-prompt one persistent session (FAE-RT-014). Task-bound sessions retire
// when their turn is collected (FAE-RT-011).
func persistentResident(l Launch) bool {
	return l.SessionPolicy == "workflow-global" || loopResident(l)
}

// loopResident reports a provider loop session, which has one identity.
func loopResident(l Launch) bool { return strings.HasPrefix(l.WorkflowID, "fae-loop:") }

// residentLive requires each registered leader's exact identity and, when the
// census is readable, a live member of its group (a defunct server is gone).
func residentLive(records []*processowner.Record) bool {
	groups, census := liveGroups()
	for _, record := range records {
		birth, e := processowner.Birth(record.PID)
		if e != nil || birth != record.Birth || census == nil && !groups[record.PGID] {
			return false
		}
	}
	return true
}

// resumeBrief delivers a recorded session handoff as a non-authoritative block
// ahead of a final kernel contract. A brief that is not a block list is kept.
func resumeBrief(brief, handoff json.RawMessage) json.RawMessage {
	var blocks []json.RawMessage
	if json.Unmarshal(brief, &blocks) != nil || !json.Valid(handoff) {
		return brief
	}
	block, _ := json.Marshal(map[string]any{"kind": "session_handoff", "authority": "none", "note": "context recorded by this role's previous session before idle retirement; not evidence and not current task scope", "handoff": handoff})
	at := len(blocks)
	var last struct{ Kind string }
	if at > 0 && json.Unmarshal(blocks[at-1], &last) == nil && last.Kind == "kernel_contract" {
		at--
	}
	blocks = append(blocks[:at], append([]json.RawMessage{block}, blocks[at:]...)...)
	out, e := json.Marshal(blocks)
	if e != nil {
		return brief
	}
	return out
}
func (m *ResidentMultiplexer) copyDirect() *Direct {
	d := m.Direct
	return &Direct{Command: append([]string(nil), d.Command...), Root: d.Root, Environment: d.Environment, Timeout: d.Timeout, StartTimeout: d.StartTimeout, StallTimeout: d.StallTimeout, StallPollInterval: d.StallPollInterval, MaxHandoffBytes: d.MaxHandoffBytes, TailBytes: d.TailBytes, Shims: d.Shims}
}

// Close refuses later resident launches, cancels and waits for those still
// being prepared (each retires what it started rather than publishing it), and
// then retires every idle transport.
func (m *ResidentMultiplexer) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	pending := make([]*residentLaunch, 0, len(m.launching))
	for _, launch := range m.launching {
		launch.cancel()
		pending = append(pending, launch)
	}
	m.mu.Unlock()
	for _, launch := range pending {
		select {
		case <-launch.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
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
	if r.token, e = writeResidentKey(dir); e != nil {
		return nil, e
	}
	if m.BeforeStart != nil {
		if e = m.BeforeStart(ctx, id, r.socket); e != nil {
			// FAE-RT-004: the pane reservation is local admission. Whatever
			// stopped it, a refusal or a failed pressure, census or ledger
			// step, no agent was launched and no provider was involved, so
			// the turn defers as infrastructure: never a startup retry, whose
			// exhaustion is charged to the provider binding.
			return nil, residentAdmissionDeferred{cause: e}
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
	_, extraEnv, e := writeHerdrCoreConfig(dir, r.socket, m.Instance, wrapper)
	if e != nil {
		return nil, e
	}
	env := append([]string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}, extraEnv...)
	cmd := exec.Command(m.Binary, "serve")
	cmd.Env = env
	cmd.Dir = l.Worktree
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	r.server = cmd
	record, e := processowner.StartRole(cmd, filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, id, "multiplexer-server")
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
		if socketReady(r.socket) {
			if raw, err := query("ls", "--json"); err == nil {
				count, err := herdrTerminalCount(raw)
				if err != nil || count != 0 {
					return nil, errors.New("resident multiplexer: startup found foreign panes")
				}
				break
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, errors.New("resident Herdr startup timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	paneName := herdrTerminalName(m.Instance)
	customLabels := m.Labels
	if len(customLabels) == 0 && len(m.SessionSpec.Labels) > 0 {
		customLabels = m.SessionSpec.Labels
	}
	runArgs := []string{"run", "--bg", "--no-parent", "--no-notify", "--name", paneName, "--cwd", l.Worktree}
	for _, kv := range buildHerdrLabels(m.Instance, "resident-worker", &l, customLabels) {
		runArgs = append(runArgs, "--label", kv)
	}
	runArgs = append(runArgs, "--json")
	raw, e := query(runArgs...)
	if e != nil {
		return nil, e
	}
	paneID, _, e := parseHerdrCreatedTerminal(raw)
	if e != nil || paneID == "" {
		return nil, errors.New("invalid resident pane")
	}
	r.pane = paneID
	raw, e = query("get", paneID, "--json")
	if e != nil {
		return nil, e
	}
	proc, e := parseHerdrProcessInfo(raw, paneID)
	if e != nil || proc.PID <= 1 {
		return nil, errors.New("invalid resident PID")
	}
	record, e = processowner.RegisterRole(proc.PID, filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, id, "resident-worker")
	if e != nil {
		return nil, e
	}
	r.records = append(r.records, record)
	if e = processowner.Watch(record); e != nil {
		return nil, e
	}
	r.holder = registerHerdrHolder(proc.PID, r.records[0].PID, dir)
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
	// FAE-SCH-014: a relaunch registers the context its retired predecessor
	// recorded instead of overwriting it; task-bound sessions keep none.
	saved := json.RawMessage(`{}`)
	if persistentResident(l) {
		if prior, e := m.Idle.Handoff(id); e == nil && string(prior) != "{}" {
			saved, r.resumed = prior, true
		}
	}
	registration := IdleRegistration{Global: global, PreCode: preCode, Protected: protected, Expire: true, ID: id, Socket: r.socket, Role: l.Role, Records: r.records, Context: saved, Reap: func() error { return m.stop(r) }, OnRetired: func() { r.unretired.Store(false) }}
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
	if r.holder != nil {
		_ = processowner.Terminate(ctx, r.holder, r.dir)
		_ = processowner.Retire(r.holder)
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
	_ = os.Remove(filepath.Join(r.dir, "worker.key"))
	if m.AfterStop != nil {
		if e := m.AfterStop(ctx, r.id); e != nil {
			return e
		}
	}
	r.closed.Store(true)
	return nil
}
func (m *ResidentMultiplexer) prepareTurn(ctx context.Context, r *residentEntry, cmd *exec.Cmd, dir string, l Launch) (*processHooks, error) {
	audit, _ := json.Marshal(map[string]any{"resident_id": r.id, "server_pid": r.records[0].PID, "pane_pid": r.records[1].PID, "dispatch_id": l.DispatchID, "session_id": l.SessionID, "task_bound": !persistentResident(l)})
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
	request := residentRequest{Token: r.token, Args: cmd.Args, Env: cmd.Env, Directory: cmd.Dir, Brief: filepath.Join(dir, "brief.json"), Output: output, Status: status}
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
	record, e := processowner.RegisterRole(response.PID, filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, l.SessionID, "agent-session")
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
