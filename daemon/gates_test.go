package daemon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

func TestGateSelfTestIncludesAdmissionOrder(t *testing.T) {
	cases := GateSelfTestCases()
	report, ok := controller.SelfTest(cases)
	if !ok || len(report) != len(controller.BuiltinSelfTests())+1 || report[len(report)-1].Gate != "io-lease-order" {
		t.Fatalf("%+v", report)
	}
	last := len(cases) - 1
	original := cases[last].Run
	cases[last].Run = func(v any) controller.GateResult { r := original(v); r.Passed = true; return r }
	if _, ok = controller.SelfTest(cases); ok {
		t.Fatal("broken admission-order gate passed self-test")
	}
}

func heavyAdmissionFixture(t *testing.T, gib float64, io10 float64) (HeavyAdmission, *pressureClock, *[]string) {
	t.Helper()
	ledger, e := scheduler.OpenDynamicLedger(filepath.Join(t.TempDir(), "host"), 1)
	if e != nil {
		t.Fatal(e)
	}
	owner, e := scheduler.CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	clock := &pressureClock{now: pressureEpoch}
	alerts := &[]string{}
	sample := hostguard.Sample{Memory: hostguard.Reading{Value: gib * float64(hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: float64(hostguard.GiB), Known: true}, IO10: hostguard.Reading{Value: io10, Known: true}}
	return HeavyAdmission{Ledger: ledger, Owner: owner, Clock: clock, MaxWait: 5 * time.Minute, Slice: time.Minute,
		Probe:   func(scheduler.Owner) scheduler.Liveness { return scheduler.Alive },
		Observe: func(context.Context) (hostguard.Sample, error) { return sample, nil },
		Alert:   func(line string) { *alerts = append(*alerts, line) }}, clock, alerts
}

func TestHeavyAdmissionHeldLeaseNamesHolderPerSliceAndTimesOut(t *testing.T) {
	a, clock, alerts := heavyAdmissionFixture(t, 64, 1)
	foreign := scheduler.Owner{PID: 1, Started: 1}
	if e := a.Ledger.Enqueue(context.Background(), scheduler.Request{ID: "sibling-landing", Owner: foreign, Weight: 1, Queued: time.Now().UTC()}); e != nil {
		t.Fatal(e)
	}
	if _, _, e := a.Ledger.Advance(context.Background(), time.Now().UTC(), map[scheduler.Owner]scheduler.Liveness{}); e != nil {
		t.Fatal(e)
	}
	start := clock.Now()
	_, e := a.Acquire(context.Background(), "gate-run-1")
	var timeout *AdmissionTimeout
	if !errors.As(e, &timeout) || timeout.Stage != "lease" || !strings.Contains(timeout.Detail, "sibling-landing") {
		t.Fatal(e)
	}
	if elapsed := clock.elapsed(start); elapsed < 5*time.Minute || elapsed > 5*time.Minute+10*time.Second {
		t.Fatal("wait not bounded by max-wait", elapsed)
	}
	slices := 0
	for _, line := range *alerts {
		if strings.Contains(line, "gate waiting stage=lease") {
			slices++
			if !strings.Contains(line, "holders=sibling-landing(pid=1") {
				t.Fatal("slice line does not name holder", line)
			}
		}
	}
	// Five minutes of waiting yield one loud line per completed 60 s slice,
	// never one per poll.
	if slices != 4 || !strings.Contains((*alerts)[len(*alerts)-1], "gate timeout stage=lease") {
		t.Fatal(slices, *alerts)
	}
	pool, e := a.Ledger.Snapshot(context.Background())
	if e != nil || len(pool.Waiting) != 0 || len(pool.Holders) != 1 || pool.Holders[0].ID != "sibling-landing" {
		t.Fatal("expired request left queued or foreign holder disturbed", pool, e)
	}
}

func TestHeavyAdmissionMemoryFloorIOGateAndControllerClass(t *testing.T) {
	a, clock, alerts := heavyAdmissionFixture(t, 11.5, 1)
	_, e := a.Acquire(context.Background(), "low-memory")
	var timeout *AdmissionTimeout
	if !errors.As(e, &timeout) || timeout.Stage != "memory" || len(*alerts) < 5 || !strings.Contains((*alerts)[0], "stage=memory") {
		t.Fatal(e, *alerts)
	}
	a, _, _ = heavyAdmissionFixture(t, 1, 1)
	var refused *AdmissionRefused
	if _, e = a.Acquire(context.Background(), "hard"); !errors.As(e, &refused) || refused.Reason != "HostHardLimit" {
		t.Fatal("hard breaker must refuse at once", e)
	}
	a, _, alerts = heavyAdmissionFixture(t, 64, 7)
	a.MaxWait = 150 * time.Second
	// The first held reading at once, one line per completed slice (60 s and
	// 120 s), and the timeout at 150 s.
	if _, e = a.Acquire(context.Background(), "io"); !errors.As(e, &timeout) || timeout.Stage != "io" || len(*alerts) != 4 || !strings.Contains((*alerts)[0], "io_full_avg10=7.00") {
		t.Fatal(e, *alerts)
	}
	a, clock, alerts = heavyAdmissionFixture(t, 12, 4.99)
	start := clock.Now()
	release, e := a.Acquire(context.Background(), "admitted")
	if e != nil {
		t.Fatal(e)
	}
	if clock.elapsed(start) < 30*time.Second {
		t.Fatal("I/O gate lifted without two readings 30 s apart", clock.elapsed(start))
	}
	pool, _ := a.Ledger.Snapshot(context.Background())
	if len(pool.Holders) != 1 || pool.Holders[0].Class != ControllerClass || pool.Holders[0].ID != "admitted" {
		t.Fatal("lease not taken at controller class", pool)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	if pool, _ = a.Ledger.Snapshot(context.Background()); len(pool.Holders) != 0 {
		t.Fatal("lease not released", pool)
	}
	if len(*alerts) != 0 {
		t.Fatal("routine two-reading window raised an alert", *alerts)
	}
	a.MemoryFloor = hostguard.GiB
	if _, e = a.Acquire(context.Background(), "below-hard"); !errors.As(e, &refused) {
		t.Fatal("landing floor below hard floor accepted", e)
	}
}

func TestRunGatedReturnsOwnExitCodeWithSanitizedEnvironment(t *testing.T) {
	env := SanitizedEnvironment(append(os.Environ(), "FAE_LEASE=secret", "GIT_DIR=/elsewhere", "ANTHROPIC_API_KEY=secret", "GOCACHE=/cache"))
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "secret") || strings.Contains(joined, "GIT_DIR") || !strings.Contains(joined, "GOCACHE=/cache") || !strings.Contains(joined, "LANG=C") {
		t.Fatal(env)
	}
	var out bytes.Buffer
	code, e := RunGated(context.Background(), []string{"sh", "-c", "env; exit 7"}, t.TempDir(), env, &out, &out)
	if e != nil || code != 7 || strings.Contains(out.String(), "FAE_LEASE") {
		t.Fatal(code, e, out.String())
	}
	if code, e = RunGated(context.Background(), []string{"sh", "-c", "kill -TERM $$"}, t.TempDir(), env, &out, &out); e != nil || code != 128+15 {
		t.Fatal(code, e)
	}
	if _, e = RunGated(context.Background(), []string{"definitely-not-a-command-fae"}, t.TempDir(), env, &out, &out); e == nil {
		t.Fatal("missing command accepted")
	}
}

func TestGatedProofIsTheOnlyDeployableGatedTip(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	tip := strings.Repeat("a", 40)
	if e = d.verifyGatedTip(ctx, "refs/fae/gated/main", tip); e == nil || !strings.Contains(e.Error(), "ungated tip") {
		t.Fatal("tip without proof accepted", e)
	}
	audit := controlplane.Audit{Actor: "test", Outcome: "ok"}
	passing := []controller.GateResult{{Gate: "suite", Passed: true, Applicable: true, Count: 3}}
	if e = d.recordGatedProof(ctx, audit, GatedProof{Ref: "refs/fae/gated/main", SHA: tip, Gates: []controller.GateResult{{Gate: "suite", Passed: true, Applicable: true}}}); e == nil {
		t.Fatal("vacuous gate proof recorded")
	}
	if e = d.recordGatedProof(ctx, audit, GatedProof{Ref: "refs/heads/main", SHA: tip, Gates: passing}); e == nil {
		t.Fatal("proof outside gated namespace recorded")
	}
	if e = d.recordGatedProof(ctx, audit, GatedProof{Ref: "refs/fae/gated/main", SHA: tip, Gates: passing}); e != nil {
		t.Fatal(e)
	}
	if e = d.verifyGatedTip(ctx, "refs/fae/gated/main", tip); e != nil {
		t.Fatal(e)
	}
	if e = d.verifyGatedTip(ctx, "refs/fae/gated/other", tip); e == nil {
		t.Fatal("proof transferred to another gated reference")
	}
	if e = d.verifyGatedTip(ctx, "refs/fae/gated/main", strings.Repeat("b", 40)); e == nil {
		t.Fatal("proof transferred to another commit")
	}
}

func TestRollRefusesGatedReferenceWithoutLandingProof(t *testing.T) {
	cfg, ctx := fixture(t)
	ws := cfg.Workspace
	liveGit(t, ws, "init")
	os.WriteFile(filepath.Join(ws, "go.mod"), []byte("module example.invalid/rolled\n\ngo 1.23\n"), 0600)
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0600)
	liveGit(t, ws, "add", ".")
	liveGit(t, ws, "commit", "-m", "running")
	build := func(name string) string {
		out := filepath.Join(t.TempDir(), name)
		cmd := exec.Command("go", "build", "-buildvcs=true", "-o", out, ".")
		cmd.Dir = ws
		cmd.Env = append(cleanGitEnv(), "GOFLAGS=", "GOWORK=off")
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatal(e, string(b))
		}
		return out
	}
	running := build("fae-running")
	os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n\nfunc main() { println() }\n"), 0600)
	liveGit(t, ws, "commit", "-am", "next")
	next := liveGit(t, ws, "rev-parse", "HEAD")
	replacement := build("fae-next")
	// An operator (or anything else) can move the gated namespace by hand; the
	// name alone never makes a tip deployable.
	liveGit(t, ws, "update-ref", "refs/fae/gated/main", next)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	hash, _ := controller.HashFile(running)
	d.mu.Lock()
	oldPath, oldHash := d.binaryPath, d.binaryHash
	d.binaryPath, d.binaryHash = running, hash
	d.mu.Unlock()
	defer func() { d.mu.Lock(); d.binaryPath, d.binaryHash = oldPath, oldHash; d.mu.Unlock() }()
	prepare := controller.Params{Action: "prepare", AuthorizingMessage: "message", Note: "deploy", Binary: replacement, GatedRef: "refs/fae/gated/main", Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}}
	if _, e = controllerCall(t, d, prepare); e == nil || !strings.Contains(e.Error(), "ungated tip") {
		t.Fatal("hand-moved gated reference accepted", e)
	}
	if e = d.recordGatedProof(ctx, controlplane.Audit{Actor: "test", Outcome: "ok"}, GatedProof{Ref: "refs/fae/gated/main", SHA: next, Gates: []controller.GateResult{{Gate: "suite", Passed: true, Applicable: true, Count: 1}}}); e != nil {
		t.Fatal(e)
	}
	if _, e = controllerCall(t, d, prepare); e != nil && strings.Contains(e.Error(), "ungated tip") {
		t.Fatal("landing proof not honoured", e)
	}
}
