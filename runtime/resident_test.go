package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/processowner"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func residentFixture(t *testing.T, mode string) (*ResidentMultiplexer, Launch) {
	t.Helper()
	mux, l := muxFixture(t, mode)
	idle, e := NewOwnedIdleSessions(filepath.Join(mux.Direct.Root, "idle"), mux.Direct.Root)
	if e != nil {
		t.Fatal(e)
	}
	m := &ResidentMultiplexer{Direct: mux.Direct, Binary: mux.Binary, Instance: mux.Instance, Idle: idle}
	// Standalone fixture simulates the caller's durable resident-lease checkpoint.
	birth, e := processowner.Birth(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	m.AfterStop = func(_ context.Context, id string) error {
		return processowner.AcknowledgeLinkedRetirement(filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, id, os.Getpid(), birth)
	}
	t.Cleanup(func() {
		if e := m.Close(context.Background()); e != nil {
			t.Error(e)
		}
	})
	return m, l
}
func TestResidentTwoTurnsSamePaneFreshConfig(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	starts, stops := 0, 0
	m.BeforeStart = func(context.Context, string, string) error { starts++; return nil }
	ackResident := m.AfterStop
	m.AfterStop = func(ctx context.Context, id string) error { stops++; return ackResident(ctx, id) }
	first, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	if !first.ProcessesRetired {
		t.Fatal("turn process not retired")
	}
	ackResidentTurn(t, m, l.SessionID)
	entry := m.entries[residentKey(l)]
	pid := entry.records[1].PID
	l.SessionID += "-second"
	l.DispatchID += "-second"
	l.Brief = []byte(`{"value":"new brief"}`)
	second, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	if !second.ProcessesRetired || first.LogDirectory == second.LogDirectory || m.entries[residentKey(l)].records[1].PID != pid || starts != 1 {
		t.Fatal("resident/config identity mismatch")
	}
	ackResidentTurn(t, m, l.SessionID)
	records, _ := filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.json"))
	if len(records) != 2 {
		t.Fatalf("expected only resident records, got %v", records)
	}
	if _, e = m.Run(context.Background(), l); e == nil {
		t.Fatal("repeated identity accepted")
	}
	if e = m.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	if stops != 1 {
		t.Fatal(stops)
	}
	records, _ = filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.json*"))
	if len(records) != 0 {
		t.Fatal(records)
	}
}

