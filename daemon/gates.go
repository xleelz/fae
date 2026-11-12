package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
)

// FAE-CTL-051 exit codes. An expired bounded admission wait and an immediate
// admission refusal are distinct from each other; a command that ran returns
// its own exit code.
const (
	GateRunTimeoutExit = 75
	GateRunRefusedExit = 76
	// LandingFloor is the Appendix A landing memory floor (FAE-HOST-007).
	LandingFloor = controller.LandingFloor
	// ControllerClass is the heavy-op priority class of operator and
	// controller commands (FAE-SCH-011: role 0, controller 1, operator 2).
	ControllerClass = controller.ControllerClass
)

// HeavyAdmission is the controller-class admission of a host-heavy command
// (FAE-CTL-051, FAE-HOST-007, -010): see controller.HeavyAdmission, which the
// controller roll build and the heavy-op broker share.
type (
	HeavyAdmission   = controller.HeavyAdmission
	AdmissionTimeout = controller.AdmissionTimeout
	AdmissionRefused = controller.AdmissionRefused
)

// SanitizedEnvironment keeps only the process basics and build caches a gated
// command needs; credentials, Fae and Git redirections never reach it.
func SanitizedEnvironment(env []string) []string {
	keep := map[string]bool{"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TMPDIR": true, "TZ": true, "GOCACHE": true, "GOMODCACHE": true, "GOPATH": true, "GOTOOLCHAIN": true, "GOPROXY": true, "GOSUMDB": true, "GONOSUMDB": true, "GOPRIVATE": true, "CARGO_HOME": true, "RUSTUP_HOME": true}
	out := []string{"LANG=C", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1"}
	// As in os/exec, the last value of a repeated key wins.
	at := map[string]int{}
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || !keep[key] {
			continue
		}
		if i, seen := at[key]; seen {
			out[i] = entry
			continue
		}
		at[key] = len(out)
		out = append(out, entry)
	}
	return out
}

// RunGated runs an admitted command in its own process group and returns the
// command's own exit status (128+signal when it was killed by a signal).
func RunGated(ctx context.Context, argv []string, dir string, env []string, stdout, stderr io.Writer) (int, error) {
	if len(argv) == 0 || argv[0] == "" {
		return 0, errors.New("gate run requires a command")
	}
	path := ""
	for _, entry := range env {
		if v, ok := strings.CutPrefix(entry, "PATH="); ok {
			path = v
		}
	}
	binary := argv[0]
	if !strings.Contains(binary, "/") {
		for _, dir := range strings.Split(path, ":") {
			if dir == "" {
				continue
			}
			candidate := dir + "/" + binary
			if info, e := os.Stat(candidate); e == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
				binary = candidate
				break
			}
		}
	}
	cmd := exec.CommandContext(ctx, binary, argv[1:]...)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, env, stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// GateSelfTestCases adds the daemon-owned admission ordering to the pure
// controller gates (FAE-CTL-050).
func GateSelfTestCases() []controller.SelfTestCase {
	cases := controller.BuiltinSelfTests()
	return append(cases, controller.SelfTestCase{Gate: "io-lease-order", Run: func(v any) controller.GateResult {
		in := v.([2]bool)
		r := controller.GateResult{Gate: "io-lease-order", Applicable: true, Count: 1, Passed: CanEnqueueHeavyLease(in[0], in[1])}
		if !r.Passed {
			r.Reason = "heavy lease requested before the I/O gate passed"
		}
		return r
	}, Refuse: []controller.SelfTestInput{{Name: "lease-before-io-gate", Value: [2]bool{true, false}}}, Pass: controller.SelfTestInput{Name: "io-gate-first", Value: [2]bool{true, true}}})
}

// GatedProof is the durable record that one exact commit passed every landing
// gate for a target; it is the only way a refs/fae/gated reference becomes
// deployable (FAE-CTL-002).
type GatedProof struct {
	Ref     string                  `json:"ref"`
	SHA     string                  `json:"sha"`
	Target  string                  `json:"target"`
	Landing string                  `json:"landing"`
	Task    string                  `json:"task"`
	Gates   []controller.GateResult `json:"gates"`
	At      time.Time               `json:"at"`
}

func gatedProofKey(ref, sha string) string {
	return fmt.Sprintf("gated/%x", sha256.Sum256([]byte(ref+"\x00"+sha)))
}

func (d *Daemon) recordGatedProof(ctx context.Context, a controlplane.Audit, p GatedProof) error {
	if !strings.HasPrefix(p.Ref, "refs/fae/gated/") || p.SHA == "" || !controller.LandingGate(p.Gates).Passed {
		return errors.New("gated proof requires a gated reference, exact commit and passing landing gates")
	}
	raw, e := json.Marshal(p)
	if e != nil {
		return e
	}
	a.Action = "landing-gated-proof"
	return d.store.PutMarker(ctx, a, gatedProofKey(p.Ref, p.SHA), raw)
}

// verifyGatedTip refuses a deployment source unless the gated reference's
// current tip has a durable proof that it passed the landing gates.
func (d *Daemon) verifyGatedTip(ctx context.Context, ref, tip string) error {
	raw, e := d.store.GetMarker(ctx, gatedProofKey(ref, tip))
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	var p GatedProof
	recorded := e == nil && json.Unmarshal(raw, &p) == nil
	exact := recorded && p.Ref == ref && p.SHA == tip && controller.LandingGate(p.Gates).Passed
	if !controller.MayDeployGated(strings.HasPrefix(ref, "refs/fae/gated/"), recorded, exact) {
		return fmt.Errorf("ungated tip: %s at %s has no landing gate proof", ref, tip)
	}
	return nil
}

// gatesSelfTest is the daemon read behind fae gates self-test (FAE-CTL-050,
// FAE-API-010): every built-in gate must refuse its failing inputs and pass
// its passing one.
func gatesSelfTest(raw json.RawMessage) (any, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return nil, err
	}
	report, ok := controller.SelfTest(GateSelfTestCases())
	return map[string]any{"passed": ok, "gates": report}, nil
}
