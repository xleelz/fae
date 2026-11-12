package commandshim

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

// HeavyOpRefused is the broker verb's refusal code. It is distinct from the
// shim refusal (77) and the CLI's 1/2/3; a run command's own code passes through.
const HeavyOpRefused = 75

// LeaseMarker is the heavy-op permit marker. Every FAE_HEAVY_OP_* variable is
// stripped from the child (FAE-ROB-030), so a nested build can never claim its
// parent's permit and must pass through a shim and take, or be refused, a lease.
const LeaseMarker = "FAE_HEAVY_OP_LEASE"

type HeavyOpRefusal struct{ Reason string }

func (r *HeavyOpRefusal) Error() string {
	return fmt.Sprintf("heavy-op refused (exit %d): %s", HeavyOpRefused, r.Reason)
}

// HeavyOp runs one command under the host heavy-op lease (FAE-SCH-011).
type HeavyOp struct {
	// Namespace execution keeps admission on the host while execution stays in the private view.
	proxySocket    string
	owner          *scheduler.Owner
	execute        func(context.Context) (int, error)
	Ledger         string
	Weight         int
	Class          string
	Argv, Env      []string
	Dir            string
	MaxWait        time.Duration
	Observe        func(context.Context) (hostguard.Sample, error)
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Admit, when set, re-validates the command under the granted lease and
	// returns the vector to run; its error refuses with the shim code (77).
	Admit func(leased bool) ([]string, error)
	// AlertSpool, when set, receives one record per hard-breaker refusal for
	// the owning daemon to raise as a host-admission alert (FAE-HOST-003).
	AlertSpool string
	// Instance names the instance in the run's log lines (FAE-OBS-001); an
	// AlertSpool in an instance's layout names its own instance instead.
	Instance string
	// Clock, Policy and Probe are test seams; zero values select the host
	// clock, the requirement defaults and scheduler.ProbeOwner.
	Clock  hostguard.Clock
	Policy hostguard.Policy
	Probe  func(scheduler.Owner) scheduler.Liveness
}

func StripLeaseMarkers(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "FAE_HEAVY_OP_") {
			out = append(out, e)
		}
	}
	return out
}

// RunHeavyOp admits the command through the shared host admission
// (controller.HeavyAdmission): the hard breaker refuses at once and is
// spooled as an alert; soft pressure backs off, for a role-class command at
// most the policy's checks (FAE-HOST-002, -005), and for a controller- or
// operator-class command until MaxWait, as landing and gate run wait; the
// two-reading I/O gate passes before the lease is
// requested (FAE-HOST-010); the lease is waited for FIFO with proven-dead
// owners reclaimed on every access (FAE-HOST-031); and one reading under the
// granted lease must still be clear. The command then runs with permit
// markers stripped, and the lease is released after the child exits. A
// refusal never runs the command and leaves no queued request behind.
func RunHeavyOp(ctx context.Context, h HeavyOp) (int, error) {
	if h.proxySocket != "" {
		return runProxyCommand(ctx, h)
	}
	refuse := func(format string, args ...any) (int, error) {
		return HeavyOpRefused, &HeavyOpRefusal{fmt.Sprintf(format, args...)}
	}
	_, ok := controller.HeavyClasses[h.Class]
	if !ok || h.Weight < 1 || len(h.Argv) == 0 || !filepath.IsAbs(h.Ledger) {
		return refuse("requires an absolute host ledger, weight >= 1, class role|controller|operator and a command")
	}
	if info, err := os.Stat(filepath.Join(h.Ledger, "state.json")); err != nil || !info.Mode().IsRegular() {
		return refuse("host heavy-op ledger is missing or unreadable; the broker never provisions one")
	}
	path, err := exec.LookPath(h.Argv[0])
	if err != nil {
		return refuse("command not found: %s", h.Argv[0])
	}
	ledger, err := scheduler.OpenDynamicLedger(h.Ledger, h.Weight)
	if err != nil {
		return refuse("host ledger: %v", err)
	}
	owner, err := scheduler.CurrentOwner()
	if h.owner != nil {
		owner, err = *h.owner, nil
	}
	if err != nil {
		return refuse("owner identity: %v", err)
	}
	pool, err := ledger.Snapshot(ctx)
	if err != nil {
		return refuse("host ledger: %v", err)
	}
	if h.Weight > pool.Capacity {
		return refuse("weight %d exceeds the heavy-op capacity %d", h.Weight, pool.Capacity)
	}
	_, cfg, err := ledger.HostLimits(ctx)
	if err != nil {
		return refuse("host limits: %v", err)
	}
	b := make([]byte, 12)
	if _, err = rand.Read(b); err != nil {
		return refuse("request identity: %v", err)
	}
	id := "heavy-op-" + hex.EncodeToString(b)
	stderr := h.Stderr
	if stderr == nil {
		stderr = io.Discard
	}
	instance := heavyOpInstance(h)
	logLine := func(text string) { fmt.Fprintln(stderr, HostCommandLogLine(instance, "heavy-op", text)) }
	observe := h.Observe
	if observe == nil {
		observe = hostguard.Observe
	}
	policy := h.Policy
	if policy == (hostguard.Policy{}) {
		policy = hostguard.DefaultPolicy()
	}
	maxWait := h.MaxWait
	if maxWait <= 0 {
		maxWait = 1800 * time.Second
	}
	// A role command backs off at most the policy's checks and then refuses
	// (FAE-HOST-002), so a role cannot hold a lease queue open under pressure;
	// a controller or operator command waits for the host until MaxWait.
	hostChecks := 0
	if h.Class == "role" {
		hostChecks = policy.Checks
	}
	admission := controller.HeavyAdmission{Ledger: ledger, Owner: owner, Observe: observe, Clock: h.Clock, Policy: policy, Config: cfg,
		MemoryFloor: -1, Weight: h.Weight, Class: h.Class, MaxWait: maxWait, HostChecks: hostChecks, Probe: h.Probe,
		// The pool's own waiting event is the lease wait's one loud line
		// (FAE-SCH-011.4), so no slice line repeats it. The host readings a
		// wait holds on are still recorded (FAE-HOST-005): the reading that
		// starts a hold at once, the one that lifts it, and a timeout's.
		Slice: maxWait,
		Alert: logLine,
		Hard: func(s hostguard.Sample) {
			if e := SpoolHostAlert(h.AlertSpool, id, s); e != nil {
				logLine(fmt.Sprintf("host alert not spooled id=%s: %v", id, e))
			}
		}}
	release, err := admission.Acquire(ctx, id)
	if err != nil {
		return refuse("host admission: %v", err)
	}
	defer func() {
		if e := release(); e != nil {
			logLine(fmt.Sprintf("lease release failed id=%s: %v", id, e))
		}
	}()
	if h.Admit != nil {
		argv, err := h.Admit(true)
		if err != nil {
			return Refused, err
		}
		if path, err = exec.LookPath(argv[0]); err != nil {
			return refuse("command not found: %s", argv[0])
		}
		h.Argv = argv
	}
	if h.execute != nil {
		return h.execute(ctx)
	}
	cmd := exec.CommandContext(ctx, path, h.Argv[1:]...)
	cmd.Args[0] = h.Argv[0]
	env := h.Env
	if env == nil {
		env = os.Environ()
	}
	cmd.Env, cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = StripLeaseMarkers(env), h.Dir, h.Stdin, h.Stdout, h.Stderr
	// The child dies with the lease owner, so a dead owner proves the work stopped.
	configureHeavyOpParentDeath(cmd)
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	if err != nil {
		return refuse("start: %v", err)
	}
	return 0, nil
}