// Retained task-bound transports (provider loops) stay isolated per task and
// expire at the idle bound.
func TestResidentTaskIsolationAndExpiry(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy, l.WorkflowID = "task-bound", "fae-loop:demo"
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	first := m.entries[residentKey(l)]
	l.TaskID += "-other"
	l.DispatchID += "-other"
	l.SessionID += "-other"
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	if len(m.entries) != 2 || m.entries[residentKey(l)].records[1].PID == first.records[1].PID {
		t.Fatal("task isolation lost")
	}
	m.Idle.TTL = time.Nanosecond
	counts := map[string]int{}
	for _, entry := range m.entries {
		counts[entry.socket] = 1
	}
	actions, e := m.Idle.Round(context.Background(), counts, 24, 0, time.Now().Add(time.Second))
	if e != nil || len(actions) != 1 {
		t.Fatalf("%v %v", actions, e)
	}
	for _, entry := range m.entries {
		if entry.closed.Load() {
			if _, e = os.Stat(entry.socket); !os.IsNotExist(e) {
				t.Fatal("expired socket retained")
			}
		}
	}
}
func TestResidentInterruptedTurnRetiresOnlyTurn(t *testing.T) {
	m, l := residentFixture(t, "wait")
	l.SessionPolicy = "workflow-global"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		for {
			records, _ := filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.json"))
			if len(records) >= 4 {
				time.Sleep(100 * time.Millisecond)
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
	out, e := m.Run(ctx, l)
	if e == nil {
		t.Fatal("timeout succeeded")
	}
	if !out.ProcessesRetired {
		t.Fatalf("turn retirement unproven: %v", e)
	}
	if m.Idle.Count() != 1 {
		t.Fatal("resident lost")
	}
}
func TestCanReuseResident(t *testing.T) {
	for _, identity := range []bool{false, true} {
		for _, live := range []bool{false, true} {
			for _, fresh := range []bool{false, true} {
				if got := CanReuseResident(identity, live, fresh); got != (identity && live && !fresh) {
					t.Fatal(identity, live, fresh)
				}
			}
		}
	}
}
func TestResidentFreshContextUsesOneShot(t *testing.T) {
	m, l := residentFixture(t, "")
	l.FreshContext = true
	oneshot := 0
	m.OneShotBeforeStart = func(context.Context, string, string) error { oneshot++; return nil }
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	if oneshot != 1 || m.Idle.Count() != 0 || len(m.entries) != 0 {
		t.Fatal("fresh context pooled")
	}
}
func TestResidentMissingBridgeRetainsSafeIdleWorker(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	m.Direct.Command = []string{"/nonexistent/fae-bridge", "{config}"}
	result, e := m.Run(context.Background(), l)
	if e == nil || !result.NonExecution || !result.ProcessesRetired {
		t.Fatalf("%+v %v", result, e)
	}
	if m.Idle.Count() != 1 {
		t.Fatal("resident not retained")
	}
}
func TestResidentStartupFailureReleasesReservation(t *testing.T) {
	m, l := residentFixture(t, "")
	m.Binary = "/nonexistent/herdr"
	m.Direct.StartTimeout = 100 * time.Millisecond
	starts, stops := 0, 0
	m.BeforeStart = func(context.Context, string, string) error { starts++; return nil }
	m.AfterStop = func(context.Context, string) error { stops++; return nil }
	if _, e := m.Run(context.Background(), l); e == nil {
		t.Fatal("missing executable succeeded")
	}
	if starts != 1 || stops != 1 || m.Idle.Count() != 0 {
		t.Fatalf("startup lease leaked %d %d", starts, stops)
	}
}
func TestResidentGlobalReuseAcrossTasks(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	id := residentKey(l)
	l.TaskID += "-next"
	l.SessionID += "-next"
	l.DispatchID += "-next"
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	if residentKey(l) != id || len(m.entries) != 1 {
		t.Fatal("workflow-global transport not shared")
	}
}
func TestResidentDistinctTasksRunConcurrently(t *testing.T) {
	m, l := residentFixture(t, "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 2)
	go func() { _, e := m.Run(ctx, l); done <- e }()
	other := l
	other.TaskID += "-two"
	other.SessionID += "-two"
	other.DispatchID += "-two"
	go func() { _, e := m.Run(ctx, other); done <- e }()
	for len(m.Sessions()) < 2 {
		select {
		case <-ctx.Done():
			t.Fatal("distinct resident turns serialized")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	for i := 0; i < 2; i++ {
		if e := <-done; e == nil {
			t.Fatal("cancelled turn succeeded")
		}
	}
}

// A provider loop re-prompts one persistent session (FAE-RT-014), so an
// overlapping iteration of the same loop defers instead of taking a slot.
func TestResidentOverlappingSameKeyRefused(t *testing.T) {
	m, l := residentFixture(t, "wait")
	l.SessionPolicy, l.WorkflowID = "task-bound", "fae-loop:demo"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := m.Run(ctx, l); done <- e }()
	for len(m.Sessions()) < 1 {
		select {
		case <-ctx.Done():
			t.Fatal("turn failed to start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	second := l
	second.SessionID += "-overlap"
	second.DispatchID += "-overlap"
	result, e := m.Run(ctx, second)
	if e == nil || !result.NonExecution || !result.ProcessesRetired {
		t.Fatalf("overlap accepted or retirement wrong: %+v %v", result, e)
	}
	var deferred interface{ AdmissionDeferred() bool }
	if !errors.As(e, &deferred) || !deferred.AdmissionDeferred() {
		t.Fatalf("active overlap should defer admission: %v", e)
	}
	cancel()
	<-done
	if len(m.entries) != 1 {
		t.Fatal("overlap replaced resident")
	}
}

// A dead resident identity is never reused: the next turn runs on a relaunched
// transport (FAE-RT-012) and leaves no phantom dispatch behind.
func TestResidentDeadIdentityDoesNotLeakNewTurn(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	entry := m.entries[residentKey(l)]
	if e := processowner.Terminate(context.Background(), entry.records[1], m.Direct.Root); e != nil {
		t.Fatal(e)
	}
	l.SessionID += "-next"
	l.DispatchID += "-next"
	result, e := m.Run(context.Background(), l)
	if e != nil || !result.ProcessesRetired || !entry.closed.Load() || m.entries[residentKey(l)] == entry {
		t.Fatalf("%+v %v", result, e)
	}
	if len(m.Sessions()) != 0 {
		t.Fatal("phantom dispatch retained")
	}
}

// The transport deliberately retains terminal receipts until its caller has
// durably journaled retirement. Simulate that acknowledgement in runtime tests.
func ackResidentTurn(t *testing.T, m *ResidentMultiplexer, id string) {
	t.Helper()
	birth, e := processowner.Birth(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	records, e := filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.retired"))
	if e != nil || len(records) < 2 {
		t.Fatalf("turn retirement receipts missing: %v %v", records, e)
	}
	if e = processowner.AcknowledgeLinkedRetirement(filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, id, os.Getpid(), birth); e != nil {
		t.Fatal(e)
	}
}

// FAE-SEC-001: the worker's request socket is owner-only from creation and
// executes nothing without the per-worker secret, which is delivered through a
// consumed private file rather than argv.
func TestResidentWorkerRefusesUnauthenticatedRequest(t *testing.T) {
	dir, e := os.MkdirTemp("/var/tmp", "fae-rw-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	token, e := writeResidentKey(dir)
	if e != nil {
		t.Fatal(e)
	}
	socket := filepath.Join(dir, "request.sock")
	exe, _ := os.Executable()
	worker := exec.Command(exe, "--fae-internal-resident-worker", socket)
	if e = worker.Start(); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = worker.Process.Kill(); _ = worker.Wait() })
	for i := 0; ; i++ {
		if _, e = os.Lstat(socket); e == nil {
			break
		}
		if i > 3000 {
			t.Fatal("worker startup")
		}
		time.Sleep(time.Millisecond)
	}
	if info, e := os.Lstat(socket); e != nil || info.Mode().Perm() != 0600 {
		t.Fatal("request socket permissions", info, e)
	}
	if _, e = os.Lstat(filepath.Join(dir, "worker.key")); !os.IsNotExist(e) {
		t.Fatal("worker secret not consumed", e)
	}
	if args, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", worker.Process.Pid)); strings.Contains(string(args), token) {
		t.Fatal("worker secret on argv")
	}
	brief := filepath.Join(dir, "brief.json")
	if e = os.WriteFile(brief, []byte("{}"), 0600); e != nil {
		t.Fatal(e)
	}
	marker := filepath.Join(dir, "executed")
	request := func(token string) (residentResponse, net.Conn, error) {
		c, e := net.Dial("unix", socket)
		if e != nil {
			return residentResponse{}, nil, e
		}
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
		r := residentRequest{Token: token, Args: []string{"/bin/sh", "-c", "touch " + marker}, Directory: dir, Brief: brief, Output: filepath.Join(dir, "out"), Status: filepath.Join(dir, "status")}
		if e = json.NewEncoder(c).Encode(r); e != nil {
			c.Close()
			return residentResponse{}, nil, e
		}
		var response residentResponse
		e = json.NewDecoder(c).Decode(&response)
		return response, c, e
	}
	for _, forged := range []string{"", strings.Repeat("0", len(token)), token[:len(token)-1]} {
		response, c, e := request(forged)
		if c != nil {
			c.Close()
		}
		if e == nil || response.PID != 0 {
			t.Fatalf("unauthenticated request accepted: %+v %v", response, e)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if _, e = os.Lstat(marker); !os.IsNotExist(e) {
		t.Fatal("unauthenticated request executed", e)
	}
	response, c, e := request(token)
	if e != nil || response.PID <= 1 {
		t.Fatal("authenticated request refused", response, e)
	}
	// Killing the worker skips its own reaping, so the stopped turn's group is
	// ended here; otherwise it outlives the test (FAE-VER-006).
	t.Cleanup(func() { _ = syscall.Kill(-response.PID, syscall.SIGKILL) })
	defer c.Close()
	if e = syscall.Kill(response.PID, syscall.SIGCONT); e != nil {
		t.Fatal(e)
	}
	for i := 0; ; i++ {
		if _, e = os.Lstat(filepath.Join(dir, "status")); e == nil {
			break
		}
		if i > 5000 {
			t.Fatal("authenticated request did not execute")
		}
		time.Sleep(time.Millisecond)
	}
	if _, e = os.Lstat(marker); e != nil {
		t.Fatal(e)
	}
	_ = json.NewEncoder(c).Encode("retired")
}

// residentPIDs reads the server and pane PIDs a turn's identity audit names.
func residentPIDs(t *testing.T, out Collected) (server, pane int) {
	t.Helper()
	raw, e := os.ReadFile(filepath.Join(out.LogDirectory, "resident-session.json"))
	if e != nil {
		t.Fatal(e)
	}
	var audit struct {
		Server int `json:"server_pid"`
		Pane   int `json:"pane_pid"`
	}
	if e = json.Unmarshal(raw, &audit); e != nil || audit.Server <= 1 || audit.Pane <= 1 {
		t.Fatal(string(raw), e)
	}
	return audit.Server, audit.Pane
}
func defunctChildren(t *testing.T) []processowner.Process {
	t.Helper()
	all, e := processowner.Census()
	if e != nil {
		t.Fatal(e)
	}
	var out []processowner.Process
	for _, p := range all {
		if p.PPID == os.Getpid() && p.Defunct() {
			out = append(out, p)
		}
	}
	return out
}
func ackTurn(t *testing.T, m *ResidentMultiplexer, session string) {
	t.Helper()
	birth, e := processowner.Birth(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	if e = processowner.AcknowledgeLinkedRetirement(filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, session, os.Getpid(), birth); e != nil {
		t.Fatal(e)
	}
}

// FAE-RT-011 acceptance: three task-bound turns finish and are collected; each
// task-bound transport (Herdr server, worker pane and turn group) is retired
// with its collection, leaving no task-bound process and no defunct child.
// residentProcessGone waits up to 5 s for pid to exit. A zombie counts as
// gone: it no longer runs, and under load its parent may reap it a moment
// after retirement is proven. It returns the last probe's evidence otherwise.
func residentProcessGone(pid int) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		e := syscall.Kill(pid, 0)
		if e == syscall.ESRCH {
			return nil
		}
		stat, readErr := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:])); readErr == nil && len(fields) > 0 && fields[0] == "Z" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("still running (kill 0: %v)", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestResidentTaskBoundRetiresAtCollection(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "task-bound"
	stops := 0
	ack := m.AfterStop
	m.AfterStop = func(ctx context.Context, id string) error { stops++; return ack(ctx, id) }
	var pids []int
	for i, task := range []string{"task-one", "task-two", "task-three"} {
		l.TaskID, l.SessionID, l.DispatchID = task, "session-"+task, "dispatch-"+task
		out, e := m.Run(context.Background(), l)
		if e != nil || !out.ProcessesRetired {
			t.Fatalf("turn %d: %+v %v", i, out, e)
		}
		ackTurn(t, m, l.SessionID)
		server, pane := residentPIDs(t, out)
		pids = append(pids, server, pane)
		if m.Idle.Count() != 0 || len(m.entries) != 0 || stops != i+1 {
			t.Fatalf("task-bound transport outlived collection: idle=%d entries=%d stops=%d", m.Idle.Count(), len(m.entries), stops)
		}
	}
	for _, pid := range pids {
		if e := residentProcessGone(pid); e != nil {
			t.Fatalf("task-bound process %d remains: %v", pid, e)
		}
	}
	if records, _ := filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.json*")); len(records) != 0 {
		t.Fatal("task-bound process records remain", records)
	}
	if contexts, _ := filepath.Glob(filepath.Join(m.Idle.root, "*.json")); len(contexts) != 0 {
		t.Fatal("task-bound context retained", contexts)
	}
	if z := defunctChildren(t); len(z) != 0 {
		t.Fatal("defunct children remain", z)
	}
}

// Provider loop sessions re-prompt one persistent session (FAE-RT-014), so
// their task-bound transport is retained across iterations.
func TestResidentLoopTransportRetained(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy, l.WorkflowID = "task-bound", "fae-loop:demo"
	first, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	l.SessionID, l.DispatchID = l.SessionID+"-2", l.DispatchID+"-2"
	second, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := residentPIDs(t, first)
	b, _ := residentPIDs(t, second)
	if a != b || m.Idle.Count() != 1 {
		t.Fatal("loop transport not retained")
	}
}

// closePane removes a resident's pane through its own Herdr server, as an
// operator's manual cleanup would, and waits for the worker to exit.
func closePane(t *testing.T, m *ResidentMultiplexer, entry *residentEntry) {
	t.Helper()
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + entry.dir,
		"TMPDIR=" + entry.dir,
		"HC_SOCKET=" + entry.socket,
		"HC_SESSION=" + herdrTerminalName(m.Instance),
		"XDG_CONFIG_HOME=" + filepath.Join(entry.dir, "cfg"),
		"XDG_RUNTIME_DIR=" + filepath.Join(entry.dir, "run"),
		"XDG_STATE_HOME=" + filepath.Join(entry.dir, "state"),
		"XDG_DATA_HOME=" + filepath.Join(entry.dir, "data"),
		"HERDR_CONFIG_PATH=" + filepath.Join(entry.dir, "cfg", "herdr-core", "config.toml"),
		"HERDR_SOCKET_PATH=" + entry.socket,
	}
	cmd := exec.Command(m.Binary, "kill", entry.pane, "--json")
	cmd.Env = env
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatal(string(out), e)
	}
	for deadline := time.Now().Add(10 * time.Second); !processowner.ProvedDead(entry.records[1].PID, entry.records[1].Birth); {
		if all, e := processowner.Census(); e == nil {
			gone := true
			for _, p := range all {
				if p.PGID == entry.records[1].PGID && !p.Defunct() {
					gone = false
				}
			}
			if gone {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("pane worker survived close")
		}
		time.Sleep(10 * time.Millisecond)
	}
	rmCmd := exec.Command(m.Binary, "rm", entry.pane, "--json")
	rmCmd.Env = env
	_ = rmCmd.Run()
}

// FAE-RT-012 acceptance: a pane removed out from under a live (idle) slot is
// reconciled through the retirement path, which releases the slot's reference
// and pane reservation, and the next turn relaunches instead of failing.
func TestResidentPaneRemovedUnderSlotRelaunches(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	starts, stops := 0, 0
	m.BeforeStart = func(context.Context, string, string) error { starts++; return nil }
	ack := m.AfterStop
	m.AfterStop = func(ctx context.Context, id string) error { stops++; return ack(ctx, id) }
	first, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, l.SessionID)
	old := m.entries[residentKey(l)]
	closePane(t, m, old)
	l.SessionID, l.DispatchID = l.SessionID+"-2", l.DispatchID+"-2"
	second, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatalf("removed pane not relaunched: %v", e)
	}
	ackTurn(t, m, l.SessionID)
	oldServer, oldPane := residentPIDs(t, first)
	newServer, newPane := residentPIDs(t, second)
	if newServer == oldServer || newPane == oldPane || starts != 2 || stops != 1 || !old.closed.Load() || m.Idle.Count() != 1 {
		t.Fatalf("slot not reconciled: starts=%d stops=%d closed=%v", starts, stops, old.closed.Load())
	}
	if e = residentProcessGone(oldServer); e != nil {
		t.Fatal("orphaned Herdr server survived reconciliation", e)
	}
}

// The maintenance round reconciles an idle slot whose pane was removed (its
// socket reports no pane) instead of failing closed on every later round.
func TestIdleRoundReconcilesRemovedPane(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, l.SessionID)
	entry := m.entries[residentKey(l)]
	closePane(t, m, entry)
	actions, e := m.Idle.Round(context.Background(), map[string]int{entry.socket: 0}, 60, 0, time.Now())
	if e != nil || len(actions) != 1 || actions[0].Kind != "idle-session-reconciled" || !entry.closed.Load() || m.Idle.Count() != 0 {
		t.Fatalf("%+v %v", actions, e)
	}
	if actions, e = m.Idle.Round(context.Background(), map[string]int{}, 60, 0, time.Now()); e != nil || len(actions) != 0 {
		t.Fatal(actions, e)
	}
	l.SessionID, l.DispatchID = l.SessionID+"-2", l.DispatchID+"-2"
	if _, e = m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
}

// FAE-SCH-014 acceptance: an idle global session retired after the bound keeps
// its handoff, and its next turn resumes from it: the relaunch registers the
// recorded context (never an empty one) and delivers it ahead of the kernel
// contract. A turn on the still-live session receives no resumed block.
func TestResidentRelaunchResumesRecordedHandoff(t *testing.T) {
	m, l := residentFixture(t, "echo-brief")
	l.SessionPolicy = "workflow-global"
	brief := func(n int) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`[{"kind":"stage","n":%d},{"kind":"kernel_contract"}]`, n))
	}
	l.Brief = brief(1)
	first, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, l.SessionID)
	entry := m.entries[residentKey(l)]
	m.Idle.TTL = time.Nanosecond
	actions, e := m.Idle.Round(context.Background(), map[string]int{entry.socket: 1}, 60, 0, time.Now().Add(time.Second))
	if e != nil || len(actions) != 1 || actions[0].Kind != "idle-session-retired" {
		t.Fatal(actions, e)
	}
	m.Idle.TTL = 0
	l.SessionID, l.DispatchID, l.Brief = l.SessionID+"-2", l.DispatchID+"-2", brief(2)
	second, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, l.SessionID)
	var got struct{ Brief []map[string]json.RawMessage }
	if e = json.Unmarshal(second.Output, &got); e != nil || len(got.Brief) != 3 {
		t.Fatalf("relaunched turn brief: %s %v", second.Output, e)
	}
	if string(got.Brief[1]["kind"]) != `"session_handoff"` || string(got.Brief[2]["kind"]) != `"kernel_contract"` || string(got.Brief[1]["handoff"]) != string(first.Output) {
		t.Fatalf("recorded handoff not resumed: %s", second.Output)
	}
	if h, e := m.Idle.Handoff(residentKey(l)); e != nil || string(h) != string(second.Output) {
		t.Fatal("handoff file not the latest context", string(h), e)
	}
	l.SessionID, l.DispatchID, l.Brief = l.SessionID+"-3", l.DispatchID+"-3", brief(3)
	third, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, l.SessionID)
	if strings.Contains(string(third.Output), "session_handoff") {
		t.Fatal("live session re-delivered its handoff", string(third.Output))
	}
}

