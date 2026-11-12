package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

// matrixHold stops one task at one durable boundary of the default pipeline:
// K checkpoints committed, then either its Block-th turn stays in flight
// (mid-stage) or its Fail-th checkpoint keeps failing (between stages).
type matrixHold struct {
	Task     string `json:"task"`
	K        int    `json:"k"`
	Block    int    `json:"block,omitempty"`
	Fail     int    `json:"fail,omitempty"`
	InFlight bool   `json:"in_flight"`
}

// matrixExecutor runs the shipped scripted pipeline and blocks each task's
// chosen turn until shutdown or SIGKILL.
type matrixExecutor struct {
	inner engine.Executor
	mu    sync.Mutex
	calls map[string]int
	block map[string]int
	held  func(string)
}

func (m *matrixExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	m.mu.Lock()
	m.calls[d.TaskID]++
	hold := m.block[d.TaskID] != 0 && m.calls[d.TaskID] == m.block[d.TaskID]
	m.mu.Unlock()
	if !hold {
		return m.inner.Execute(ctx, d)
	}
	m.held(d.TaskID)
	<-ctx.Done()
	return engine.Result{DispatchID: d.ID, TaskID: d.TaskID, Tree: d.Tree, Session: d.Sessions[0].ID, Class: "interrupted", ProducedAt: time.Now().UTC()}, ctx.Err()
}

// steerMatrix installs the holds on a daemon started with executor x and
// returns a channel closed once every task rests at its boundary.
func steerMatrix(d *Daemon, x *matrixExecutor, holds []matrixHold) <-chan struct{} {
	var mu sync.Mutex
	pending := map[string]bool{}
	fail, saves := map[string]int{}, map[string]int{}
	for _, h := range holds {
		pending[h.Task] = true
		if h.Block != 0 {
			x.block[h.Task] = h.Block
		} else {
			fail[h.Task] = h.Fail
		}
	}
	ready := make(chan struct{})
	held := func(task string) {
		mu.Lock()
		defer mu.Unlock()
		if pending[task] {
			delete(pending, task)
			if len(pending) == 0 {
				close(ready)
			}
		}
	}
	x.held = held
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		if point != "save" || fail[id] == 0 {
			return nil
		}
		mu.Lock()
		saves[id]++
		stuck := saves[id] >= fail[id]
		mu.Unlock()
		if !stuck {
			return nil
		}
		// The row rests at its boundary: checkpoint k committed, k+1 refused.
		// Leaving the cache keeps later rounds from retrying it.
		held(id)
		delete(d.jobs, id)
		return &controlplane.RetryableError{Attempts: 1, Cause: errors.New("crash matrix hold")}
	}
	d.mu.Unlock()
	return ready
}

func admitMatrix(t *testing.T, d *Daemon, holds []matrixHold) {
	t.Helper()
	for _, h := range holds {
		if _, err := call(t, d, "Admit", AdmitParams{ID: h.Task, Tree: "tree", AuthorizingMessage: "message"}, h.Task); err != nil {
			t.Fatal(err)
		}
	}
}

// matrixOutcome is the comparable result of one task: terminal state, the
// artifacts and provenance it recorded, and every budget it consumed.
type matrixOutcome struct {
	State      string
	Artifacts  []string
	Provenance []string
	Attempts   int
	TestRuns   int
	Counts     map[string]int
	Failures   map[string]int
}

func matrixResult(t *testing.T, s controlplane.JobSnapshot) matrixOutcome {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(s.Payload, &e); err != nil {
		t.Fatal(err)
	}
	j := e.Job
	o := matrixOutcome{State: s.State, Attempts: j.Attempts, TestRuns: j.TestRuns, Counts: j.Counts, Failures: map[string]int{}}
	for k, v := range j.Failures {
		if v != 0 {
			o.Failures[k] = v
		}
	}
	seen := map[string]bool{}
	for _, p := range j.Provenance {
		if seen[p.DispatchID+"/"+p.Session] {
			t.Fatalf("%s recorded dispatch %s twice", s.ID, p.DispatchID)
		}
		seen[p.DispatchID+"/"+p.Session] = true
		o.Provenance = append(o.Provenance, fmt.Sprintf("%s/%s/%s/%s", p.Stage, p.Primitive, p.Role, p.ExecutionKind))
	}
	for _, a := range j.Evidence {
		fields, _ := json.Marshal(a.Fields)
		canonical, err := controlplane.CanonicalJSON(fields)
		if err != nil {
			t.Fatal(err)
		}
		o.Artifacts = append(o.Artifacts, a.Stage+"/"+a.Type+"/"+string(canonical))
	}
	return o
}