// HostCommandLogLine is one log line of a host-heavy command Fae runs
// outside its daemon (heavy-op run, gate run), in the form of the daemon's
// supervision log (FAE-OBS-001): an RFC 3339 UTC timestamp, the instance and
// the component, then text. An empty instance is written as "unknown".
func HostCommandLogLine(instance, component, text string) string {
	if instance == "" {
		instance = "unknown"
	}
	return time.Now().UTC().Format(time.RFC3339Nano) + " instance=" + instance + " component=" + component + " " + text
}

// heavyOpInstance is the instance h's log lines name: the one whose layout
// holds h's alert spool (<home>/<instance>/run/host-alerts), which a role
// session's shim policy names, else h.Instance.
func heavyOpInstance(h HeavyOp) string {
	run := filepath.Dir(h.AlertSpool)
	if h.AlertSpool != "" && filepath.Base(h.AlertSpool) == "host-alerts" && filepath.Base(run) == "run" {
		return filepath.Base(filepath.Dir(run))
	}
	return h.Instance
}

// HostAlert is one spooled hard-breaker refusal of a brokered command.
type HostAlert struct {
	ID              string    `json:"id"`
	At              time.Time `json:"at"`
	Reason          string    `json:"reason"`
	MemoryAvailable *float64  `json:"memory_available,omitempty"`
	SwapTotal       *float64  `json:"swap_total,omitempty"`
}

// SpoolHostAlert records a hard refusal in the owning daemon's private spool
// directory; an empty spool records nothing.
func SpoolHostAlert(spool, id string, s hostguard.Sample) error {
	if spool == "" {
		return nil
	}
	if !filepath.IsAbs(spool) {
		return errors.New("alert spool must be absolute")
	}
	if info, e := os.Lstat(spool); e != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.Join(e, errors.New("alert spool must be a private directory"))
	}
	value := func(r hostguard.Reading) *float64 {
		if !r.Known {
			return nil
		}
		v := r.Value
		return &v
	}
	raw, e := json.Marshal(HostAlert{ID: id, At: time.Now().UTC(), Reason: "HostHardLimit", MemoryAvailable: value(s.Memory), SwapTotal: value(s.Swap)})
	if e != nil {
		return e
	}
	temp, e := os.CreateTemp(spool, ".alert-*")
	if e != nil {
		return e
	}
	_, e = temp.Write(raw)
	if e = errors.Join(e, temp.Close()); e == nil {
		e = os.Rename(temp.Name(), filepath.Join(spool, id+".json"))
	}
	if e != nil {
		os.Remove(temp.Name())
	}
	return e
}
