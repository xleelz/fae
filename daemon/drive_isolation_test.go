package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// Census: every per-task drive error point is classified; an unwrapped error
// from a new point is Unclassified, which the property test below forbids.
func TestTaskFaultCensusClassifiesEveryPoint(t *testing.T) {
	live := context.Background()
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		err  error
		ctx  context.Context
		want taskOutcome
	}{
		{taskErr("decode", errors.New("bad payload")), live, outcomeQuarantined},
		{taskErr("validate", errors.New("workflow binding hash mismatch")), live, outcomeQuarantined},
		{taskErr("prepare", errors.New("missing success route")), live, outcomeQuarantined},
		{taskErr("complete", errors.New("stale or terminal completion")), live, outcomeQuarantined},
		{taskErr("persist", errors.New("checkpoint binding mismatch")), live, outcomeQuarantined},
		{&controlplane.RetryableError{Attempts: 5, Cause: errors.New("1213")}, live, outcomeTransient},
		{taskErr("idle", &controlplane.RetryableError{Attempts: 5}), live, outcomeTransient},
		{controlplane.ErrConflict, live, outcomeReloaded},
		{fmt.Errorf("%w: d1", errStaleCompletion), live, outcomeStale},
		{&infrastructureError{errors.New("Dolt restart exhausted")}, stopped, outcomeInfrastructure},
		{taskErr("persist", context.Canceled), stopped, outcomeShutdown},
		{errors.New("unwrapped"), live, outcomeUnclassified},
	}
	for _, tc := range cases {
		if got := classifyTaskError(tc.ctx, tc.err); got != tc.want {
			t.Fatalf("%v: got %s want %s", tc.err, got, tc.want)
		}
	}
}

type faultingExecutor struct {
	good   *engine.Scripted
	mu     sync.Mutex
	target string
	kind   string
	at     int
	seen   int
}

func (f *faultingExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	r, err := f.good.Execute(ctx, d)
	f.mu.Lock()
	defer f.mu.Unlock()
	if d.TaskID != f.target {
		return r, err
	}
	f.seen++
	if f.seen < f.at {
		return r, err
	}
	switch f.kind {
	case "executor-error":
		return r, errors.New("injected executor failure")
	case "foreign-envelope":
		r.Session = "foreign-session"
	}
	return r, err
}

// shortWorkflow keeps the five-task property run within race-instrumented
// bounds: one agent turn, one gate and a typed stop.
func shortWorkflow() workflow.Document {
	schema := json.RawMessage(`{"type":"object","required":["ok"],"additionalProperties":false,"properties":{"ok":{"type":"boolean"}}}`)
	pass := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "work", Path: []string{"ok"}}, {Op: "literal", Value: true}}}
	return currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "short", Start: "work", Limits: workflow.Limits{Attempts: 6, TestRuns: 2, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"worker": {Instructions: "Fixture", Briefs: map[string]string{"default": "fixture"}, Routes: []workflow.Binding{{Provider: "scripted", Model: "fixture"}}, Scope: "task-bound", Cap: 8, Outputs: []string{"Report"}}},
		Artifacts: map[string]workflow.ArtifactType{"Report": {Version: 1, Schema: schema}},
		Stages:    map[string]workflow.Stage{"work": {Primitive: workflow.AgentTurn, Role: "worker", Output: "Report"}, "check": {Primitive: workflow.Gate, Expr: &pass, Failure: "stop"}, "stop": {Primitive: workflow.Escalate, Kind: domain.HumanDecisionRequired}},
		Edges:     []workflow.Edge{{From: "work", To: "check"}, {From: "check", To: "Done"}, {From: "check", To: "stop"}}})
}

