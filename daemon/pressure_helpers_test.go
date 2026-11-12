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
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/scheduler"
)

// Live daemon tests admit against the host's real PSI and load (see init in
// live_admission_pressure_test.go), and the suite raises that pressure itself:
// every fixture runs a private Dolt plane and live workflows run fresh (-a)
// builds. Pressure never fails work: a synchronous operation refused with a
// deferred host-admission refusal was refused before any write, and a task
// parked infrastructure-deferred resumes on its own without charge
// (FAE-HOST-002, -006, -010). The helpers below retry and wait through those
// deferrals, bounded by pressureAllowance and logged. A refusal that is not
// deferred, a charged attempt and an escalation still fail.

// pressureAllowance bounds how long one operation or one wait may stay
// deferred by host pressure before the test fails.
const pressureAllowance = 5 * time.Minute

// deferredReasons are the host-admission refusals that back off, defer and
// park without charging (FAE-HOST-002, -005, -006, -010). HostHardLimit and
// configuration refusals are not among them.
var deferredReasons = map[string]bool{"IOPressure": true, "IOPressureHold": true, "HostPressure": true, "HostPressureHold": true, "PaneLimit": true, "MemoryReservation": true}

// deferredRefusal reports whether e is a deferred host-admission refusal,
// typed in process or carried as "host admission: <Reason>" across the RPC
// boundary.
func deferredRefusal(e error) (string, bool) {
	if e == nil {
		return "", false
	}
	var refusal *scheduler.AdmissionRefusal
	if errors.As(e, &refusal) {
		return refusal.Reason, deferredReasons[refusal.Reason]
	}
	_, rest, ok := strings.Cut(e.Error(), "host admission: ")
	if !ok {
		return "", false
	}
	if end := strings.IndexFunc(rest, func(r rune) bool { return !unicode.IsLetter(r) }); end >= 0 {
		rest = rest[:end]
	}
	return rest, deferredReasons[rest]
}

// admitted runs op, retrying while it returns a deferred host-admission
// refusal, for at most pressureAllowance. It returns op's first other result,
// or the last refusal once the allowance is spent.
func admitted[T any](t testing.TB, what string, op func() (T, error)) (T, error) {
	t.Helper()
	start := time.Now()
	for n := 1; ; n++ {
		v, e := op()
		reason, deferred := deferredRefusal(e)
		if !deferred {
			if n > 1 {
				t.Logf("%s admitted after %d deferred host-admission refusals over %s", what, n-1, time.Since(start).Round(time.Millisecond))
			}
			return v, e
		}
		if time.Since(start) > pressureAllowance {
			t.Logf("%s still refused by host admission (%s) after %d attempts over %s", what, reason, n, time.Since(start).Round(time.Second))
			return v, e
		}
		if n == 1 || n%10 == 0 {
			t.Logf("%s deferred by host admission (%s), attempt %d; retrying", what, reason, n)
		}
		time.Sleep(min(time.Duration(n)*250*time.Millisecond, 2*time.Second))
	}
}

// admittedErr is admitted for an operation with no result.
func admittedErr(t testing.TB, what string, op func() error) error {
	t.Helper()
	_, e := admitted(t, what, func() (struct{}, error) { return struct{}{}, op() })
	return e
}

// executeAdmitted runs one dispatch, re-executing it while the executor
// reports a no-launch infrastructure-deferred refusal by host admission (the
// engine contract for a retryable refusal before any launch or write). Any
// other outcome, including a refusal after a multi-session dispatch already
// launched (uncertain), is returned as is.
func executeAdmitted(t testing.TB, ctx context.Context, x engine.Executor, dispatch engine.Dispatch) (engine.Result, error) {
	t.Helper()
	start := time.Now()
	for n := 1; ; n++ {
		r, e := x.Execute(ctx, dispatch)
		reason, deferred := deferredRefusal(e)
		if !deferred || r.Class != "infrastructure-deferred" || time.Since(start) > pressureAllowance || ctx.Err() != nil {
			if n > 1 {
				t.Logf("dispatch %s (%s) ran after %d deferred host-admission refusals over %s", dispatch.ID, dispatch.Stage, n-1, time.Since(start).Round(time.Millisecond))
			}
			return r, e
		}
		if n == 1 || n%10 == 0 {
			t.Logf("dispatch %s (%s) deferred by host admission (%s), attempt %d; retrying", dispatch.ID, dispatch.Stage, reason, n)
		}
		time.Sleep(min(time.Duration(n)*250*time.Millisecond, 2*time.Second))
	}
}