// FAE-SCH-009: one task's slow resident launch (its pane reservation's pressure
// back-off, its Herdr startup) never delays another task's resident turn.
func TestResidentSlowLaunchDoesNotSerialiseOtherTasks(t *testing.T) {
	m, a := residentFixture(t, "")
	a.SessionPolicy = "task-bound"
	b := a
	b.TaskID, b.SessionID, b.DispatchID = a.TaskID+"-b", a.SessionID+"-b", a.DispatchID+"-b"
	slow, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	m.BeforeStart = func(ctx context.Context, id, _ string) error {
		if id == residentKey(a) {
			close(slow)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	turn := func(l Launch) chan error {
		done := make(chan error, 1)
		go func() {
			out, e := m.Run(context.Background(), l)
			if e == nil && !out.ProcessesRetired {
				e = errors.New("turn retirement unproven")
			}
			done <- e
		}()
		return done
	}
	doneA := turn(a)
	<-slow
	doneB := turn(b)
	select {
	case e := <-doneB:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(20 * time.Second):
		unblock()
		<-doneB
		<-doneA
		t.Fatal("task B's resident turn waited for task A's launch")
	}
	select {
	case e := <-doneA:
		t.Fatal("task A finished before its launch was released", e)
	default:
	}
	unblock()
	if e := <-doneA; e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, a.SessionID)
	ackTurn(t, m, b.SessionID)
	if m.Idle.Count() != 0 || len(m.entries) != 0 {
		t.Fatalf("task-bound transports outlived collection: idle=%d entries=%d", m.Idle.Count(), len(m.entries))
	}
}

// Concurrent Runs never start one identity twice. Concurrent turns of one
// workflow-global contract take distinct identities (slots), so a second
// task's Run neither waits for nor defers behind the first one's launch, and
// each identity starts exactly once; a later turn reuses an idle slot. A
// provider loop has one identity: its overlapping Run defers its admission.
func TestResidentConcurrentSameIdentityStartsOnce(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	slow, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	// Registered after the fixture, so a failure releases the blocked launch
	// before Close waits for it.
	t.Cleanup(unblock)
	var mu sync.Mutex
	starts := map[string]int{}
	m.BeforeStart = func(ctx context.Context, id, _ string) error {
		mu.Lock()
		starts[id]++
		first := len(starts) == 1 && starts[id] == 1
		mu.Unlock()
		if first {
			close(slow)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	done := make(chan error, 1)
	go func() { _, e := m.Run(context.Background(), l); done <- e }()
	<-slow
	second := l
	second.TaskID, second.SessionID, second.DispatchID = l.TaskID+"-2", l.SessionID+"-2", l.DispatchID+"-2"
	overlap := make(chan error, 1)
	go func() { _, e := m.Run(context.Background(), second); overlap <- e }()
	select {
	case e := <-overlap:
		if e != nil {
			t.Fatalf("second task's turn of the shared role not run: %v", e)
		}
	case <-time.After(20 * time.Second):
		unblock()
		<-overlap
		<-done
		t.Fatal("second task's turn waited for the first task's launch")
	}
	unblock()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, l.SessionID)
	ackTurn(t, m, second.SessionID)
	mu.Lock()
	if len(starts) != 2 || starts[residentSlotID(residentKey(l), 0)] != 1 || starts[residentSlotID(residentKey(l), 1)] != 1 {
		t.Fatalf("one identity started twice or slots not distinct: %v", starts)
	}
	mu.Unlock()
	if m.Idle.Count() != 2 || len(m.entries) != 2 {
		t.Fatalf("idle=%d entries=%d", m.Idle.Count(), len(m.entries))
	}
	third := l
	third.TaskID, third.SessionID, third.DispatchID = l.TaskID+"-3", l.SessionID+"-3", l.DispatchID+"-3"
	if _, e := m.Run(context.Background(), third); e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, third.SessionID)
	mu.Lock()
	defer mu.Unlock()
	if len(starts) != 2 || m.Idle.Count() != 2 {
		t.Fatalf("idle slot not reused: starts=%v idle=%d", starts, m.Idle.Count())
	}

	loop, loopLaunch := residentFixture(t, "")
	loopLaunch.SessionPolicy, loopLaunch.WorkflowID = "task-bound", "fae-loop:demo"
	loopSlow, loopRelease := make(chan struct{}), make(chan struct{})
	var loopOnce sync.Once
	loopUnblock := func() { loopOnce.Do(func() { close(loopRelease) }) }
	t.Cleanup(loopUnblock)
	var loopStarts atomic.Int32
	loop.BeforeStart = func(context.Context, string, string) error {
		if loopStarts.Add(1) == 1 {
			close(loopSlow)
			<-loopRelease
		}
		return nil
	}
	loopDone := make(chan error, 1)
	go func() { _, e := loop.Run(context.Background(), loopLaunch); loopDone <- e }()
	<-loopSlow
	again := loopLaunch
	again.SessionID, again.DispatchID = loopLaunch.SessionID+"-2", loopLaunch.DispatchID+"-2"
	out, e := loop.Run(context.Background(), again)
	var deferred interface{ AdmissionDeferred() bool }
	if e == nil || !out.NonExecution || !out.ProcessesRetired || !errors.As(e, &deferred) {
		loopUnblock()
		<-loopDone
		t.Fatalf("overlapping loop iteration not deferred: %+v %v", out, e)
	}
	loopUnblock()
	if e = <-loopDone; e != nil {
		t.Fatal(e)
	}
	if loopStarts.Load() != 1 || loop.Idle.Count() != 1 {
		t.Fatalf("loop identity started %d times", loopStarts.Load())
	}
}

// FAE-SCH-005, -009: with resident sessions on, two tasks' turns of one
// workflow-global role run at once, each on its own resident identity, and
// then both slots stay idle for reuse; neither is deferred behind the other.
func TestResidentSharedRoleTurnsRunConcurrently(t *testing.T) {
	m, l := residentFixture(t, "wait")
	l.SessionPolicy = "workflow-global"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type outcome struct {
		out Collected
		e   error
	}
	done := make(chan outcome, 2)
	other := l
	other.TaskID, other.SessionID, other.DispatchID = l.TaskID+"-two", l.SessionID+"-two", l.DispatchID+"-two"
	for _, launch := range []Launch{l, other} {
		go func() { out, e := m.Run(ctx, launch); done <- outcome{out, e} }()
	}
	for len(m.Sessions()) < 2 {
		select {
		case r := <-done:
			t.Fatalf("a shared-role turn ended before both ran: %+v %v", r.out, r.e)
		case <-ctx.Done():
			t.Fatal("shared-role resident turns serialised")
		case <-time.After(10 * time.Millisecond):
		}
	}
	m.mu.Lock()
	ids := map[string]bool{}
	for id := range m.busy {
		ids[id] = true
	}
	m.mu.Unlock()
	if len(ids) != 2 || !ids[residentSlotID(residentKey(l), 0)] || !ids[residentSlotID(residentKey(l), 1)] {
		t.Fatalf("shared-role turns not on distinct resident identities: %v", ids)
	}
	cancel()
	for i := 0; i < 2; i++ {
		if r := <-done; r.e == nil || !r.out.ProcessesRetired {
			t.Fatalf("cancelled turn: %+v %v", r.out, r.e)
		}
	}
	if m.Idle.Count() != 2 {
		t.Fatal("a shared-role resident did not stay idle for reuse", m.Idle.Count())
	}
}

// FAE-RT-011, -012: a resident turn whose retirement is unproven never pins its
// identity. The turn ends and its transport is refused for reuse; a later turn
// of the same role runs at once on another slot, maintenance retries the
// unproven retirement, and the role keeps running on its idle slot.
func TestResidentUnprovenTurnRetirementFreesIdentity(t *testing.T) {
	m, l := residentFixture(t, "wait")
	l.SessionPolicy = "workflow-global"
	registry := filepath.Join(m.Direct.Root, ".processes")
	t.Cleanup(func() { _ = os.Chmod(registry, 0700) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	type outcome struct {
		out Collected
		e   error
	}
	done := make(chan outcome, 1)
	go func() { out, e := m.Run(ctx, l); done <- outcome{out, e} }()
	for {
		// Server, pane, turn and client records: the turn is running.
		if records, _ := filepath.Glob(filepath.Join(registry, "*.json")); len(records) >= 4 {
			break
		}
		select {
		case r := <-done:
			t.Fatalf("turn ended early: %+v %v", r.out, r.e)
		case <-ctx.Done():
			t.Fatal("turn never started")
		case <-time.After(5 * time.Millisecond):
		}
	}
	// A registry that cannot record retirement receipts leaves the turn's
	// retirement, and the transport's, unproven.
	if e := os.Chmod(registry, 0500); e != nil {
		t.Fatal(e)
	}
	cancel()
	var first outcome
	select {
	case first = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("unproven turn retirement never returned")
	}
	if first.e == nil || first.out.ProcessesRetired {
		t.Fatalf("turn retirement unexpectedly proven: %+v %v", first.out, first.e)
	}
	slot0 := residentSlotID(residentKey(l), 0)
	if _, e := m.Idle.Begin(slot0); !errors.Is(e, ErrIdleSessionRetiring) {
		t.Fatalf("unproven resident left %v, want refused for reuse pending retirement", e)
	}
	if n := m.Idle.Unretired(); n != 1 {
		t.Fatal("unretired transports reported", n)
	}
	if e := os.Chmod(registry, 0700); e != nil {
		t.Fatal(e)
	}
	m.Direct.Environment["BRIDGE_MODE"] = ""
	next := l
	next.TaskID, next.SessionID, next.DispatchID = l.TaskID+"-next", l.SessionID+"-next", l.DispatchID+"-next"
	second, e := m.Run(context.Background(), next)
	if e != nil || !second.ProcessesRetired {
		t.Fatalf("later turn of the role did not run: %+v %v", second, e)
	}
	ackTurn(t, m, next.SessionID)
	m.mu.Lock()
	unproven, replacement := m.entries[slot0], m.entries[residentSlotID(residentKey(l), 1)]
	m.mu.Unlock()
	if unproven == nil || replacement == nil || unproven.closed.Load() || !unproven.unretired.Load() {
		t.Fatal("later turn did not run on a replacement slot while the unproven one awaits retirement")
	}
	counts := map[string]int{unproven.socket: 1, replacement.socket: 1}
	actions, e := m.Idle.Round(context.Background(), counts, 60, 0, time.Now())
	if e != nil || len(actions) != 1 || actions[0].Kind != "idle-session-reconciled" || actions[0].Session != slot0 || !unproven.closed.Load() || m.Idle.Count() != 1 || m.Idle.Unretired() != 0 {
		t.Fatalf("maintenance did not retry the unproven retirement: %+v %v", actions, e)
	}
	if e = residentProcessGone(unproven.records[0].PID); e != nil {
		t.Fatal("unproven transport's Herdr server survived its retried retirement", e)
	}
	third := l
	third.TaskID, third.SessionID, third.DispatchID = l.TaskID+"-third", l.SessionID+"-third", l.DispatchID+"-third"
	out, e := m.Run(context.Background(), third)
	if e != nil || !out.ProcessesRetired {
		t.Fatalf("role not usable after maintenance: %+v %v", out, e)
	}
	ackTurn(t, m, third.SessionID)
	secondServer, _ := residentPIDs(t, second)
	if thirdServer, _ := residentPIDs(t, out); thirdServer != secondServer {
		t.Fatal("idle replacement slot not reused")
	}
}

// FAE-RT-011, -012, FAE-RT-004: a launch whose reconciliation of a removed
// slot stays unproven (here its pane-ledger release fails) sets the slot aside
// like any unretired transport. The turn launched nothing and the failure is
// local, so it is an admission deferral, which consumes no startup retry and
// charges no provider binding. The role's next turn runs at once on another
// slot rather than failing on the same one, and maintenance retries the
// set-aside slot until its retirement is proven.
func TestResidentFailedSlotReconciliationSetsSlotAside(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	slot0 := residentSlotID(residentKey(l), 0)
	var failing atomic.Bool
	ack := m.AfterStop
	m.AfterStop = func(ctx context.Context, id string) error {
		if failing.Load() && id == slot0 {
			return errors.New("pane ledger release failed")
		}
		return ack(ctx, id)
	}
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, l.SessionID)
	m.mu.Lock()
	old := m.entries[slot0]
	m.mu.Unlock()
	closePane(t, m, old)
	failing.Store(true)
	second := l
	second.TaskID, second.SessionID, second.DispatchID = l.TaskID+"-2", l.SessionID+"-2", l.DispatchID+"-2"
	out, e := m.Run(context.Background(), second)
	var turn *TurnError
	var deferral interface{ AdmissionDeferred() bool }
	if e == nil || !errors.As(e, &turn) || turn.Class != "startup-not-executed" || !out.NonExecution || !out.ProcessesRetired {
		t.Fatalf("failed slot reconciliation: %+v %v", out, e)
	}
	if !errors.As(e, &deferral) || !deferral.AdmissionDeferred() {
		t.Fatalf("a local slot retirement failure would consume startup retries and charge the provider: %v", e)
	}
	if !old.unretired.Load() || old.closed.Load() || m.Idle.Unretired() != 1 {
		t.Fatalf("slot not set aside: unretired=%v closed=%v count=%d", old.unretired.Load(), old.closed.Load(), m.Idle.Unretired())
	}
	if _, e = m.Idle.Begin(slot0); !errors.Is(e, ErrIdleSessionRetiring) {
		t.Fatalf("set-aside slot left %v, want refused for reuse pending retirement", e)
	}
	third := l
	third.TaskID, third.SessionID, third.DispatchID = l.TaskID+"-3", l.SessionID+"-3", l.DispatchID+"-3"
	if out, e = m.Run(context.Background(), third); e != nil || !out.ProcessesRetired {
		t.Fatalf("the role's next turn failed on the set-aside slot: %+v %v", out, e)
	}
	ackTurn(t, m, third.SessionID)
	m.mu.Lock()
	replacement := m.entries[residentSlotID(residentKey(l), 1)]
	m.mu.Unlock()
	if replacement == nil || m.entries[slot0] != old {
		t.Fatal("the next turn did not run on another slot while the set-aside one awaits retirement")
	}
	// Still unproven: the round reports it and keeps it set aside.
	counts := map[string]int{old.socket: 0, replacement.socket: 1}
	if actions, e := m.Idle.Round(context.Background(), counts, 60, 0, time.Now()); e == nil || len(actions) != 0 || m.Idle.Unretired() != 1 {
		t.Fatalf("unproven retry: %+v %v", actions, e)
	}
	failing.Store(false)
	actions, e := m.Idle.Round(context.Background(), counts, 60, 0, time.Now())
	if e != nil || len(actions) != 1 || actions[0].Kind != "idle-session-reconciled" || actions[0].Session != slot0 || !old.closed.Load() || m.Idle.Count() != 1 || m.Idle.Unretired() != 0 {
		t.Fatalf("maintenance did not retry the set-aside slot: %+v %v", actions, e)
	}
	if e = residentProcessGone(old.records[0].PID); e != nil {
		t.Fatal("set-aside slot's Herdr server survived its retried retirement", e)
	}
}

// Close racing a launch whose Herdr server and pane are already up retires that
// transport instead of publishing it, so neither pane nor server outlives
// Close, and later launches are refused.
func TestResidentCloseDuringLaunchRetiresTransport(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	var stops atomic.Int32
	ack := m.AfterStop
	m.AfterStop = func(ctx context.Context, id string) error { stops.Add(1); return ack(ctx, id) }
	slow, release := make(chan struct{}), make(chan struct{})
	m.Classify = func(Launch) (bool, bool, bool) {
		close(slow)
		<-release
		return true, false, false
	}
	type outcome struct {
		out Collected
		e   error
	}
	done := make(chan outcome, 1)
	go func() { out, e := m.Run(context.Background(), l); done <- outcome{out, e} }()
	<-slow
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		closed <- m.Close(ctx)
	}()
	select {
	case e := <-closed:
		t.Fatal("Close returned while a launch it must retire was in progress", e)
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	if e := <-closed; e != nil {
		t.Fatal(e)
	}
	r := <-done
	if r.e == nil || !r.out.NonExecution || !r.out.ProcessesRetired {
		t.Fatalf("launch raced by Close: %+v %v", r.out, r.e)
	}
	if m.Idle.Count() != 0 || len(m.entries) != 0 || stops.Load() != 1 {
		t.Fatalf("transport leaked past Close: idle=%d entries=%d stops=%d", m.Idle.Count(), len(m.entries), stops.Load())
	}
	if records, _ := filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.json*")); len(records) != 0 {
		t.Fatal("transport process records remain", records)
	}
	if z := defunctChildren(t); len(z) != 0 {
		t.Fatal("defunct children remain", z)
	}
	l.SessionID, l.DispatchID = l.SessionID+"-late", l.DispatchID+"-late"
	if out, e := m.Run(context.Background(), l); e == nil || !out.NonExecution || !out.ProcessesRetired {
		t.Fatalf("launch after Close: %+v %v", out, e)
	}
}

// residentLaunchKey marks a test's launch context, so a hook can show it was
// given the turn's own context.
type residentLaunchKey struct{}

// FAE-RT-012, FAE-SCH-010: a slot whose processes were stopped (closed) but
// whose registry retirement then failed is unretired like any other. Later
// turns of its contract skip it and run on another slot instead of deferring
// on it again; maintenance retries it, and once its retirement is proven the
// slot is usable again. The one turn that met the failure is told to the
// caller with its launch context, which withholds its task by that cause.
func TestResidentClosedUnretiredSlotSkipped(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	slot0, slot1 := residentSlotID(residentKey(l), 0), residentSlotID(residentKey(l), 1)
	var planting atomic.Bool
	var old *residentEntry
	receipt := ""
	ack := m.AfterStop
	m.AfterStop = func(ctx context.Context, id string) error {
		e := ack(ctx, id)
		if planting.Load() && id == slot0 && e == nil {
			// After the processes stop and the lease is acknowledged, the
			// registry's own retirement step fails: the server record's
			// retirement receipt names another process.
			planted := *old.records[0]
			planted.PID, planted.PGID, planted.Birth = 999999, 999999, "planted"
			raw, err := json.Marshal(planted)
			if err == nil {
				err = os.WriteFile(receipt, raw, 0600)
			}
			return err
		}
		return e
	}
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, l.SessionID)
	m.mu.Lock()
	old = m.entries[slot0]
	m.mu.Unlock()
	receipt = old.records[0].Path + ".retired"
	planting.Store(true)
	closePane(t, m, old)
	type deferral struct {
		id, launch string
		cause      error
	}
	var told []deferral
	m.ReconciliationDeferred = func(ctx context.Context, id string, cause error) {
		launch, _ := ctx.Value(residentLaunchKey{}).(string)
		told = append(told, deferral{id, launch, cause})
	}
	turn := func(name string) (Launch, Collected, error) {
		next := l
		next.TaskID, next.SessionID, next.DispatchID = l.TaskID+"-"+name, l.SessionID+"-"+name, l.DispatchID+"-"+name
		out, e := m.Run(context.WithValue(context.Background(), residentLaunchKey{}, name), next)
		return next, out, e
	}
	// The turn that reconciles the removed slot meets the failure: it stops
	// the slot's processes but cannot prove its retirement, and defers.
	_, out, e := turn("second")
	var deferred interface{ AdmissionDeferred() bool }
	if e == nil || !out.NonExecution || !out.ProcessesRetired || !errors.As(e, &deferred) || !deferred.AdmissionDeferred() {
		t.Fatalf("failed reconciliation: %+v %v", out, e)
	}
	if !old.closed.Load() || !old.unretired.Load() || m.Idle.Unretired() != 1 {
		t.Fatalf("fixture: closed=%v unretired=%v count=%d", old.closed.Load(), old.unretired.Load(), m.Idle.Unretired())
	}
	if len(told) != 1 || told[0].id != slot0 || told[0].launch != "second" || told[0].cause == nil {
		t.Fatalf("reconciliation deferral not told with the turn's context: %+v", told)
	}
	// Later turns of the contract skip the closed, unretired slot.
	for _, name := range []string{"third", "fourth"} {
		next, out, e := turn(name)
		if e != nil || !out.ProcessesRetired {
			t.Fatalf("%s turn deferred on the closed unretired slot instead of using another: %+v %v", name, out, e)
		}
		ackTurn(t, m, next.SessionID)
	}
	m.mu.Lock()
	replacement, kept := m.entries[slot1], m.entries[slot0]
	m.mu.Unlock()
	if replacement == nil || kept != old || len(told) != 1 {
		t.Fatalf("later turns did not run on another slot: slot1=%v slot0 kept=%v told=%d", replacement != nil, kept == old, len(told))
	}
	// Maintenance retries it: still unproven while the receipt is unreadable.
	counts := map[string]int{old.socket: 0, replacement.socket: 1}
	if actions, e := m.Idle.Round(context.Background(), counts, 60, 0, time.Now()); e == nil || len(actions) != 0 || m.Idle.Unretired() != 1 || !old.unretired.Load() {
		t.Fatalf("unproven retry: %+v %v", actions, e)
	}
	planting.Store(false)
	if e = os.Remove(receipt); e != nil {
		t.Fatal(e)
	}
	actions, e := m.Idle.Round(context.Background(), counts, 60, 0, time.Now())
	if e != nil || len(actions) != 1 || actions[0].Kind != "idle-session-reconciled" || actions[0].Session != slot0 || m.Idle.Unretired() != 0 {
		t.Fatalf("maintenance did not retire the closed slot: %+v %v", actions, e)
	}
	if old.unretired.Load() {
		t.Fatal("a slot proven retired stays set aside")
	}
	// Proven retired, slot 0 is usable again: with slot 1 taken, the next
	// turn relaunches it rather than starting a third slot.
	m.mu.Lock()
	m.busy[slot1] = true
	m.mu.Unlock()
	next, out, e := turn("fifth")
	m.mu.Lock()
	delete(m.busy, slot1)
	relaunched := m.entries[slot0]
	_, third := m.entries[residentSlotID(residentKey(l), 2)]
	m.mu.Unlock()
	if e != nil || !out.ProcessesRetired {
		t.Fatalf("turn after maintenance: %+v %v", out, e)
	}
	ackTurn(t, m, next.SessionID)
	if relaunched == nil || relaunched == old || third {
		t.Fatalf("retired slot 0 not relaunched: relaunched=%v third slot=%v", relaunched != nil && relaunched != old, third)
	}
}