// FAE-ROB-001 acceptance: a random per-task failure injected into a 5-task run
// leaves the other four reaching Done; the drive generation never exits and
// every fault is classified at the single choke point.
func TestDriveSurvivesRandomPerTaskFaults(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, kind := range []string{"executor-error", "foreign-envelope", "save-transient", "save-conflict", "corrupt-cache"} {
		t.Run(kind, func(t *testing.T) {
			cfg, ctx := fixture(t)
			cfg.MaxInFlight = 5
			ids := []string{"t0", "t1", "t2", "t3", "t4"}
			target := ids[rng.Intn(len(ids))]
			at := 1 + rng.Intn(4)
			ok := engine.ScriptStep{Output: json.RawMessage(`{"ok":true}`)}
			// A faulted target fails each visit for the same reason, so its
			// fourth identical failure escalates SameGate (Appendix A bound 3).
			script := map[string][]engine.ScriptStep{"work": {ok, ok, ok, ok, ok}}
			if kind == "corrupt-cache" {
				script[target+"/work"] = []engine.ScriptStep{{Output: json.RawMessage(`{"ok":true}`), Delay: 3 * time.Second}}
			}
			executor := &faultingExecutor{good: engine.NewScripted(script), target: target, kind: kind, at: 1}
			cfg.Executor = executor
			d, err := Start(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer stop(t, d)
			var mu sync.Mutex
			var outcomes []taskOutcome
			saves := 0
			d.mu.Lock()
			d.testTaskFault = func(id string, o taskOutcome, err error) {
				mu.Lock()
				outcomes = append(outcomes, o)
				mu.Unlock()
			}
			d.testFault = func(point, id string) error {
				// Only drive checkpoint saves count; Admit's acknowledgement
				// point fires for the same task id.
				if point != "save" || id != target {
					return nil
				}
				saves++
				switch {
				case kind == "save-transient" && saves >= at && saves < at+3:
					return &controlplane.RetryableError{Attempts: 5, Cause: errors.New("injected serialization failure")}
				case kind == "save-conflict" && saves == at:
					// A concurrent writer moved the durable row; the cache must re-adopt it.
					current, e := d.store.GetJob(context.Background(), id)
					if e != nil {
						return e
					}
					if _, e = d.store.SaveJob(context.Background(), controlplane.Audit{Actor: "test", Action: "concurrent writer", Outcome: "fixture"}, current, current.Version); e != nil {
						return e
					}
					return errors.New("injected lost acknowledgement")
				}
				return nil
			}
			d.mu.Unlock()
			authorize(t, d)
			doc := shortWorkflow()
			if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
				t.Fatal(err)
			}
			for _, id := range ids {
				if _, err := call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "corrupt-cache" {
				awaitInFlight(t, d, target, "work")
				d.mu.Lock()
				snapshot := d.jobs[target]
				var e envelope
				json.Unmarshal(snapshot.Payload, &e)
				e.Job.WorkflowHash = "corrupted"
				snapshot.Payload, _ = json.Marshal(e)
				d.jobs[target] = snapshot
				d.mu.Unlock()
			}
			for _, id := range ids {
				if id != target {
					awaitState(t, d, id, "Done")
				}
			}
			switch kind {
			case "executor-error", "foreign-envelope":
				snapshot := awaitState(t, d, target, "Escalated")
				var e envelope
				json.Unmarshal(snapshot.Payload, &e)
				if e.Job.EscalationKind != domain.SameGate {
					t.Fatal("isolated task lacks its typed kind", e.Job.Reason)
				}
			case "save-transient", "save-conflict":
				awaitState(t, d, target, "Done")
			case "corrupt-cache":
				deadline := time.Now().Add(30 * time.Second)
				for {
					d.mu.Lock()
					reason := d.quarantined[target]
					d.mu.Unlock()
					if reason != "" {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("corrupted task not quarantined")
					}
					time.Sleep(20 * time.Millisecond)
				}
				raw, err := call(t, d, "Status", nil, "")
				if err != nil || !strings.Contains(string(raw.Result), target) {
					t.Fatal("quarantine not visible in status", err)
				}
			}
			if g := d.driveGeneration.Load(); g != 1 || !d.driveAlive.Load() {
				t.Fatal("per-task fault ended the drive generation", g)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, o := range outcomes {
				if o == outcomeUnclassified || o == outcomeInfrastructure {
					t.Fatal("per-task fault escaped the classifier", outcomes)
				}
			}
			if (kind == "save-transient" || kind == "save-conflict" || kind == "corrupt-cache") && len(outcomes) == 0 {
				t.Fatal("injected fault never reached the choke point")
			}
		})
	}
}

// FAE-ROB-021: a mutating RPC waits for at most one task's checkpoint, not a
// whole round of slow plane writes.
func TestAdmitDoesNotWaitForTheRound(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MaxInFlight = 8
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	slow := make(chan struct{}, 64)
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		if point == "save" && strings.HasPrefix(id, "slow") {
			slow <- struct{}{}
			time.Sleep(700 * time.Millisecond)
		}
		return nil
	}
	d.mu.Unlock()
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("slow%d", i)
		if _, err := call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	// Wait until a round is inside the slow checkpoints, then admit.
	for i := 0; i < 2; i++ {
		select {
		case <-slow:
		case <-time.After(20 * time.Second):
			t.Fatal("round never started")
		}
	}
	// FAE-ROB-021 acceptance: an admit during the round returns in under 1 s,
	// though each slow checkpoint alone takes 0.7 s of plane time.
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("fast%d", i)
		started := time.Now()
		if _, err := call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
		if elapsed := time.Since(started); elapsed >= time.Second {
			t.Fatal("admit waited for the round", elapsed)
		}
		select {
		case <-slow:
		case <-time.After(20 * time.Second):
			t.Fatal("round stopped")
		}
	}
	// A task control still waits for the checkpoint in flight, never refuses.
	if _, err := call(t, d, "Reprioritize", TaskControlParams{ID: "fast0", Priority: 20, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal("task control refused during a drive checkpoint", err)
	}
}

// FAE-ROB-001: a non-transient failure of an instance-level housekeeping step
// (runtime config, idle sessions, crons, loops, stuck-commit detection) skips
// that step with a typed StepFault outcome and one alert per episode; the
// drive generation never exits and every task still reaches Done. Only an
// infrastructure error ends the generation, which then re-arms.
func TestDriveIsolatesHousekeepingStepFaults(t *testing.T) {
	cfg, ctx := fixture(t)
	script := map[string][]engine.ScriptStep{"work": {{Output: json.RawMessage(`{"ok":true}`)}}}
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	steps := []string{"runtime-config", "idle-sessions", "crons", "loops", "stuck-commit"}
	var mu sync.Mutex
	failing, seen, infra := map[string]bool{}, map[string]int{}, false
	for _, s := range steps {
		failing[s] = true
	}
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		if point != "step" {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if infra && id == "crons" {
			infra = false
			return &infrastructureError{errors.New("injected instance failure")}
		}
		if failing[id] {
			seen[id]++
			return fmt.Errorf("injected %s failure", id)
		}
		return nil
	}
	d.mu.Unlock()
	authorize(t, d)
	doc := shortWorkflow()
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"a", "b", "c"} {
		awaitState(t, d, id, "Done")
	}
	// Several rounds pass with every step still failing.
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		rounds := seen["crons"]
		mu.Unlock()
		if rounds >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("rounds stopped while steps failed")
		}
		d.signal()
	}
	if g := d.driveGeneration.Load(); g != 1 || !d.driveAlive.Load() {
		t.Fatal("a housekeeping step fault ended the drive generation", g)
	}
	alerts := func() int {
		return countRows(t, d, `SELECT COUNT(*) FROM events WHERE kind='drive-step-fault'`)
	}
	if n := alerts(); n != len(steps) {
		t.Fatal("step fault alerts are not one per step episode", n)
	}
	raw, err := call(t, d, "Status", nil, "")
	if err != nil || !strings.Contains(string(raw.Result), "injected crons failure") {
		t.Fatal("step fault not visible in status", err)
	}
	// Recovery closes the episode; a later fault alerts again.
	mu.Lock()
	failing = map[string]bool{}
	mu.Unlock()
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		d.mu.Lock()
		open := len(d.stepFaults)
		d.mu.Unlock()
		if open == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered steps still reported as faulted")
		}
		d.signal()
	}
	mu.Lock()
	failing["crons"] = true
	mu.Unlock()
	for deadline := time.Now().Add(20 * time.Second); alerts() != len(steps)+1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a new step fault episode did not alert")
		}
		d.signal()
	}
	// Infrastructure still ends the generation, which re-arms.
	mu.Lock()
	failing, infra = map[string]bool{}, true
	mu.Unlock()
	d.signal()
	for deadline := time.Now().Add(20 * time.Second); d.driveGeneration.Load() < 2 || !d.driveAlive.Load(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("infrastructure step failure did not end and re-arm the generation", d.driveGeneration.Load())
		}
		d.signal()
	}
}