// backupAdmitted is d.backupPlane retried through deferred host admission.
func backupAdmitted(t testing.TB, ctx context.Context, d *Daemon) (planeBackup, error) {
	t.Helper()
	return admitted(t, "plane backup", func() (planeBackup, error) { return d.backupPlane(ctx) })
}

// tickAdmitted retries one schedule tick through deferred host admission. A
// deferred run is withdrawn, so the retried tick runs the same occurrence.
func tickAdmitted(t testing.TB, what string, tick func() error) error {
	t.Helper()
	return admittedErr(t, what+" tick", tick)
}

// callOnce makes one authenticated RPC with no retry.
func callOnce(t *testing.T, d *Daemon, method string, p any, key string) (api.Response, error) {
	t.Helper()
	raw, _ := json.Marshal(p)
	token, err := os.ReadFile(d.layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	return api.Call(context.Background(), d.layout.Socket, token, api.Request{Version: 1, Method: method, Params: raw, Key: key})
}

// hangGuard bounds a wait by the time the watched work makes no progress.
// Time spent deferred by host pressure does not count, progress restarts the
// count, and an absolute limit (twice the timeout plus pressureAllowance)
// still ends a wait that never finishes. A slow host thus delays a test
// instead of failing it, while a hang still fails.
type hangGuard struct {
	timeout               time.Duration
	idleFrom, last, limit time.Time
	deferredFor           time.Duration
}

func newHangGuard(timeout time.Duration) *hangGuard {
	now := time.Now()
	return &hangGuard{timeout: timeout, idleFrom: now, last: now, limit: now.Add(2*timeout + pressureAllowance)}
}

// progressed records that the watched work advanced.
func (g *hangGuard) progressed() { g.idleFrom = time.Now() }

// observe records one poll; deferred reports that the work is deferred on
// host admission now. It returns false once the guard has expired.
func (g *hangGuard) observe(deferred bool) bool {
	now := time.Now()
	if deferred {
		g.deferredFor += now.Sub(g.last)
		g.idleFrom = g.idleFrom.Add(now.Sub(g.last))
	}
	g.last = now
	return now.Sub(g.idleFrom) < g.timeout && now.Before(g.limit)
}

// deferredPark reports a task parked infrastructure-deferred: refused by host
// admission (or another infrastructure deferral) before launch, uncharged,
// and retried by the drive on its own.
func deferredPark(e envelope) bool {
	return e.Job.Parked && e.Job.Reason == "infrastructure-deferred"
}

// anyDeferredPark reports whether any task of d is parked
// infrastructure-deferred now.
func anyDeferredPark(d *Daemon) bool {
	jobs, err := d.store.ListJobs(context.Background(), 1000)
	if err != nil {
		return false
	}
	for _, j := range jobs {
		var env envelope
		if json.Unmarshal(j.Payload, &env) == nil && deferredPark(env) {
			return true
		}
	}
	return false
}

// awaitJob polls task id until check holds, under a hangGuard of timeout:
// progress (a new job version) restarts it and infrastructure-deferred parks
// extend it. It fails with the last state on expiry or a stopped daemon.
func awaitJob(t *testing.T, d *Daemon, id string, timeout time.Duration, check func(controlplane.JobSnapshot, envelope) bool) (controlplane.JobSnapshot, envelope) {
	t.Helper()
	guard := newHangGuard(timeout)
	version := int64(-1)
	for {
		var env envelope // An omitted in_flight must clear the prior snapshot.
		s, err := d.store.GetJob(context.Background(), id)
		decoded := err == nil && json.Unmarshal(s.Payload, &env) == nil
		if decoded && check(s, env) {
			return s, env
		}
		if err == nil && s.Version != version {
			version = s.Version
			guard.progressed()
		}
		select {
		case <-d.done:
			t.Fatalf("daemon stopped: %v", d.err)
		default:
		}
		if !guard.observe(decoded && deferredPark(env)) {
			t.Fatalf("task %s wait expired: state=%s reason=%s failures=%v err=%v", id, s.State, env.Job.Reason, env.Job.Failures, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// awaitThroughDeferred waits for task id to reach state, waiting through
// infrastructure-deferred parks with the hang guard extended while deferred.
// Any other park, an escalation (unless state is Escalated), a different
// terminal state or a stopped daemon fails at once.
func awaitThroughDeferred(t *testing.T, d *Daemon, id, state string, timeout time.Duration) controlplane.JobSnapshot {
	t.Helper()
	guard := newHangGuard(timeout)
	var s controlplane.JobSnapshot
	var env envelope
	version := int64(-1)
	for {
		var err error
		s, err = d.store.GetJob(context.Background(), id)
		deferred := false
		if err == nil && s.Version != version {
			version = s.Version
			guard.progressed()
		}
		if err == nil && json.Unmarshal(s.Payload, &env) == nil {
			if s.State == state {
				if guard.deferredFor > 0 {
					t.Logf("task %s reached %s after %s parked by host pressure", id, state, guard.deferredFor.Round(time.Millisecond))
				}
				return s
			}
			deferred = deferredPark(env)
			switch {
			case env.Job.Parked && !deferred:
				t.Fatalf("task %s parked in %s: %s; history=%+v", id, s.State, env.Job.Reason, env.Job.History)
			case s.State == "Escalated" || env.Job.State.Terminal():
				t.Fatalf("task %s stopped in %s waiting for %s: %s; history=%+v", id, s.State, state, env.Job.Reason, env.Job.History)
			}
		}
		select {
		case <-d.done:
			t.Fatalf("daemon stopped: %v", d.err)
		default:
		}
		if !guard.observe(deferred) {
			t.Fatalf("task %s did not reach %s: state=%s reason=%s err=%v (deferred %s)", id, state, s.State, env.Job.Reason, err, guard.deferredFor.Round(time.Second))
		}
		time.Sleep(30 * time.Millisecond)
	}
}

// planeRoot makes a fixture scratch root whose Dolt port is free, first
// sweeping orphaned fixture planes off the host (see sweepOrphanPlanes). The plane
// port is derived from the data directory into a shared range, and every
// fixture Dolt on the host (other suites, a leaked plane) holds one, so a
// root whose port is in use is discarded rather than refused at Start
// (FAE-ARC-006 refuses adoption). The probe connects, as Start does.
func planeRoot(t testing.TB, base string) (string, error) {
	t.Helper()
	sweepOrphanPlanes(t.Logf)
	for n := 0; ; n++ {
		root, err := os.MkdirTemp(base, "fae-daemon-test-")
		if err != nil {
			return "", err
		}
		l, err := Paths(filepath.Join(root, "instances"), "test")
		if err != nil {
			return root, err
		}
		port, err := controlplane.Port(l.Data, 0)
		if err != nil {
			return root, err
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		if err == nil {
			conn.Close()
		}
		if errors.Is(err, syscall.ECONNREFUSED) || n == 20 {
			return root, nil
		}
		t.Logf("fixture plane port %d in use (%v); choosing another root", port, err)
		if err = os.RemoveAll(root); err != nil {
			return "", err
		}
	}
}

// subprocess is a crash helper: this test binary running one helper test as a
// separate daemon process, so a parent test can SIGKILL it.
type subprocess struct {
	cmd    *exec.Cmd
	log    string
	exited chan struct{}
}

// startSubprocess runs helper test name with env added, logging to logPath.
// Test cleanup kills and reaps it.
func startSubprocess(t *testing.T, name, logPath string, env ...string) *subprocess {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^"+name+"$")
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	p := &subprocess{cmd: cmd, log: logPath, exited: make(chan struct{})}
	go func() { _ = cmd.Wait(); log.Close(); close(p.exited) }()
	t.Cleanup(p.kill)
	return p
}

// kill SIGKILLs the helper and waits for it to be reaped.
func (p *subprocess) kill() {
	_ = p.cmd.Process.Kill()
	<-p.exited
}

// await polls ready until it holds. The helper bounds its own waits,
// host-pressure deferral included, so a helper that exits first fails the test
// at once with its log, and timeout only guards against a hang.
func (p *subprocess) await(t *testing.T, timeout time.Duration, ready func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	for !ready() {
		select {
		case <-p.exited:
			if ready() {
				return
			}
			out, _ := os.ReadFile(p.log)
			t.Fatalf("helper exited before it was ready: %s", out)
		case <-deadline:
			out, _ := os.ReadFile(p.log)
			t.Fatalf("helper not ready within %s: %s", timeout, out)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// reapProcesses kills, at the end of a test, every process of this user that
// still runs in or names the scratch root: a Dolt plane, a daemon subprocess,
// its process watchdogs, bridge fixtures or brokered builds. A leaked Dolt
// keeps its port, and later fixtures can hash to it (FAE-ARC-006 refuses
// adoption). Processes get a short grace to exit on their own first; one that
// outlives a passing test is a leak and fails it.
func reapProcesses(t testing.TB, root string) {
	t.Helper()
	find := func() map[int]string {
		out := map[int]string{}
		for pid, p := range userProcesses() {
			if strings.Contains(p.cmdline, root) || p.cwd == root || strings.HasPrefix(p.cwd, root+"/") {
				out[pid] = p.cmdline
			}
		}
		return out
	}
	left := find()
	for deadline := time.Now().Add(5 * time.Second); len(left) > 0 && time.Now().Before(deadline); left = find() {
		time.Sleep(100 * time.Millisecond)
	}
	// A failed test may leave its processes behind; a passing one must not.
	report := t.Errorf
	if t.Failed() {
		report = t.Logf
	}
	for pid, cmdline := range left {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		report("reaped leftover process %d after the test: %s", pid, cmdline)
	}
	for deadline := time.Now().Add(5 * time.Second); len(left) > 0 && time.Now().Before(deadline); left = find() {
		time.Sleep(50 * time.Millisecond)
	}
	if len(left) > 0 {
		t.Errorf("processes survived SIGKILL: %s", leftoverSummary(left))
	}
}

// userProcess is one live (not zombie) process of this user, other than the
// test process itself.
type userProcess struct{ cmdline, cwd string }

// userProcesses lists this user's live processes from /proc (empty where
// /proc is absent). A cwd removed under a process keeps its " (deleted)" mark.
func userProcesses() map[int]userProcess {
	out := map[int]userProcess{}
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		if info, err := os.Stat(dir); err != nil || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			continue
		}
		if stat, err := os.ReadFile(filepath.Join(dir, "stat")); err != nil || zombieStat(string(stat)) {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		cwd, _ := os.Readlink(filepath.Join(dir, "cwd"))
		out[pid] = userProcess{strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " ")), cwd}
	}
	return out
}

// fixtureRootPattern matches the scratch roots of daemon and CLI fixtures.
var fixtureRootPattern = regexp.MustCompile(`(?:/var/tmp|/private/tmp)/(?:fae-daemon-test-|fae-cli-native-|fae-cli-serve-)[0-9]+`)

// sweepOrphanPlanes kills fixture Dolt planes, and their process watchdogs,
// that no other process uses any more: an interrupted test never ran its
// cleanup, or a cleanup removed the root under a running plane. A plane holds
// its port, which later fixtures can derive (FAE-ARC-006 refuses adoption). A
// root stays in use while any other process of this user runs in it or names
// it (a test chdirs into its workspace; a crash helper's parent does too), so
// a concurrently running suite's planes are never touched.
func sweepOrphanPlanes(logf func(string, ...any)) {
	planes, inUse := map[string][]int{}, map[string]bool{}
	for pid, p := range userProcesses() {
		root := fixtureRootPattern.FindString(p.cwd)
		if root == "" {
			root = fixtureRootPattern.FindString(p.cmdline)
		}
		if root == "" {
			continue
		}
		if strings.Contains(p.cmdline, "sql-server") || strings.Contains(p.cmdline, "--fae-internal-process-watch") {
			planes[root] = append(planes[root], pid)
		} else {
			inUse[root] = true
		}
	}
	for root, pids := range planes {
		if inUse[root] {
			continue
		}
		for _, pid := range pids {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		logf("killed orphaned fixture plane processes %v of %s", pids, root)
	}
}

// zombieStat reports a /proc/<pid>/stat line in state Z (or X).
func zombieStat(stat string) bool {
	if i := strings.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) {
		return stat[i+2] == 'Z' || stat[i+2] == 'X'
	}
	return false
}

// leftoverSummary names the processes for a failure message.
func leftoverSummary(left map[int]string) string {
	parts := []string{}
	for pid, cmd := range left {
		parts = append(parts, fmt.Sprintf("%d:%s", pid, cmd))
	}
	return strings.Join(parts, "; ")
}

func TestPressureHelpersClassifyRetryAndReap(t *testing.T) {
	for e, want := range map[error]string{
		&scheduler.AdmissionRefusal{Reason: "IOPressure"}:                              "IOPressure",
		admissionDeferredError{&scheduler.AdmissionRefusal{Reason: "PaneLimit"}}:       "PaneLimit",
		errors.New("host admission: IOPressureHold"):                                   "IOPressureHold",
		errors.Join(errors.New("tick"), errors.New("host admission: HostPressure")):    "HostPressure",
		&api.RPCError{Code: "x", Message: "backup: host admission: MemoryReservation"}: "MemoryReservation",
	} {
		if got, ok := deferredRefusal(e); !ok || got != want {
			t.Fatalf("%v: got %q %v", e, got, ok)
		}
	}
	for _, e := range []error{nil, errors.New("host admission: HostHardLimit"), &scheduler.AdmissionRefusal{Reason: "Configuration"}, errors.New("backup operation in progress")} {
		if _, ok := deferredRefusal(e); ok {
			t.Fatalf("%v classified as deferred", e)
		}
	}
	calls := 0
	v, e := admitted(t, "fixture", func() (int, error) {
		if calls++; calls < 3 {
			return 0, &scheduler.AdmissionRefusal{Reason: "IOPressure"}
		}
		return 7, errors.New("real refusal")
	})
	if calls != 3 || v != 7 || e == nil || e.Error() != "real refusal" {
		t.Fatal("deferred refusals not retried up to the real outcome", calls, v, e)
	}
	g := newHangGuard(50 * time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	g.progressed()
	time.Sleep(30 * time.Millisecond)
	if !g.observe(false) {
		t.Fatal("progress did not restart the hang guard")
	}
	time.Sleep(60 * time.Millisecond)
	if g.observe(false) {
		t.Fatal("hang guard did not expire without progress")
	}
	root := t.TempDir()
	sleeper := exec.Command("sleep", "300")
	sleeper.Dir = root
	if e = sleeper.Start(); e != nil {
		t.Fatal(e)
	}
	done := make(chan struct{})
	go func() { _ = sleeper.Wait(); close(done) }()
	leak := &leakRecorder{TB: t}
	reapProcesses(leak, root)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = sleeper.Process.Kill()
		t.Fatal("leftover process not reaped")
	}
	if len(leak.errors) != 1 || !strings.Contains(leak.errors[0], "sleep 300") {
		t.Fatal("leak after a passing test not reported", leak.errors)
	}
}

// leakRecorder captures the errors reapProcesses reports.
type leakRecorder struct {
	testing.TB
	errors []string
}

func (r *leakRecorder) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func TestSweepOrphanPlanesKillsOnlyUnusedRoots(t *testing.T) {
	if _, e := os.Stat("/proc/self/cwd"); e != nil {
		t.Skip("needs /proc")
	}
	start := func(dir string, argv0 string, args ...string) (*exec.Cmd, chan struct{}) {
		t.Helper()
		cmd := exec.Command("bash", append([]string{"-c", `exec -a "$0" sleep 300`, argv0}, args...)...)
		cmd.Dir = dir
		if e := cmd.Start(); e != nil {
			t.Fatal(e)
		}
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
		return cmd, done
	}
	roots := []string{}
	for range 2 {
		root, e := os.MkdirTemp("/var/tmp", "fae-daemon-test-")
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { os.RemoveAll(root) })
		if e = os.Mkdir(filepath.Join(root, "workspace"), 0700); e != nil {
			t.Fatal(e)
		}
		roots = append(roots, root)
	}
	neutral := t.TempDir()
	_, orphanDone := start(neutral, "dolt sql-server --config "+roots[0]+"/instances/test/run/dolt.yaml")
	_, usedDone := start(neutral, "dolt sql-server --config "+roots[1]+"/instances/test/run/dolt.yaml")
	start(filepath.Join(roots[1], "workspace"), "daemon.test")
	time.Sleep(200 * time.Millisecond) // exec -a has replaced bash in both planes
	var logged []string
	sweepOrphanPlanes(func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	select {
	case <-orphanDone:
	case <-time.After(10 * time.Second):
		t.Fatal("orphaned plane survived the sweep", logged)
	}
	select {
	case <-usedDone:
		t.Fatal("sweep killed a plane whose root is in use", logged)
	case <-time.After(300 * time.Millisecond):
	}
	if len(logged) != 1 || !strings.Contains(logged[0], roots[0]) {
		t.Fatal("sweep not reported once for the orphan", logged)
	}
}
