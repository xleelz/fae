package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

// gateAlerts receives FAE-CTL-051 wait-slice lines; tests replace it.
var gateAlerts io.Writer = os.Stderr

// gateObserve and gateClock are the host telemetry and time sources of fae
// gate run; tests inject deterministic ones.
var (
	gateObserve                 = hostguard.Observe
	gateClock   hostguard.Clock = hostguard.SystemClock{}
	// gatePolicy is the pressure policy; the zero value selects the defaults.
	gatePolicy hostguard.Policy
	// gateProbe reports a ledger owner's liveness; nil selects the host probe.
	gateProbe func(scheduler.Owner) scheduler.Liveness
)

// instanceSpool is the instance's host-alert spool when it exists, else "".
func instanceSpool(layout daemon.Layout) string {
	spool := daemon.HostAlertSpool(layout)
	if info, e := os.Lstat(spool); e != nil || !info.IsDir() {
		return ""
	}
	return spool
}

func gatesCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	if verb == "gates" {
		if len(args) == 0 || args[0] != "self-test" {
			return usage("gates requires self-test")
		}
		f, opts := flags("gates self-test", c)
		offline := f.Bool("offline", false, "run the self-test in this process instead of through the daemon's GatesSelfTest read")
		if e := parse(f, args[1:]); e != nil {
			return e
		}
		var report []controller.SelfTestResult
		var ok bool
		if *offline {
			report, ok = controller.SelfTest(daemon.GateSelfTestCases())
		} else {
			var raw bytes.Buffer
			if e := rpc(ctx, *opts, "GatesSelfTest", "", map[string]any{}, &raw); e != nil {
				return e
			}
			var body struct {
				Passed bool                        `json:"passed"`
				Gates  []controller.SelfTestResult `json:"gates"`
			}
			if e := json.Unmarshal(raw.Bytes(), &body); e != nil {
				return e
			}
			report, ok = body.Gates, body.Passed
		}
		if e := jsonOut(out, map[string]any{"passed": ok, "gates": report}); e != nil {
			return e
		}
		if !ok {
			failed := []string{}
			for _, r := range report {
				if !r.CanFail || !r.CanPass {
					failed = append(failed, r.Gate)
				}
			}
			return &commandError{1, fmt.Errorf("gate self-test failed: %s", strings.Join(failed, ","))}
		}
		return nil
	}
	if len(args) == 0 || args[0] != "run" {
		return usage("gate requires run -- COMMAND")
	}
	split := len(args)
	for i, a := range args {
		if a == "--" {
			split = i
			break
		}
	}
	f, opts := flags("gate run", c)
	defaultLedger, _ := scheduler.DefaultHostLedger()
	ledgerPath := f.String("host-ledger", defaultLedger, "absolute shared host heavy-op ledger directory (default the host-stable ~/.fae/host-ledger)")
	capacity := f.Int("capacity", 2, "heavy-op capacity in weight units when creating a new ledger (1..1000000)")
	weight := f.Int("weight", 1, "heavy-op weight (1 targeted, 2 whole suite or build)")
	maxWait := f.Duration("max-wait", 30*time.Minute, "bounded admission wait (positive duration)")
	slice := f.Duration("slice", time.Minute, "interval between loud wait lines (positive duration)")
	floor := f.Float64("memory-floor-gib", float64(daemon.LandingFloor/hostguard.GiB), "required available memory in GiB (landing floor; at least the hard memory floor)")
	dir := f.String("dir", "", "working directory (default current)")
	fallBack := configurableFlags(f, gateRunDefaults)
	if e := parse(f, args[1:split]); e != nil {
		return e
	}
	if split >= len(args)-1 {
		return usage("gate run requires -- COMMAND")
	}
	fallBack()
	command := args[split+1:]
	if !filepath.IsAbs(*ledgerPath) || *weight < 1 || *weight > 2 || *slice <= 0 {
		return usage("gate run requires an absolute --host-ledger, weight 1..2 and a positive --slice")
	}
	if *dir == "" {
		wd, e := os.Getwd()
		if e != nil {
			return e
		}
		*dir = wd
	}
	ledger, e := scheduler.OpenDynamicLedger(*ledgerPath, *capacity)
	if e != nil {
		return &commandError{daemon.GateRunRefusedExit, e}
	}
	_, thresholds, e := ledger.HostLimits(ctx)
	if e != nil {
		return &commandError{daemon.GateRunRefusedExit, e}
	}
	memoryFloor := gateMemoryFloor(*floor, thresholds, opts.instance)
	owner, e := scheduler.CurrentOwner()
	if e != nil {
		return &commandError{daemon.GateRunRefusedExit, e}
	}
	id, e := identity("gate-run-")
	if e != nil {
		return e
	}
	// FAE-OBS-001: each line carries its time, the instance and the
	// component, as the daemon's supervision log does.
	logLine := func(text string) {
		fmt.Fprintln(gateAlerts, commandshim.HostCommandLogLine(opts.instance, "gate-run", text))
	}
	alert := func(line string) {
		logLine(line)
		if opts.message != "" {
			if err := gateDaemonAlert(ctx, *opts, line); err != nil {
				logLine("alert not recorded id=" + id + ": " + err.Error())
			}
		}
	}
	layout, e := daemon.Paths(opts.home, opts.instance)
	if e != nil {
		return usage("gate run: %v", e)
	}
	// FAE-HOST-003: a hard refusal reaches the instance's alert path through
	// its spool when the instance exists.
	hard := func(s hostguard.Sample) {
		if err := commandshim.SpoolHostAlert(instanceSpool(layout), id, s); err != nil {
			logLine("host alert not spooled id=" + id + ": " + err.Error())
		}
	}
	admission := daemon.HeavyAdmission{Ledger: ledger, Owner: owner, Observe: gateObserve, Clock: gateClock, Policy: gatePolicy, Config: thresholds, MemoryFloor: memoryFloor, Weight: *weight, MaxWait: *maxWait, Slice: *slice, Alert: alert, Hard: hard, Probe: gateProbe}
	release, e := admission.Acquire(ctx, id)
	var timeout *daemon.AdmissionTimeout
	if errors.As(e, &timeout) {
		return &commandError{daemon.GateRunTimeoutExit, e}
	}
	if e != nil {
		return &commandError{daemon.GateRunRefusedExit, e}
	}
	// FAE-HOST-020: the command's scratch lives on disk under the instance
	// root, never in the RAM-backed system temporary directory, and is
	// removed on every exit path, with any read-only tree a test left in it;
	// its owner record lets the next daemon start sweep it after a SIGKILL.
	scratch, removeScratch, e := daemon.GateRunScratch(layout.Root, id)
	if e != nil {
		_ = release()
		return &commandError{daemon.GateRunRefusedExit, e}
	}
	defer func() {
		if err := removeScratch(); err != nil {
			logLine("scratch " + scratch + " not removed: " + err.Error())
		}
	}()
	env := append(daemon.SanitizedEnvironment(os.Environ()), "TMPDIR="+scratch, "GOTMPDIR="+scratch)
	code, runErr := daemon.RunGated(ctx, command, *dir, env, out, os.Stderr)
	releaseErr := release()
	if runErr != nil {
		return &commandError{daemon.GateRunRefusedExit, errors.Join(runErr, releaseErr)}
	}
	if releaseErr != nil {
		logLine("lease release failed id=" + id + ": " + releaseErr.Error())
	}
	if code != 0 {
		return &commandError{code, fmt.Errorf("gated command exited %d", code)}
	}
	return nil
}

