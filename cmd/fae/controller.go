package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
	"io"
	"os"
	"path/filepath"
	"time"
)

func controllerCommand(ctx context.Context, verb string, args []string, c common, out io.Writer) error {
	action := ""
	if verb == "maintenance" || verb == "drive" {
		if len(args) == 0 {
			return usage("maintenance requires stop, start or status")
		}
		action, args = args[0], args[1:]
	}
	f, opts := flags(verb, c)
	launcher := f.String("launcher-socket", "", "private unsandboxed runtime launcher socket")
	socket := f.String("socket", "", "private runtime launcher socket")
	note := f.String("note", "", "maintenance reason")
	binary := f.String("binary", "", "replacement executable")
	buildRef := f.String("build-ref", "", "compile a clean pinned gated source revision")
	overlapWait := f.Duration("overlap-wait", 2*time.Hour, "per-task overlap hold bound; protected tasks never expire")
	gated := f.String("gated-ref", "", "verified refs/fae/gated reference for changed builds")
	max := f.Duration("max-wait", 300*time.Second, "bounded wait")
	ack := f.Bool("ack-overlap", false, "explicitly acknowledge file overlap")
	clear := f.Bool("when-clear", false, "also wait for QA stages")
	revokeGrant := f.String("revoke-grant", "", "revoke watchdog authority granted by this message ID")
	period := f.Duration("period", time.Minute, "watchdog period")
	stall := f.Duration("stall", 45*time.Minute, "watchdog transition stall bound")
	if e := parse(f, args); e != nil {
		return e
	}
	if verb == "runtime-launcher" {
		return controller.ServeLauncher(ctx, *socket)
	}
	layout, e := daemon.Paths(opts.home, opts.instance)
	if e != nil {
		return e
	}
	call := func(ctx context.Context, p controller.Params) (controller.Health, error) {
		token, e := readCredential(layout.Credential)
		if e != nil {
			return controller.Health{}, e
		}
		raw, _ := json.Marshal(p)
		key, e := identity("controller-")
		if e != nil {
			return controller.Health{}, e
		}
		response, e := api.Call(ctx, layout.Socket, token, api.Request{Version: api.Version, Method: "Controller", Key: key, Params: raw})
		if e != nil {
			return controller.Health{}, e
		}
		body, _ := json.Marshal(response.Result)
		var h controller.Health
		e = json.Unmarshal(body, &h)
		return h, e
	}
	if verb == "drive" {
		mapped := map[string]string{"stop": "stop-drive", "rearm": "rearm", "status": "health"}[action]
		if mapped == "" {
			return usage("drive requires stop, rearm or status")
		}
		h, err := call(ctx, controller.Params{Action: mapped, AuthorizingMessage: opts.message})
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(h)
	}
	client := controller.Client{RPC: call, MarkerPath: filepath.Join(layout.Root, "controller-maintenance.json")}
	o := controller.Options{Message: opts.message, Note: *note, Launcher: *launcher, Binary: *binary, GatedRef: *gated, BuildRef: *buildRef, OverlapWait: *overlapWait, MaxWait: *max, AckOverlap: *ack, WhenClear: *clear}
	if verb == "watchdog" {
		if *revokeGrant != "" {
			h, e := call(ctx, controller.Params{Action: "watchdog-revoke", AuthorizingMessage: opts.message, GrantMessage: *revokeGrant})
			if e != nil {
				return e
			}
			return json.NewEncoder(out).Encode(h)
		}
		w := controller.Watchdog{RPC: call, MarkerPath: client.MarkerPath, StatePath: filepath.Join(layout.Run, "watchdog-state.json"), Message: opts.message, Period: *period, Stall: *stall, Paths: []string{layout.Root, os.TempDir()}, Output: out}
		return w.Run(ctx)
	}
	if verb == "roll" {
		h, e := client.Roll(ctx, o)
		if e != nil {
			return e
		}
		return jsonOut(out, map[string]any{"pid": h.PID, "old_pid": h.OldPID, "new_pid": h.PID, "binary_sha256": h.BinaryHash, "accepted": h.Consistent, "drive_rounds": h.DriveRounds})
	}
	switch action {
	case "status":
		m, e := client.Status(ctx)
		if e != nil {
			return e
		}
		if m == nil {
			return jsonOut(out, map[string]bool{"maintenance": false})
		}
		return jsonOut(out, map[string]any{"id": m.ID, "owner": m.Owner, "reason": m.Reason, "state": m.State, "next_action": m.NextAction, "started": m.Started})
	case "stop":
		m, e := client.Stop(ctx, o)
		if e != nil {
			return e
		}
		return jsonOut(out, map[string]any{"id": m.ID, "old_pid": m.OldPID, "state": "stopped"})
	case "start":
		h, e := client.Start(ctx, o)
		if e != nil {
			return e
		}
		return jsonOut(out, map[string]any{"pid": h.PID, "old_pid": h.OldPID, "new_pid": h.PID, "binary_sha256": h.BinaryHash, "accepted": h.Consistent, "drive_rounds": h.DriveRounds})
	default:
		return errors.New("unknown maintenance action")
	}
}
