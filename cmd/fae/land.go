package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
)

// landClient is the daemon surface of fae land; tests substitute it.
type landClient struct {
	start  func(context.Context, daemon.LandParams) (daemon.LandingRecord, error)
	status func(context.Context, string) (daemon.LandingRecord, error)
	alert  func(ctx context.Context, cause, note string) error
	roll   func(context.Context, controller.Options) (controller.Health, error)
}

var newLandClient = func(c common) landClient {
	call := func(ctx context.Context, method string, params, result any) error {
		layout, e := daemon.Paths(c.home, c.instance)
		if e != nil {
			return e
		}
		token, e := readCredential(layout.Credential)
		if e != nil {
			if errors.Is(e, os.ErrNotExist) {
				return unreachable(e)
			}
			return e
		}
		raw, _ := json.Marshal(params)
		response, e := api.Call(ctx, layout.Socket, token, api.Request{Version: api.Version, Method: method, Params: raw})
		if e != nil {
			return dialError(e)
		}
		body, _ := json.Marshal(response.Result)
		return json.Unmarshal(body, result)
	}
	controllerRPC := func(ctx context.Context, p controller.Params) (controller.Health, error) {
		var h controller.Health
		return h, call(ctx, "Controller", p, &h)
	}
	return landClient{
		start: func(ctx context.Context, p daemon.LandParams) (r daemon.LandingRecord, e error) {
			return r, call(ctx, "Land", p, &r)
		},
		status: func(ctx context.Context, id string) (r daemon.LandingRecord, e error) {
			return r, call(ctx, "LandStatus", map[string]string{"id": id}, &r)
		},
		alert: func(ctx context.Context, cause, note string) error {
			_, e := controllerRPC(ctx, controller.Params{Action: "alert", AuthorizingMessage: c.message, Cause: cause, Note: note})
			return e
		},
		roll: func(ctx context.Context, o controller.Options) (controller.Health, error) {
			layout, e := daemon.Paths(c.home, c.instance)
			if e != nil {
				return controller.Health{}, e
			}
			return runRoll(ctx, controllerClient(layout, controllerRPC), o)
		},
	}
}