// gateRunDefaults are gate run's flags that carry an Appendix A configurable
// default: the landing floor and its wait (FAE-HOST-007) and the heavy-op
// capacity (FAE-SCH-011). --slice has no such default, and --weight's
// Appendix A default would under-count a whole-suite run, so an invalid one
// of either stays a usage error.
var gateRunDefaults = map[string]valueRange{
	"memory-floor-gib": landingFloor,
	"max-wait":         positiveDuration,
	"capacity":         heavyCapacity,
}

// gateMemoryFloor is gate run's landing floor in bytes (FAE-HOST-007). The
// flag's fallback has already replaced an empty, non-finite or out-of-range
// value with 12 GiB; a floor below the host's hard memory floor, which the
// admission would refuse as invalid, falls back the same way. No input can
// make it zero or negative, which would apply only the soft floor.
func gateMemoryFloor(gib float64, thresholds hostguard.Config, instance string) int64 {
	hard := thresholds.Hard
	if thresholds == (hostguard.Config{}) {
		hard = hostguard.DefaultConfig().Hard
	}
	floor, ok := landingFloorBytes(gib)
	if !ok {
		return daemon.LandingFloor // the flag's own fallback has warned
	}
	if floor < hard {
		accepts := fmt.Sprintf("at least the hard memory floor of %g GiB", float64(hard)/float64(hostguard.GiB))
		warnFallback(instance, "gate run", "memory-floor-gib", strconv.FormatFloat(gib, 'g', -1, 64), accepts, strconv.FormatFloat(float64(daemon.LandingFloor)/float64(hostguard.GiB), 'g', -1, 64))
		return daemon.LandingFloor
	}
	return floor
}

// gateDaemonAlert records a wait-slice line as a durable controller alert.
func gateDaemonAlert(ctx context.Context, c common, line string) error {
	layout, e := daemon.Paths(c.home, c.instance)
	if e != nil {
		return e
	}
	token, e := readCredential(layout.Credential)
	if e != nil {
		return e
	}
	raw, _ := json.Marshal(controller.Params{Action: "alert", AuthorizingMessage: c.message, Cause: "gate-wait", Note: line})
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, e = api.Call(call, layout.Socket, token, api.Request{Version: api.Version, Method: "Controller", Params: raw})
	return e
}