// FAE-ROB-001: instance-level plane faults outside the per-task classifier
// (the backlog checkpoint, the round's backlog and order reads) skip the round
// with one alert per episode; the generation survives and work resumes.
func TestDriveSurvivesInstanceReadFaults(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"work": {{Output: json.RawMessage(`{"ok":true}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	var mu sync.Mutex
	failing, seen := map[string]bool{}, map[string]int{}
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		mu.Lock()
		defer mu.Unlock()
		if (point == "step" || point == "read") && failing[id] {
			seen[id]++
			return fmt.Errorf("injected %s failure", id)
		}
		return nil
	}
	d.mu.Unlock()
	authorize(t, d)
	doc := shortWorkflow()
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, WorkflowID: doc.ID, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	rounds := func(names ...string) {
		mu.Lock()
		failing = map[string]bool{}
		for _, n := range names {
			failing[n] = true
		}
		seen = map[string]int{}
		mu.Unlock()
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			mu.Lock()
			n := seen[names[len(names)-1]]
			mu.Unlock()
			if n >= 3 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatal("rounds stopped while", names, "failed")
			}
			d.signal()
		}
	}
	rounds("backlog", "backlog-read")
	rounds("order")
	if g := d.driveGeneration.Load(); g != 1 || !d.driveAlive.Load() {
		t.Fatal("an instance read fault ended the drive generation", g)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM events WHERE kind='drive-step-fault'`); n != 3 {
		t.Fatal("read fault alerts are not one per episode", n)
	}
	mu.Lock()
	failing = map[string]bool{}
	mu.Unlock()
	for _, id := range []string{"a", "b"} {
		awaitState(t, d, id, "Done")
	}
	if g := d.driveGeneration.Load(); g != 1 {
		t.Fatal("generation", g)
	}
}

// FAE-ROB-001: a loop collection that cannot be checkpointed is retried
// without reading further loop results; a completion for no in-flight loop is
// dropped with an alert. Neither ends the generation.
func TestDriveLoopCollectionFaultsKeepTheGeneration(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	var mu sync.Mutex
	failing, seen := true, 0
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		mu.Lock()
		defer mu.Unlock()
		if point == "step" && id == "loop-collect" && failing {
			seen++
			return errors.New("injected loop checkpoint failure")
		}
		return nil
	}
	d.mu.Unlock()
	d.loopResults <- loopCompletion{id: "ghost"}
	for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		mu.Lock()
		n := seen
		mu.Unlock()
		if n >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("loop collection was not retried")
		}
		d.signal()
	}
	d.mu.Lock()
	pending := d.pendingLoop != nil
	d.mu.Unlock()
	if g := d.driveGeneration.Load(); g != 1 || !d.driveAlive.Load() || !pending {
		t.Fatal("a loop collection fault ended the generation or lost the completion", g, pending)
	}
	mu.Lock()
	failing = false
	mu.Unlock()
	for deadline := time.Now().Add(20 * time.Second); countRows(t, d, `SELECT COUNT(*) FROM events WHERE kind='loop-completion-dropped'`) != 1; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("stale loop completion was not dropped with an alert")
		}
		d.signal()
	}
	d.mu.Lock()
	pending, open := d.pendingLoop != nil, len(d.stepFaults)
	d.mu.Unlock()
	if g := d.driveGeneration.Load(); g != 1 || pending || open != 0 {
		t.Fatal("stale completion handling", g, pending, open)
	}
}

