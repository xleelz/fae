package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

// liveMatrixHold interrupts one live task at one durable boundary, ending its
// round in Mode ("kill" is SIGKILL, "stop" a graceful stop). "between" and
// "completed" refuse the checkpoint with Signature (its first occurrence in
// the round): the task rests between stages, or its local effect completed
// with a durable receipt whose completion checkpoint was lost. "launched"
// holds the bridge launch Launch (stage#ordinal), a turn running in its
// session.
type liveMatrixHold struct {
	Task      string `json:"task"`
	K         int    `json:"k"`
	Kind      string `json:"kind"`
	Mode      string `json:"mode"`
	Stage     string `json:"stage,omitempty"`
	Primitive string `json:"primitive,omitempty"`
	Signature string `json:"signature,omitempty"`
	Launch    string `json:"launch,omitempty"`
}

// liveMatrixHeld is what a held bridge launch announces in <task>.held.
type liveMatrixHeld struct {
	Dispatch string `json:"dispatch"`
	PID      int    `json:"pid"`
	Parent   int    `json:"parent"`
}

// TestLiveMatrixBridgeFixture is the live bridge fixture with one addition:
// the launch named by <holds>/<task>.hold hangs in its session (after
// announcing itself in <task>.held) until it is cancelled or killed. Launches
// are numbered per task and stage across processes by exclusive files.
func TestLiveMatrixBridgeFixture(t *testing.T) {
	holds := os.Getenv("LIVE_MATRIX_HOLDS")
	if os.Getenv("LIVE_DAEMON_BRIDGE") != "1" || holds == "" {
		return
	}
	var c agentruntime.BridgeConfig
	raw, err := os.ReadFile(os.Args[len(os.Args)-1])
	if err != nil || json.Unmarshal(raw, &c) != nil {
		os.Exit(21)
	}
	var brief []struct{ Kind, Stage string }
	if json.Unmarshal(c.Brief, &brief) != nil {
		os.Exit(22)
	}
	stage := ""
	for _, b := range brief {
		if b.Kind == "stage" {
			stage = b.Stage
		}
	}
	ordinal := 1
	for ; ; ordinal++ {
		f, e := os.OpenFile(filepath.Join(holds, fmt.Sprintf("%s.%s.%d", c.TaskID, stage, ordinal)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e == nil {
			f.Close()
			break
		}
		if !os.IsExist(e) {
			os.Exit(29)
		}
	}
	if want, _ := os.ReadFile(filepath.Join(holds, c.TaskID+".hold")); string(want) == fmt.Sprintf("%s#%d", stage, ordinal) {
		held, _ := json.Marshal(liveMatrixHeld{Dispatch: c.DispatchID, PID: os.Getpid(), Parent: os.Getppid()})
		if os.WriteFile(filepath.Join(holds, c.TaskID+".held"), held, 0600) != nil {
			os.Exit(30)
		}
		for {
			time.Sleep(time.Second)
		}
	}
	TestLiveDaemonBridgeFixture(t)
}

// liveMatrixConfig makes one live instance in dir: a private target
// repository and a live configuration whose bridge is the hold fixture. The
// instances of a matrix share one host heavy-op ledger, which bounds their
// concurrent host work as production bounds a host's.
func liveMatrixConfig(t *testing.T, dir, ledger string) Config {
	t.Helper()
	ws := filepath.Join(dir, "workspace")
	for _, p := range []string{ws, filepath.Join(dir, "holds")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	liveGit(t, ws, "init")
	for name, body := range map[string]string{"go.mod": "module example.com/live\n\ngo 1.23\n", "answer.go": "package sample\n\nfunc answer() int { return 0 }\n"} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	liveGit(t, ws, "add", ".")
	liveGit(t, ws, "commit", "-m", "base")
	base := liveGit(t, ws, "rev-parse", "HEAD")
	liveGit(t, ws, "branch", "fae/integration")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Root: filepath.Join(dir, "instances"), Workspace: ws, Instance: "test", DoltBinary: os.Getenv("FAE_TEST_DOLT_BINARY"), MaxInFlight: -1}
	// A held turn is ended only by its round's SIGKILL or stop: the turn
	// timeout must never fire inside the matrix.
	cfg.Live = &LiveConfig{Command: []string{exe, "-test.run=^TestLiveMatrixBridgeFixture$", "--", "{config}"}, Environment: map[string]string{"LIVE_DAEMON_BRIDGE": "1", "LIVE_MATRIX_HOLDS": filepath.Join(dir, "holds")}, AllowedCommands: [][]string{defaults.TestCommand(), {"gofmt", "-l", "."}}, Files: []string{"answer.go", "answer_test.go"}, Base: base, IntegrationRef: "refs/heads/fae/integration", CommitMessage: "live task verified bytes", TestFile: "answer_test.go", TestID: "TestBehavior", ExpectedFailure: "got 0 want 42", HostLedger: ledger, HeavyCapacity: 4, TimeoutSeconds: 900}
	return cfg
}

// liveCheckpointSignature identifies a proposed checkpoint independently of
// dispatch identifiers, attempts and infrastructure deferrals.
func liveCheckpointSignature(raw []byte) (string, engine.Job) {
	var e envelope
	if decodeEnvelope(raw, &e) != nil {
		return "", engine.Job{}
	}
	j := e.Job
	inflight := "-"
	if j.InFlight != nil {
		inflight = j.InFlight.Stage
	}
	return fmt.Sprintf("%s|%s|%t|%s|%d|%d|%d", j.State, inflight, j.Parked, j.Reason, len(j.Provenance), len(j.Evidence), j.TestRuns), j
}

// liveMatrixOutcome is matrixOutcome with the run-specific bytes removed from
// live artifacts: commit identities (their author time differs per run) and
// the digest of a test run's timed output. Trees, verdicts and counts stay.
func liveMatrixOutcome(t *testing.T, s controlplane.JobSnapshot) matrixOutcome {
	t.Helper()
	var e envelope
	if err := json.Unmarshal(s.Payload, &e); err != nil {
		t.Fatal(err)
	}
	for i := range e.Job.Evidence {
		fields := map[string]any{}
		for k, v := range e.Job.Evidence[i].Fields {
			fields[k] = v
		}
		for _, k := range []string{"sha", "output_digest"} {
			if _, ok := fields[k]; ok {
				fields[k] = "<run-specific>"
			}
		}
		e.Job.Evidence[i].Fields = fields
	}
	s.Payload, _ = json.Marshal(e)
	return matrixResult(t, s)
}

// liveMatrixBaseline runs one uninterrupted live task and derives a boundary
// for every distinct durable checkpoint: a turn's completion becomes a
// launched hold of its first session, a local effect's completion a
// lost-checkpoint hold, and every other checkpoint a between-stages hold.
func liveMatrixBaseline(t *testing.T, base, ledger string) (matrixOutcome, []liveMatrixHold) {
	t.Helper()
	cfg := liveMatrixConfig(t, filepath.Join(base, "baseline"), ledger)
	t.Chdir(cfg.Workspace)
	d, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	type save struct {
		signature  string
		completion bool
		parked     bool
		inflight   *engine.Dispatch
	}
	var saves []save
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		if point != "save" || id != "baseline" || d.pendingCheckpoint == nil {
			return nil
		}
		signature, proposed := liveCheckpointSignature(d.pendingCheckpoint.Proposed.Payload)
		s := save{signature: signature, parked: proposed.Parked && proposed.Reason == "infrastructure-deferred"}
		if d.pendingCompletion != nil && d.pendingCompletion.id == id {
			var prior envelope
			if decodeEnvelope(d.pendingCheckpoint.Before.Payload, &prior) == nil && prior.Job.InFlight != nil {
				s.completion, s.inflight = true, prior.Job.InFlight
			}
		}
		saves = append(saves, s)
		return nil
	}
	d.mu.Unlock()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "baseline", AuthorizingMessage: "message"}, "baseline"); err != nil {
		t.Fatal(err)
	}
	want := liveMatrixOutcome(t, awaitThroughDeferred(t, d, "baseline", "Done", 3*time.Minute))
	d.mu.Lock()
	defer d.mu.Unlock()
	holds, seen, launches := []liveMatrixHold{}, map[string]bool{}, map[string]int{}
	for k, s := range saves {
		if s.parked || seen[s.signature] {
			continue
		}
		seen[s.signature] = true
		h := liveMatrixHold{K: k, Kind: "between", Signature: s.signature}
		if s.completion {
			h.Stage, h.Primitive = s.inflight.Stage, string(s.inflight.Config.Primitive)
			h.Kind = "completed"
			if p := s.inflight.Config.Primitive; p == workflow.AgentTurn || p == workflow.Review {
				h.Kind, h.Signature = "launched", ""
				h.Launch = fmt.Sprintf("%s#%d", h.Stage, launches[h.Stage]+1)
				launches[h.Stage] += len(s.inflight.Sessions)
			}
		}
		holds = append(holds, h)
	}
	return want, holds
}

