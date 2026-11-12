package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// controllerClient is the one roll and maintenance client: fae roll,
// maintenance and land --then-roll all accept a roll only after checking the
// running watchdog's sandbox state (FAE-CTL-001 step 6).
func controllerClient(layout daemon.Layout, call controller.RPC) controller.Client {
	return controller.Client{RPC: call, MarkerPath: filepath.Join(layout.Root, "controller-maintenance.json"), WatchdogIdentity: controller.WatchdogIdentityPath(filepath.Join(layout.Run, "watchdog-state.json"))}
}

// runRoll performs a planned roll; tests substitute it to observe the client.
var runRoll = func(ctx context.Context, c controller.Client, o controller.Options) (controller.Health, error) {
	r, err := runControllerOperation(ctx, c, "roll", o)
	return r.Health, err
}

// Submit one complete operation; the launcher owns its state machine across
// daemon replacement. The CLI only transports requests and polls the receipt.
func runControllerOperation(ctx context.Context, c controller.Client, action string, o controller.Options) (controller.OperationResult, error) {
	// The launcher can have a different working directory from the invoking
	// CLI. Resolve local path input before moving execution to that service.
	if o.Binary != "" {
		binary, err := filepath.Abs(o.Binary)
		if err != nil {
			return controller.OperationResult{}, err
		}
		o.Binary = binary
	}
	request := controller.OperationRequest{Action: action, Options: o}
	var result controller.OperationResult
	var err error
	if action == "start" {
		marker, e := controller.ReadMarker(c.MarkerPath)
		if e != nil {
			return result, e
		}
		if marker == nil {
			return result, fmt.Errorf("authorized maintenance marker required")
		}
		if o.Launcher == "" {
			o.Launcher = marker.Launcher
			request.Options = o
		}
		result, err = controller.OperationRPC(ctx, o.Launcher, request)
	} else {
		root := filepath.Dir(c.MarkerPath)
		credential, e := readCredential(filepath.Join(root, "run", "credential"))
		if e != nil {
			return result, dialError(e)
		}
		raw, _ := json.Marshal(daemon.ControllerOperationParams{AuthorizingMessage: o.Message, Operation: request})
		response, e := api.Call(ctx, filepath.Join(root, "run", "rpc.sock"), credential, api.Request{Version: api.Version, Method: "ControllerOperation", Params: raw})
		if e != nil {
			return result, dialError(e)
		}
		err = json.Unmarshal(response.Result, &result)
	}
	if err != nil {
		return result, err
	}
	return controller.WaitOperation(ctx, o.Launcher, result)
}

// controllerDefaults are the controller verbs' flags that carry an Appendix A
// configurable default: the roll's clear-window wait and overlap hold, and
// the watchdog's idle alert, re-arm budget and filesystem alert.
var controllerDefaults = map[string]valueRange{
	"max-wait":     positiveDuration,
	"overlap-wait": positiveDuration,
	"stall":        positiveDuration,
	"rearm-budget": positiveCount,
	"rearm-window": positiveDuration,
	"fs-threshold": {"a fraction strictly between 0 and 1", func(v any) bool { x := v.(float64); return x > 0 && x < 1 }},
}