// FAE-RT-004: a resident pane reservation that fails without a refusal (a
// pressure, census or ledger step) is local admission, not a failed agent
// start. The turn launched nothing, so it defers as infrastructure: it is
// never a startup retry, three of which would park the task and record the
// provider binding unavailable.
func TestResidentPaneReservationErrorDefers(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	cause := errors.New("pane census unreadable")
	reservations := 0
	m.BeforeStart = func(context.Context, string, string) error { reservations++; return cause }
	out, e := m.Run(context.Background(), l)
	var deferred interface{ AdmissionDeferred() bool }
	if e == nil || !errors.Is(e, cause) || !out.NonExecution || !out.ProcessesRetired || reservations != 1 {
		t.Fatalf("failed reservation: %+v %v", out, e)
	}
	if !errors.As(e, &deferred) || !deferred.AdmissionDeferred() {
		t.Fatalf("a local pane-reservation failure would be a startup retry charged to the provider: %v", e)
	}
	if m.Idle.Count() != 0 || len(m.Sessions()) != 0 {
		t.Fatal("failed reservation left a session")
	}
	if records, _ := filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.json*")); len(records) != 0 {
		t.Fatal("failed reservation left process records", records)
	}
}

func TestResidentLabelsAppliedToRealHerdrTerminal(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	l.TaskID = "task-res-99"
	l.Stage = "review"
	m.SessionSpec = ResidentSessionSpec{
		Labels: map[string]string{
			"fae.molecule": "mol-res-1",
			"herdr.tab":    "mol-res-1",
		},
	}
	out, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	ackTurn(t, m, l.SessionID)
	if strings.Contains(out.Tail, "7501") {
		t.Fatalf("OSC 7501 leaked into resident tail: %q", out.Tail)
	}
	entry := m.entries[residentKey(l)]
	if entry == nil {
		t.Fatal("missing resident entry")
	}
	env := []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + entry.dir,
		"TMPDIR=" + entry.dir,
		"HC_SOCKET=" + entry.socket,
		"HC_SESSION=" + herdrTerminalName(m.Instance),
		"XDG_CONFIG_HOME=" + filepath.Join(entry.dir, "cfg"),
		"XDG_RUNTIME_DIR=" + filepath.Join(entry.dir, "run"),
		"XDG_STATE_HOME=" + filepath.Join(entry.dir, "state"),
		"XDG_DATA_HOME=" + filepath.Join(entry.dir, "data"),
		"HERDR_CONFIG_PATH=" + filepath.Join(entry.dir, "cfg", "herdr-core", "config.toml"),
		"HERDR_SOCKET_PATH=" + entry.socket,
	}
	cmd := exec.Command(m.Binary, "get", entry.pane, "--json")
	cmd.Env = env
	raw, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("herdr get failed: %s %v", raw, e)
	}
	var info struct {
		Labels map[string]string `json:"labels"`
	}
	if e := json.Unmarshal(raw, &info); e != nil {
		t.Fatalf("unmarshal herdr get: %s %v", raw, e)
	}
	if info.Labels["fae.instance"] != m.Instance || info.Labels["fae.molecule"] != "mol-res-1" || info.Labels["fae.role"] != "resident-worker" {
		t.Fatalf("unexpected herdr labels: %+v", info.Labels)
	}
}