// FAE-ROB-001: after infrastructure ends a generation the drive re-arms itself
// with bounded backoff (1 s doubling, capped), alerting once per episode, and
// never needs an operator while the re-arm budget lasts.
func TestDriveAutoRearmBacksOffUntilRearmed(t *testing.T) {
	if rearmDelay(0) != time.Second || rearmDelay(1) != 2*time.Second || rearmDelay(3) != 8*time.Second || rearmDelay(10) != 60*time.Second || rearmDelay(100) != 60*time.Second {
		t.Fatal("rearm backoff", rearmDelay(0), rearmDelay(10))
	}
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	var mu sync.Mutex
	infra, refusals := true, 2
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		mu.Lock()
		defer mu.Unlock()
		if point == "step" && id == "crons" && infra {
			infra = false
			return &infrastructureError{errors.New("injected instance failure")}
		}
		if point == "rearm" && refusals > 0 {
			refusals--
			return errors.New("injected rearm refusal")
		}
		return nil
	}
	d.mu.Unlock()
	started := time.Now()
	d.signal()
	for deadline := time.Now().Add(30 * time.Second); d.driveGeneration.Load() < 2 || !d.driveAlive.Load(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("drive did not re-arm itself after refused attempts", d.driveGeneration.Load())
		}
	}
	if elapsed := time.Since(started); elapsed < 3*time.Second {
		t.Fatal("re-arm attempts did not back off", elapsed)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM events WHERE kind='drive-rearm-refused'`); n != 1 {
		t.Fatal("refused re-arm alerts are not one per episode", n)
	}
}