func splitList(s string) []string {
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// minLandPoll is the shortest land --poll accepted. --poll has no default in
// the spec, so a poll outside the range is refused, not replaced. Each poll
// is a LandStatus read (an IPC round trip, a read-pool slot and a plane
// marker read), which takes a few milliseconds: a shorter interval cannot be
// honoured, and the loop would issue those reads back to back for the whole
// landing. It is the watchdog's floor, so the CLI's two pollers share one.
const minLandPoll = controller.MinWatchdogPeriod

// landDefaults are the --then-roll flags that carry an Appendix A
// configurable default: the roll's clear-window wait (300 s) and overlap hold
// (2 h), as on fae roll. --poll has no such default, so a nonpositive poll,
// or one below minLandPoll, stays a usage error.
var landDefaults = map[string]valueRange{
	"roll-max-wait": positiveDuration,
	"overlap-wait":  positiveDuration,
}

// landCommand implements FAE-CTL-030..033: land, salvage, batch and then-roll.
func landCommand(ctx context.Context, args []string, c common, out io.Writer) error {
	f, opts := flags("land", c)
	task := f.String("task", "", "Done task to land (or the escalated task to salvage)")
	batch := f.String("batch", "", "comma-separated tasks landed in dependency order")
	target := f.String("target", "", "operator branch to land onto")
	salvage := f.Bool("salvage", false, "land the tree the task's engine runs recorded as passing")
	source := f.String("source-ref", "", "salvage source tree or commit")
	poll := f.Duration("poll", 2*time.Second, "landing status poll interval (at least 10ms)")
	thenRoll := f.Bool("then-roll", false, "after landing, roll to the gated tip when the QA window clears")
	launcher := f.String("launcher-socket", "", "runtime launcher socket for --then-roll")
	note := f.String("note", "", "roll note for --then-roll")
	rollWait := f.Duration("roll-max-wait", 300*time.Second, "bounded wait for the commit/QA-clear window (positive duration)")
	overlapWait := f.Duration("overlap-wait", 2*time.Hour, "per-task overlap hold bound for --then-roll (positive duration)")
	ack := f.Bool("ack-overlap", false, "acknowledge file overlap for --then-roll")
	fallBack := configurableFlags(f, landDefaults)
	if e := parse(f, args); e != nil {
		return e
	}
	fallBack()
	tasks := splitList(*batch)
	if *task != "" {
		tasks = append(tasks, *task)
	}
	if *target == "" || len(tasks) == 0 || (*task != "") == (*batch != "") || *salvage != (*source != "") || *salvage && *task == "" || *thenRoll && !filepath.IsAbs(*launcher) {
		return usage("land requires --target, exactly one of --task or --batch, --salvage with --source-ref, and --launcher-socket for --then-roll")
	}
	if *poll <= 0 {
		return usage("land --poll must be a positive duration, not %s", *poll)
	}
	if *poll < minLandPoll {
		return usage("land --poll must be at least %s, not %s", minLandPoll, *poll)
	}
	if e := requireMessage("land", opts); e != nil {
		return e
	}
	// Flakes and frozen checks come from each task's pinned target (FAE-CTL-030).
	p := daemon.LandParams{Tasks: tasks, Target: *target, Salvage: *salvage, SourceRef: *source, AuthorizingMessage: opts.message}
	client := newLandClient(*opts)
	rec, e := client.start(ctx, p)
	if e != nil {
		return e
	}
	for rec.State == "running" {
		select {
		case <-ctx.Done():
			return fmt.Errorf("landing %s still running; inspect with LandStatus: %w", rec.ID, ctx.Err())
		case <-time.After(*poll):
		}
		if rec, e = client.status(ctx, rec.ID); e != nil {
			return e
		}
	}
	result := map[string]any{"landing": rec}
	failed := []string{}
	for _, r := range rec.Results {
		if r.Outcome != controller.LandLanded {
			failed = append(failed, fmt.Sprintf("%s %s %s", r.Task, r.Outcome, strings.TrimSpace(r.FailingGate+" "+r.Reason)))
		}
	}
	if rec.State != "finished" || len(failed) > 0 || len(rec.Results) != len(rec.Tasks) || rec.Error != "" {
		if e := jsonOut(out, result); e != nil {
			return e
		}
		return &commandError{1, fmt.Errorf("landing %s %s: %s %s", rec.ID, rec.State, strings.Join(failed, "; "), rec.Error)}
	}
	if *thenRoll {
		if *note == "" {
			*note = "land " + rec.ID + " then roll"
		}
		h, rollErr := client.roll(ctx, controller.Options{Message: opts.message, Note: *note, Launcher: *launcher, BuildRef: rec.Gated, GatedRef: rec.Gated, WhenClear: true, MaxWait: *rollWait, OverlapWait: *overlapWait, AckOverlap: *ack})
		if rollErr != nil {
			result["roll"] = map[string]any{"error": rollErr.Error(), "exit_code": exitCode(rollErr)}
			alertErr := client.alert(context.WithoutCancel(ctx), "then-roll-failed", fmt.Sprintf("landing %s landed but roll to %s failed: %v", rec.ID, rec.Gated, rollErr))
			if e := jsonOut(out, result); e != nil {
				return e
			}
			return &commandError{exitCode(rollErr), errors.Join(fmt.Errorf("landing %s landed; roll failed: %w", rec.ID, rollErr), alertErr)}
		}
		result["roll"] = map[string]any{"old_pid": h.OldPID, "new_pid": h.PID, "binary_sha256": h.BinaryHash, "accepted": h.Consistent, "drive_rounds": h.DriveRounds, "holds": h.Holds}
	}
	return jsonOut(out, result)
}