// matrixBaseline runs one uninterrupted task and derives a hold for every
// durable boundary: after checkpoint k the task is either between stages or
// has the turn dispatched by checkpoint k in flight.
func matrixBaseline(t *testing.T) (matrixOutcome, []matrixHold) {
	t.Helper()
	cfg, ctx := fixture(t)
	x := &matrixExecutor{inner: engine.NewScripted(defaults.HappyScript()), calls: map[string]int{}, block: map[string]int{}, held: func(string) {}}
	cfg.Executor = x
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	type save struct {
		completion bool
		calls      int
	}
	var saves []save
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		if point == "save" && id == "baseline" {
			x.mu.Lock()
			calls := x.calls[id]
			x.mu.Unlock()
			saves = append(saves, save{d.pendingCompletion != nil && d.pendingCompletion.id == id, calls})
		}
		return nil
	}
	d.mu.Unlock()
	authorize(t, d)
	admitMatrix(t, d, []matrixHold{{Task: "baseline"}})
	want := matrixResult(t, awaitState(t, d, "baseline", "Done"))
	d.mu.Lock()
	defer d.mu.Unlock()
	holds := []matrixHold{}
	for k := 0; k < len(saves); k++ {
		h := matrixHold{Task: fmt.Sprintf("b%02d", k), K: k}
		if saves[k].completion {
			h.Block, h.InFlight = saves[k].calls, true
		} else {
			h.Fail = k + 1
		}
		holds = append(holds, h)
	}
	return want, holds
}