// liveMatrixUncertain reports whether an interruption leaves its dispatch
// parked UncertainDispatch for the operator: a turn SIGKILLed in its session,
// or a reconcile (Merge, which keeps no effect receipt) whose completion was
// never checkpointed. A stopped turn replays, and a local effect with a
// completed receipt is adopted, both without the operator.
func liveMatrixUncertain(h liveMatrixHold) bool {
	return h.Kind == "launched" && h.Mode == "kill" || h.Kind == "completed" && h.Primitive == string(workflow.Merge)
}

// TestLiveCrashMatrixHelper is one daemon process, one round of one task's
// chain (LIVE_MATRIX_ROUND). It installs the round's hold, plays the operator
// for a dispatch the previous round left uncertain, rests the task at its
// boundary and is SIGKILLed or stops gracefully; the last round resumes the
// task to its outcome.
func TestLiveCrashMatrixHelper(t *testing.T) {
	path := os.Getenv("LIVE_MATRIX")
	if path == "" {
		return
	}
	round, err := strconv.Atoi(os.Getenv("LIVE_MATRIX_ROUND"))
	var cfg Config
	var chain []liveMatrixHold
	raw, e := os.ReadFile(path)
	err = errors.Join(err, e, json.Unmarshal(raw, &cfg))
	raw, e = os.ReadFile(path + ".plan")
	err = errors.Join(err, e, json.Unmarshal(raw, &chain))
	if err == nil {
		err = os.Chdir(cfg.Workspace)
	}
	if err != nil {
		t.Fatal(err)
	}
	task := chain[0].Task
	ctx := context.Background()
	var d *Daemon
	// A SIGKILLed predecessor's process watchdog stops its Dolt asynchronously.
	for deadline := time.Now().Add(45 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		if d, err = Start(ctx, cfg); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
	}
	// The hold is in place before the task can move: at once after Start
	// (the drive's first round reads the plane before any checkpoint), and
	// before the operator's re-dispatch below.
	var h liveMatrixHold
	ready := make(chan struct{})
	var once sync.Once
	held := func() { once.Do(func() { close(ready) }) }
	if round < len(chain) && chain[round].Kind != "launched" {
		h = chain[round]
		resting := false
		d.mu.Lock()
		d.testFault = func(point, id string) error {
			if point != "save" || id != task || d.pendingCheckpoint == nil {
				return nil
			}
			if proposed, _ := liveCheckpointSignature(d.pendingCheckpoint.Proposed.Payload); proposed != h.Signature && !resting {
				return nil
			}
			// Checkpoint k is committed and k+1 refused; leaving the cache
			// keeps later rounds from retrying it (see steerMatrix).
			resting = true
			held()
			delete(d.jobs, id)
			return &controlplane.RetryableError{Attempts: 1, Cause: errors.New("live crash matrix hold")}
		}
		d.mu.Unlock()
	}
	holdDir := cfg.Live.Environment["LIVE_MATRIX_HOLDS"]
	os.Remove(filepath.Join(holdDir, task+".hold"))
	os.Remove(filepath.Join(holdDir, task+".held"))
	if round < len(chain) && chain[round].Kind == "launched" {
		h = chain[round]
		if err = os.WriteFile(filepath.Join(holdDir, task+".hold"), []byte(h.Launch), 0600); err != nil {
			t.Fatal(err)
		}
		go func() {
			for {
				if _, e := os.Stat(filepath.Join(holdDir, task+".held")); e == nil {
					held()
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}
	if round == 0 {
		authorize(t, d)
		if _, err = call(t, d, "Admit", AdmitParams{ID: task, AuthorizingMessage: "message"}, task); err != nil {
			t.Fatal(err)
		}
	} else {
		// FAE-ROB-016: an uncertain dispatch is never re-run automatically.
		// Its tree is unchanged and its sessions retired, so the operator
		// re-dispatches it.
		s, e := d.store.GetJob(ctx, task)
		var env envelope
		if e == nil {
			e = decodeEnvelope(s.Payload, &env)
		}
		if e != nil {
			t.Fatal(e)
		}
		j := env.Job
		uncertain, want := j.Parked && j.Reason == "UncertainDispatch" && j.InFlight != nil, liveMatrixUncertain(chain[round-1])
		if uncertain != want {
			t.Fatalf("round %d task %s: uncertain park %v, expected %v: state=%s reason=%s failures=%v history=%+v", round, task, uncertain, want, j.State, j.Reason, j.Failures, j.History)
		}
		if uncertain {
			if _, e = call(t, d, "ResolveUncertain", ResolveUncertainParams{ID: task, DispatchID: j.InFlight.ID, Action: "redispatch", AuthorizingMessage: "message"}, ""); e != nil {
				t.Fatalf("round %d task %s: redispatch of uncertain %s: %v", round, task, j.InFlight.Stage, e)
			}
		}
	}
	if round == len(chain) {
		defer stop(t, d)
		raw, _ = json.Marshal(liveMatrixOutcome(t, awaitThroughDeferred(t, d, task, "Done", 3*time.Minute)))
		if err = os.WriteFile(path+".result", raw, 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	select {
	case <-ready:
	case <-time.After(4*time.Minute + pressureAllowance):
		s, _ := d.store.GetJob(ctx, task)
		t.Fatalf("round %d task %s never rested at %+v: state=%s", round, task, h, s.State)
	}
	if h.Mode == "stop" {
		stopCtx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		if err = d.Close(stopCtx); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(fmt.Sprintf("%s.ready%d", path, round), []byte(h.Mode), 0600); err != nil {
		t.Fatal(err)
	}
	if h.Mode == "kill" {
		select {}
	}
}

// FAE-VER-010, FAE-ROB-012, FAE-ROB-016, FAE-ROB-011: the shipped default
// pipeline in live mode (the live bridge fixture, the production broker,
// worktrees, host leases, commit and integration) is interrupted at every
// distinct durable boundary of an uninterrupted run: between stages (by
// SIGKILL, then by a graceful stop at the same boundary), with a turn running
// in its session (SIGKILL, then a graceful stop of its re-dispatch), and with
// a local host or VCS effect completed whose completion checkpoint was lost
// (SIGKILL or a graceful stop, alternately). A task walks a chain of
// boundaries across restarts of its own instance, so the matrix costs five
// live pipelines rather than one per boundary. Every task reaches the
// uninterrupted outcome: a lost effect checkpoint is adopted from its receipt
// without charge, a stopped turn replays without charge, and a SIGKILLed turn
// parks for the operator, whose re-dispatch charges crash recovery once and
// visits the stage in a new attempt.
func TestLiveCrashMatrixEveryDurableBoundary(t *testing.T) {
	if os.Getenv("FAE_TEST_DOLT_BINARY") == "" {
		t.Skip("FAE_TEST_DOLT_BINARY required for isolated daemon integration")
	}
	base, err := os.MkdirTemp("/var/tmp", "fae-lm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { killMatrixStrays(base); os.RemoveAll(base) })
	ledger := filepath.Join(base, "host-ledger.json")
	want, holds := liveMatrixBaseline(t, base, ledger)
	kinds, primitives := map[string]int{}, map[string]bool{}
	var charged, others [][]liveMatrixHold
	completed := 0
	for _, h := range holds {
		kinds[h.Kind]++
		primitives[h.Primitive] = true
		var group []liveMatrixHold
		switch h.Kind {
		case "completed":
			// Recovery adopts a receipt whatever the stop: one interruption.
			h.Mode = []string{"kill", "stop"}[completed%2]
			completed++
			group = []liveMatrixHold{h}
		case "launched":
			stopped := h
			stopped.Mode = "stop"
			stage, ordinal, _ := strings.Cut(h.Launch, "#")
			n, _ := strconv.Atoi(ordinal)
			stopped.Launch = fmt.Sprintf("%s#%d", stage, n+1)
			h.Mode = "kill"
			group = []liveMatrixHold{h, stopped}
		default:
			stopped := h
			h.Mode, stopped.Mode = "kill", "stop"
			group = []liveMatrixHold{h, stopped}
		}
		if liveMatrixUncertain(group[0]) && group[0].Mode == "kill" {
			charged = append(charged, group)
		} else {
			others = append(others, group)
		}
	}
	if len(holds) < 45 || kinds["launched"] < 8 || kinds["completed"] < 10 {
		t.Fatal("live pipeline has too few durable boundaries", len(holds), kinds)
	}
	for _, p := range []workflow.Primitive{workflow.AgentTurn, workflow.Review, workflow.HostCommand, workflow.FreezeArtifact, workflow.CompareFrozen, workflow.Merge, workflow.VcsCommit, workflow.Integrate} {
		if !primitives[string(p)] {
			t.Fatal("no boundary interrupts a live", p)
		}
	}
	// Each chain is one task in its own instance: tasks sharing a target
	// would reconcile against each other's landings. Chains interleave early
	// and late boundaries; a chain charges crash recovery at most twice,
	// inside the dispatch-recovery bound (FAE-ROB-017).
	const width = 5
	chains := make([][][]liveMatrixHold, width)
	for i, group := range append(charged, others...) {
		chains[i%width] = append(chains[i%width], group)
	}
	exe, _ := os.Executable()
	run := func(chain []liveMatrixHold, cfg Config) (matrixOutcome, error) {
		var got matrixOutcome
		dir := filepath.Dir(cfg.Workspace)
		path := filepath.Join(dir, "matrix.json")
		raw, _ := json.Marshal(cfg)
		plan, _ := json.Marshal(chain)
		if e := errors.Join(os.WriteFile(path, raw, 0600), os.WriteFile(path+".plan", plan, 0600)); e != nil {
			return got, e
		}
		// A SIGKILLed helper leaves its Dolt child and a held turn's bridge
		// for the next round's startup to reclaim; never leave them behind
		// when the chain ends early.
		defer killMatrixStrays(dir)
		for round := 0; round <= len(chain); round++ {
			cmd := exec.Command(exe, "-test.run=^TestLiveCrashMatrixHelper$")
			cmd.Env = append(os.Environ(), "LIVE_MATRIX="+path, fmt.Sprintf("LIVE_MATRIX_ROUND=%d", round))
			log, e := os.OpenFile(path+".log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
			if e != nil {
				return got, e
			}
			cmd.Stdout, cmd.Stderr = log, log
			e = cmd.Start()
			log.Close()
			if e != nil {
				return got, e
			}
			marker, last := fmt.Sprintf(".ready%d", round), round == len(chain)
			if last {
				marker = ".result"
			}
			exited := make(chan struct{})
			go func() { cmd.Wait(); close(exited) }()
			e = awaitLiveMatrix(path, marker, exited)
			if e != nil || last || chain[round].Mode == "kill" {
				cmd.Process.Kill()
			}
			<-exited
			if e != nil {
				return got, fmt.Errorf("round %d of %d: %w", round, len(chain), e)
			}
		}
		raw, _ = os.ReadFile(path + ".result")
		return got, json.Unmarshal(raw, &got)
	}
	var wg sync.WaitGroup
	for c, groups := range chains {
		var chain []liveMatrixHold
		for _, group := range groups {
			for _, h := range group {
				h.Task = fmt.Sprintf("c%d", c)
				chain = append(chain, h)
			}
		}
		sort.SliceStable(chain, func(i, j int) bool { return chain[i].K < chain[j].K })
		cfg := liveMatrixConfig(t, filepath.Join(base, fmt.Sprintf("c%d", c)), ledger)
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := run(chain, cfg)
			if e != nil {
				t.Errorf("chain %d: %v", c, e)
				return
			}
			expected := want
			expected.Failures, expected.Counts = map[string]int{}, map[string]int{}
			for k, v := range want.Counts {
				expected.Counts[k] = v
			}
			var walk []string
			for _, h := range chain {
				walk = append(walk, fmt.Sprintf("%s:%s@%d/%s", h.Mode, h.Kind, h.K, h.Stage))
				if !liveMatrixUncertain(h) {
					continue
				}
				// The operator's re-dispatch refunds nothing: the stage is
				// visited again in a new attempt, and only a genuine crash
				// charges crash recovery (FAE-ROB-011).
				expected.Attempts++
				expected.Counts[h.Stage]++
				if h.Mode == "kill" {
					expected.Failures["crash-recovery"]++
				}
			}
			if len(walk) == 0 || !reflect.DeepEqual(got, expected) {
				t.Errorf("chain %d %v: got %+v want %+v", c, walk, got, expected)
			}
		}()
	}
	wg.Wait()
}

// awaitLiveMatrix waits for a helper's marker file. The helper bounds its own
// waits, host-pressure deferral included; this bound only catches a hang.
func awaitLiveMatrix(path, marker string, exited <-chan struct{}) error {
	deadline := time.After(5*time.Minute + pressureAllowance)
	for {
		if _, e := os.Stat(path + marker); e == nil {
			return nil
		}
		select {
		case <-exited:
			if _, e := os.Stat(path + marker); e == nil {
				return nil
			}
			b, _ := os.ReadFile(path + ".log")
			return fmt.Errorf("helper exited without %s: %s", marker, liveMatrixFailure(b))
		case <-deadline:
			b, _ := os.ReadFile(path + ".log")
			return fmt.Errorf("helper never produced %s: %s", marker, liveMatrixFailure(b))
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// liveMatrixFailure reports a helper log by its last test failure, without
// the full task history the failure embeds.
func liveMatrixFailure(b []byte) string {
	text := string(b)
	if i := strings.LastIndex(text, "crash_matrix_live_test.go:"); i >= 0 {
		text = text[i:]
	}
	if len(text) > 1500 {
		text = text[:1500] + "..."
	}
	return text
}