// controllerCommand serves drive, maintenance, roll, watchdog and
// runtime-launcher. Each verb (and maintenance or drive action) takes only
// its own flags, so a flag that means nothing to it is a usage error.
func controllerCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	action := ""
	if verb == "maintenance" || verb == "drive" {
		if len(args) == 0 {
			return usage("%s requires %s", verb, map[string]string{"maintenance": "stop, start or status", "drive": "stop, rearm or status"}[verb])
		}
		action, args = args[0], args[1:]
	}
	name := strings.TrimSpace(verb + " " + action)
	f, opts := flags(name, c)
	var o controller.Options
	var socket, revokeGrant string
	var period, stall, rearmWindow time.Duration
	var rearmBudget int
	var fsThreshold float64
	switch name {
	case "roll", "maintenance stop", "maintenance start":
		f.StringVar(&o.Launcher, "launcher-socket", "", "private unsandboxed runtime launcher socket")
		f.StringVar(&o.Binary, "binary", "", "replacement executable")
		f.DurationVar(&o.MaxWait, "max-wait", 300*time.Second, "bounded wait")
		if name == "maintenance start" {
			break
		}
		f.StringVar(&o.Note, "note", "", "maintenance reason")
		f.StringVar(&o.GatedRef, "gated-ref", "", "verified refs/fae/gated reference for changed builds")
		f.DurationVar(&o.OverlapWait, "overlap-wait", 2*time.Hour, "per-task overlap hold bound; protected tasks never expire")
		f.BoolVar(&o.AckOverlap, "ack-overlap", false, "explicitly acknowledge file overlap")
		f.BoolVar(&o.WhenClear, "when-clear", false, "also wait for QA stages")
		if name == "roll" {
			f.StringVar(&o.BuildRef, "build-ref", "", "compile a clean pinned gated source revision")
		}
	case "watchdog":
		f.StringVar(&revokeGrant, "revoke-grant", "", "revoke watchdog authority granted by this message ID")
		f.DurationVar(&period, "period", time.Minute, "watchdog period (at least 10ms)")
		f.DurationVar(&stall, "stall", 45*time.Minute, "watchdog transition stall bound")
		f.IntVar(&rearmBudget, "rearm-budget", controller.DefaultRearmBudget, "watchdog drive re-arms per cause per window")
		f.DurationVar(&rearmWindow, "rearm-window", controller.DefaultRearmWindow, "watchdog re-arm budget window")
		f.Float64Var(&fsThreshold, "fs-threshold", controller.DefaultFilesystemThreshold, "watchdog filesystem alert at this used fraction of bytes or inodes (0..1)")
	case "runtime-launcher":
		f.StringVar(&socket, "socket", "", "private runtime launcher socket")
	case "maintenance status", "drive stop", "drive rearm", "drive status":
	default:
		if verb == "drive" {
			return usage("drive requires stop, rearm or status")
		}
		return usage("maintenance requires stop, start or status")
	}
	fallBack := configurableFlags(f, controllerDefaults)
	if e := parse(f, args); e != nil {
		return e
	}
	fallBack()
	o.Message = opts.message
	// --period has no default in the spec, so it keeps its own check: a
	// nonpositive period is a usage error, not a silent fallback to 1 minute,
	// and so is one below controller.MinWatchdogPeriod, which
	// controller.Watchdog.Run refuses too.
	if verb == "watchdog" && period <= 0 {
		return usage("watchdog --period must be a positive duration, not %s", period)
	}
	if verb == "watchdog" && period < controller.MinWatchdogPeriod {
		return usage("watchdog --period must be at least %s, not %s", controller.MinWatchdogPeriod, period)
	}
	if verb == "runtime-launcher" && !filepath.IsAbs(socket) {
		return usage("runtime-launcher requires an absolute --socket")
	}
	if verb == "roll" && (o.Binary == "") == (o.BuildRef == "") {
		return usage("roll requires exactly one of --binary or --build-ref")
	}
	if (name == "roll" || name == "maintenance stop") && (o.Note == "" || !filepath.IsAbs(o.Launcher)) {
		return usage("%s requires --note and an absolute --launcher-socket", name)
	}
	if name == "maintenance start" && o.Launcher != "" && !filepath.IsAbs(o.Launcher) {
		return usage("maintenance start --launcher-socket must be absolute")
	}
	if verb == "roll" || verb == "watchdog" || action == "stop" || action == "start" || action == "rearm" {
		if e := requireMessage(name, opts); e != nil {
			return e
		}
	}
	layout, e := daemon.Paths(opts.home, opts.instance)
	if e != nil {
		return e
	}
	markerPath := filepath.Join(layout.Root, "controller-maintenance.json")
	if verb == "runtime-launcher" {
		// The launcher serves one instance: it starts only what that
		// instance's maintenance marker authorized (FAE-SEC-001).
		return controller.ServeLauncher(ctx, socket, markerPath)
	}
	if verb == "watchdog" {
		// A never-started instance has no credential: unreachable (exit 3),
		// and nothing is written under its root.
		if _, e = readCredential(layout.Credential); e != nil {
			return dialError(e)
		}
	}
	call := func(ctx context.Context, p controller.Params) (controller.Health, error) {
		token, e := readCredential(layout.Credential)
		if e != nil {
			return controller.Health{}, dialError(e)
		}
		raw, _ := json.Marshal(p)
		response, e := api.Call(ctx, layout.Socket, token, api.Request{Version: api.Version, Method: "Controller", Params: raw})
		if e != nil {
			return controller.Health{}, dialError(e)
		}
		body, _ := json.Marshal(response.Result)
		var h controller.Health
		e = json.Unmarshal(body, &h)
		return h, e
	}
	if verb == "drive" {
		mapped := map[string]string{"stop": "stop-drive", "rearm": "rearm", "status": "health"}[action]
		h, err := call(ctx, controller.Params{Action: mapped, AuthorizingMessage: opts.message})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(h)
	}
	client := controllerClient(layout, call)
	if verb == "watchdog" {
		if revokeGrant != "" {
			h, e := call(ctx, controller.Params{Action: "watchdog-revoke", AuthorizingMessage: opts.message, GrantMessage: revokeGrant})
			if e != nil {
				return e
			}
			return json.NewEncoder(out).Encode(h)
		}
		// The instance and temporary filesystems, the RAM-backed one, and the
		// bound workspace's, which health also names (FAE-CTL-020).
		paths := []string{layout.Root, os.TempDir()}
		if info, e := os.Stat("/dev/shm"); e == nil && info.IsDir() {
			paths = append(paths, "/dev/shm")
		}
		var binding struct {
			Workspace string `json:"workspace"`
		}
		if raw, e := os.ReadFile(layout.Binding); e == nil && json.Unmarshal(raw, &binding) == nil && binding.Workspace != "" {
			paths = append(paths, binding.Workspace)
		}
		w := controller.Watchdog{RPC: call, MarkerPath: client.MarkerPath, StatePath: filepath.Join(layout.Run, "watchdog-state.json"), Message: opts.message, Period: period, Stall: stall, Paths: paths, Output: out, RearmBudget: rearmBudget, RearmWindow: rearmWindow, FilesystemThreshold: fsThreshold, JSON: opts.json, Instance: opts.instance}
		return w.Run(ctx)
	}
	if verb == "roll" {
		h, e := runRoll(ctx, client, o)
		if e != nil {
			return e
		}
		return jsonOut(out, map[string]any{"pid": h.PID, "old_pid": h.OldPID, "new_pid": h.PID, "binary_sha256": h.BinaryHash, "old_binary_sha256": h.OldBinaryHash, "old_version": h.OldVersion, "new_version": h.NewVersion, "accepted": h.Consistent, "drive_rounds": h.DriveRounds, "holds": h.Holds, "watchdog": h.Watchdog})
	}
	switch action {
	case "status":
		m, e := client.Status(ctx)
		if e != nil {
			return e
		}
		if m == nil {
			// Without a marker the answer depends on a reachable daemon.
			if _, e = call(ctx, controller.Params{Action: "health"}); e != nil {
				return e
			}
			return jsonOut(out, map[string]bool{"maintenance": false})
		}
		return jsonOut(out, map[string]any{"id": m.ID, "owner": m.Owner, "reason": m.Reason, "state": m.State, "next_action": m.NextAction, "started": m.Started})
	case "stop":
		r, e := runControllerOperation(ctx, client, "stop", o)
		if e != nil {
			return e
		}
		m := r.Marker
		if m == nil {
			return fmt.Errorf("controller stop returned no maintenance marker")
		}
		return jsonOut(out, map[string]any{"id": m.ID, "old_pid": m.OldPID, "state": "stopped"})
	case "start":
		r, e := runControllerOperation(ctx, client, "start", o)
		if e != nil {
			return e
		}
		h := r.Health
		return jsonOut(out, map[string]any{"pid": h.PID, "old_pid": h.OldPID, "new_pid": h.PID, "binary_sha256": h.BinaryHash, "accepted": h.Consistent, "drive_rounds": h.DriveRounds})
	default:
		return usage("maintenance requires stop, start or status")
	}
}