// TestCrashMatrixHelper is one daemon process of the matrix. "kill" and
// "stop" rest every planned task at its boundary, then wait to be SIGKILLed
// or stop gracefully; "resume" restarts the instance and records outcomes.
func TestCrashMatrixHelper(t *testing.T) {
	path := os.Getenv("CRASH_MATRIX")
	if path == "" {
		return
	}
	mode := os.Getenv("CRASH_MATRIX_MODE")
	var cfg Config
	var holds []matrixHold
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &cfg)
	}
	if raw, err = os.ReadFile(path + ".plan"); err == nil {
		err = json.Unmarshal(raw, &holds)
	}
	if err == nil {
		err = os.Chdir(cfg.Workspace)
	}
	if err != nil {
		t.Fatal(err)
	}
	if mode == "resume" {
		cfg.Executor = engine.NewScripted(defaults.HappyScript())
		var d *Daemon
		for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if d, err = Start(context.Background(), cfg); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
		}
		defer stop(t, d)
		outcomes := map[string]matrixOutcome{}
		for _, h := range holds {
			outcomes[h.Task] = matrixResult(t, awaitState(t, d, h.Task, "Done"))
		}
		raw, _ = json.Marshal(outcomes)
		if err = os.WriteFile(path+".result", raw, 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	x := &matrixExecutor{inner: engine.NewScripted(defaults.HappyScript()), calls: map[string]int{}, block: map[string]int{}}
	cfg.Executor = x
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ready := steerMatrix(d, x, holds)
	authorize(t, d)
	admitMatrix(t, d, holds)
	<-ready
	if mode == "stop" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err = d.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(path+".ready", []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	if mode == "kill" {
		select {}
	}
}

// FAE-ROB-012, FAE-ROB-011, FAE-VER-010: the default pipeline in scripted mode
// is interrupted at every durable boundary, between stages and mid-stage, by
// SIGKILL and by a graceful stop. Every resumed task reaches the uninterrupted
// outcome (state, artifacts, provenance, budgets) with no completed turn
// dispatched twice. Only a SIGKILL of a turn in flight charges dispatch
// recovery; a graceful stop charges nothing. Each instance holds few tasks,
// because every round re-validates every runnable task; instances run in a
// bounded pool of independent daemon processes.
func TestCrashMatrixEveryDurableBoundary(t *testing.T) {
	want, holds := matrixBaseline(t)
	if len(holds) < 40 {
		t.Fatal("default pipeline has too few durable boundaries", len(holds))
	}
	base, err := os.MkdirTemp("/var/tmp", "fae-matrix-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(base) })
	exe, _ := os.Executable()
	type run struct {
		mode, path string
		holds      []matrixHold
	}
	const perInstance = 3
	workers := min(6, max(2, runtime.NumCPU()/2))
	// A plane's port is a hash of its data directory, so concurrent instances
	// take directories whose ports differ from each other's and are free on
	// the host (as planeRoot does for single fixtures).
	ports := map[int]bool{}
	instanceDir := func(name string) string {
		for n := 0; ; n++ {
			dir := filepath.Join(base, name)
			if n > 0 {
				dir = filepath.Join(base, fmt.Sprintf("%s-%d", name, n))
			}
			l, err := Paths(filepath.Join(dir, "instances"), "test")
			if err != nil {
				t.Fatal(err)
			}
			port, err := controlplane.Port(l.Data, 0)
			if err != nil {
				t.Fatal(err)
			}
			if ports[port] {
				continue
			}
			if conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), time.Second); err == nil {
				conn.Close()
				continue
			}
			ports[port] = true
			return dir
		}
	}
	var runs []run
	for _, mode := range []string{"kill", "stop"} {
		for p := 0; p*perInstance < len(holds); p++ {
			r := run{mode: mode, path: filepath.Join(instanceDir(fmt.Sprintf("%s%02d", mode, p)), "matrix.json")}
			// Interleave early and late boundaries so instances cost alike.
			for i := p; i < len(holds); i += (len(holds) + perInstance - 1) / perInstance {
				r.holds = append(r.holds, holds[i])
			}
			runs = append(runs, r)
		}
	}
	helper := func(r run, mode string) (*exec.Cmd, error) {
		cmd := exec.Command(exe, "-test.run=^TestCrashMatrixHelper$")
		cmd.Env = append(os.Environ(), "CRASH_MATRIX="+r.path, "CRASH_MATRIX_MODE="+mode)
		log, e := os.OpenFile(r.path+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			return nil, e
		}
		defer log.Close()
		cmd.Stdout, cmd.Stderr = log, log
		return cmd, cmd.Start()
	}
	await := func(r run, file string) error {
		for deadline := time.Now().Add(4 * time.Minute); ; time.Sleep(50 * time.Millisecond) {
			if _, e := os.Stat(r.path + file); e == nil {
				return nil
			}
			if time.Now().After(deadline) {
				b, _ := os.ReadFile(r.path + ".log")
				return fmt.Errorf("%s helper never produced %s: %s", r.mode, file, b)
			}
		}
	}
	interrupt := func(r run) (map[string]matrixOutcome, error) {
		cfg := Config{Root: filepath.Join(filepath.Dir(r.path), "instances"), Workspace: filepath.Join(filepath.Dir(r.path), "workspace"), Instance: "test", DoltBinary: os.Getenv("FAE_TEST_DOLT_BINARY"), MaxInFlight: -1}
		if e := os.MkdirAll(cfg.Workspace, 0700); e != nil {
			return nil, e
		}
		raw, _ := json.Marshal(cfg)
		plan, _ := json.Marshal(r.holds)
		if e := errors.Join(os.WriteFile(r.path, raw, 0600), os.WriteFile(r.path+".plan", plan, 0600)); e != nil {
			return nil, e
		}
		cmd, e := helper(r, r.mode)
		if e != nil {
			return nil, e
		}
		defer func() { cmd.Process.Kill(); cmd.Wait() }()
		// A SIGKILLed helper leaves its Dolt child for the restart to reclaim;
		// never leave it behind when the restart does not happen.
		defer killMatrixStrays(filepath.Dir(r.path))
		if e = await(r, ".ready"); e != nil {
			return nil, e
		}
		if r.mode == "kill" {
			cmd.Process.Kill()
		}
		cmd.Wait()
		resume, e := helper(r, "resume")
		if e != nil {
			return nil, e
		}
		defer func() { resume.Process.Kill(); resume.Wait() }()
		if e = await(r, ".result"); e != nil {
			return nil, e
		}
		var got map[string]matrixOutcome
		raw, _ = os.ReadFile(r.path + ".result")
		return got, json.Unmarshal(raw, &got)
	}
	jobs := make(chan run)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range jobs {
				got, e := interrupt(r)
				if e != nil {
					t.Error(e)
					continue
				}
				for _, h := range r.holds {
					expected := want
					expected.Failures = map[string]int{}
					if r.mode == "kill" && h.InFlight {
						// FAE-ROB-011: a genuine crash consumes dispatch recovery.
						expected.Failures["dispatch-recovery"] = 1
					}
					if !reflect.DeepEqual(got[h.Task], expected) {
						t.Errorf("%s at boundary %d (in flight %v): got %+v want %+v", r.mode, h.K, h.InFlight, got[h.Task], expected)
					}
				}
			}
		}()
	}
	for _, r := range runs {
		jobs <- r
	}
	close(jobs)
	wg.Wait()
}

// killMatrixStrays kills any process still serving one matrix instance
// directory (a Dolt child or process watchdog of a SIGKILLed helper).
func killMatrixStrays(dir string) {
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		cmdline, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if strings.Contains(string(cmdline), dir+"/") {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
