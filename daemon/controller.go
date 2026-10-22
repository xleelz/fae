package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/workflow"
)

func (d *Daemon) controllerMarkerPath() string {
	return filepath.Join(d.layout.Root, "controller-maintenance.json")
}
func (d *Daemon) controllerHealth() controller.Health { return d.captureControllerHealth(true) }
func (d *Daemon) captureControllerHealth(verifyExecutable bool) controller.Health {
	h := controller.Health{Workspace: d.config.Workspace, PID: os.Getpid(), DoltPID: d.child.PID(), DriveAlive: d.driveAlive.Load(), DriveRounds: d.driveRounds.Load(), LastTransition: time.Unix(0, d.lastTransition.Load()).UTC(), EnvironmentHash: controller.EnvironmentHash(os.Environ()), Consistent: true}
	h.InFlight = len(d.prophetActive)
	h.DriveGeneration = d.driveGeneration.Load()
	h.DriveCause, _ = d.driveCause.Load().(string)
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil || d.backlogUncertain {
		h.Consistent = false
	}
	var markerErr error
	h.Maintenance, markerErr = controller.ReadMarker(d.controllerMarkerPath())
	if markerErr != nil {
		h.Consistent = false
	}
	h.ProcessBirth, _ = processowner.Birth(os.Getpid())
	h.BinaryHash = d.binaryHash
	if verifyExecutable {
		if _, _, e := d.deploymentExecutable(); e != nil {
			h.Consistent = false
		}
	}
	for id, snapshot := range d.jobs {
		var e envelope
		if decodeEnvelope(snapshot.Payload, &e) != nil || engine.Validate(e.Job) != nil {
			h.Consistent = false
			continue
		}
		stage, allowed := e.Job.Document.Stages[string(e.Job.State)]
		if allowed {
			h.InFlight++
		}
		unsafe := allowed && maintenanceEffect(stage.Primitive)
		if e.Job.InFlight != nil {
			unsafe = unsafe || maintenanceEffect(e.Job.InFlight.Config.Primitive)
		}
		if unsafe {
			h.Unsafe = append(h.Unsafe, id)
		}
		if allowed && (strings.EqualFold(stage.Role, "qa") || strings.Contains(strings.ToLower(string(e.Job.State)), "qa")) {
			h.QA = append(h.QA, id)
		}
	}
	// Drain is about live or unresolved turns, not all runnable molecule states.
	if d.draining {
		h.InFlight = len(d.active) + len(d.loopActive) + len(d.prophetActive)
		for id, schedule := range d.loops {
			if schedule.InFlight {
				if _, active := d.loopActive[id]; !active {
					h.InFlight++
				}
			}
		}
		for _, snapshot := range d.jobs {
			var e envelope
			if decodeEnvelope(snapshot.Payload, &e) == nil && e.Job.InFlight != nil && !d.active[snapshot.ID] {
				h.InFlight++
			}
		}
	}
	if d.ticketActive.Load() {
		h.InFlight++
		h.Unsafe = append(h.Unsafe, "ticket-outbox")
	}
	if v := d.ticketStatus.Load(); v != nil && v.(ticketStatus).Uncertain > 0 {
		h.InFlight += v.(ticketStatus).Uncertain
		h.Unsafe = append(h.Unsafe, "ticket-outbox-uncertain")
	}
	if d.backlogUncertain {
		h.InFlight++
		h.Unsafe = append(h.Unsafe, "backlog-commit-uncertain")
	}
	return h
}
func (d *Daemon) controllerAction(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p controller.Params
	if e := decode(raw, &p); e != nil {
		return nil, e
	}
	h := d.controllerHealth()
	path := d.controllerMarkerPath()
	record := func(kind string, m *controller.Marker, alert bool) error {
		audit.Action = kind
		body, _ := json.Marshal(m)
		if e := d.store.ControllerAction(ctx, audit, body, alert); e != nil {
			return e
		}
		if m != nil {
			if e := writePrivate(path, body); e != nil {
				return e
			}
		}
		d.notifyEvents()
		return nil
	}
	switch p.Action {
	case "watchdog-register", "watchdog-revoke":
		if e := d.watchdogGrantAction(ctx, p, audit); e != nil {
			return nil, e
		}
		return h, nil
	case "health":
		return h, nil
	case "preflight-failed":
		if h.Maintenance != nil && (h.Maintenance.State != "preflight-failed" || h.Maintenance.Message != p.AuthorizingMessage) {
			return nil, errors.New("existing maintenance retained")
		}
		executable, hash := d.binaryPath, d.binaryHash
		// A refusal records only the original startup hash, never authorizes
		// replacement bytes now found at the old executable pathname.
		if _, _, identityErr := d.deploymentExecutable(); identityErr != nil {
			p.Note += "; " + identityErr.Error()
		}
		birth, e := processowner.Birth(os.Getpid())
		if e != nil {
			return nil, e
		}
		launcher := ""
		if p.Marker != nil {
			launcher = p.Marker.Launcher
		}
		m := &controller.Marker{ID: fmt.Sprintf("maintenance-%d", time.Now().UnixNano()), Owner: "operator", Reason: p.Note, Message: p.AuthorizingMessage, State: "preflight-failed", NextAction: "daemon remains running; inspect failure and retry authorized roll or maintenance stop", Started: time.Now().UTC(), OldPID: os.Getpid(), OldBirth: birth, Launcher: launcher, Launch: controller.LaunchSpec{Binary: executable, Arguments: append([]string(nil), os.Args[1:]...), Directory: d.config.Workspace, Environment: os.Environ(), SHA256: hash}}
		if e = record("controller-maintenance-preflight-failed", m, true); e != nil {
			return nil, e
		}
		d.publishStatus()
		return d.controllerHealth(), nil
	case "prepare":
		if p.Note == "" || p.Marker == nil || !filepath.IsAbs(p.Marker.Launcher) {
			return nil, errors.New("maintenance note and launcher required")
		}
		if h.Maintenance != nil {
			if h.Maintenance.Message != p.AuthorizingMessage {
				return nil, errors.New("maintenance is owned by another authorization")
			}
			if h.Maintenance.State != "preflight-failed" {
				return h, nil
			}
			h.Maintenance = nil
		}
		executable, hash, e := d.deploymentExecutable()
		if e != nil {
			return nil, e
		}
		if !h.Consistent {
			return nil, errors.New("plane consistency check failed")
		}
		if p.Binary != "" {
			newHash, e := controller.HashFile(p.Binary)
			if e != nil {
				return nil, e
			}
			if newHash != hash || p.WhenClear {
				if newHash != hash && p.Binary == executable {
					return nil, errors.New("replacement must preserve old binary path for rollback")
				}
				if e = d.rollOverlap(ctx, p.Binary, p.GatedRef, &h); e != nil {
					return nil, fmt.Errorf("gated source proof unavailable: %w", e)
				}
			}
		}
		if err := d.deploymentHolds(ctx, p, &h, audit); err != nil {
			return nil, err
		}
		if !controller.CanEnterMaintenance(len(h.Unsafe) == 0, p.WhenClear, len(h.QA) == 0, len(h.Overlap) == 0, p.AckOverlap) {
			return h, nil
		}
		m := &controller.Marker{ID: fmt.Sprintf("maintenance-%d", time.Now().UnixNano()), Owner: "operator", Reason: p.Note, Message: p.AuthorizingMessage, State: "draining", NextAction: "wait for graceful stop; start with the same authorization", Started: time.Now().UTC(), OldPID: os.Getpid(), Launcher: p.Marker.Launcher, AckOverlap: p.AckOverlap, Launch: controller.LaunchSpec{Binary: executable, Arguments: append([]string(nil), os.Args[1:]...), Directory: d.config.Workspace, Environment: os.Environ(), SHA256: hash}}
		m.OldBirth, e = processowner.Birth(os.Getpid())
		if e != nil {
			return nil, e
		}
		if p.Binary != "" {
			target := m.Launch
			target.Binary = p.Binary
			target.SHA256, e = controller.HashFile(p.Binary)
			if e != nil {
				return nil, e
			}
			m.Replacement = &target
			m.GatedRef = p.GatedRef
			m.Holds = h.Holds
			if target.SHA256 != hash {
				m.DeployedTip, e = controller.SourceRevision(p.Binary)
				if e != nil {
					return nil, e
				}
				if d.live == nil {
					return nil, errors.New("deployment requires configured integration reference")
				}
				m.IntegrationRef = d.live.config.IntegrationRef
			}
		}
		if e = record("controller-maintenance-prepared", m, false); e != nil {
			return nil, e
		}
		d.draining = true
		d.publishStatus()
		h = d.controllerHealth()
		return h, nil
	case "stop":
		if h.Maintenance == nil || h.Maintenance.Message != p.AuthorizingMessage {
			return nil, errors.New("matching maintenance authorization required")
		}
		// The authorized drain may checkpoint a preceding turn at a queued
		// effect stage. Only unresolved/active dispatches block finishing it.
		if h.InFlight != 0 {
			return nil, errors.New("graceful stop still draining")
		}
		m := h.Maintenance
		m.State = "stopped"
		m.NextAction = "maintenance start through authorized runtime launcher"
		if e := record("controller-maintenance-stopped", m, false); e != nil {
			return nil, e
		}
		d.cancel()
		return h, nil
	case "accept":
		if h.Maintenance == nil || h.Maintenance.Message != p.AuthorizingMessage {
			return nil, errors.New("matching maintenance authorization required")
		}
		if p.Marker == nil || p.Marker.ID != h.Maintenance.ID {
			return nil, errors.New("maintenance identity mismatch")
		}
		authorized := h.Maintenance.Launch
		if h.Maintenance.Replacement != nil && h.BinaryHash == h.Maintenance.Replacement.SHA256 {
			authorized = *h.Maintenance.Replacement
		}
		if h.BinaryHash != authorized.SHA256 || h.EnvironmentHash != controller.EnvironmentHash(authorized.Environment) || h.ProcessBirth == h.Maintenance.OldBirth && h.PID == h.Maintenance.OldPID {
			return nil, errors.New("daemon binary, environment or replacement identity does not match authorization")
		}
		if !h.DriveAlive || h.DriveRounds == 0 || !h.Consistent {
			return nil, errors.New("post-start acceptance incomplete")
		}
		safe, e := controller.Unsandboxed(os.Getpid())
		childSafe, childErr := controller.Unsandboxed(d.child.PID())
		if e != nil || childErr != nil || !safe || !childSafe {
			return nil, errors.Join(e, childErr, errors.New("daemon/Dolt native sandbox acceptance failed"))
		}
		if h.Maintenance.DeployedTip != "" && h.BinaryHash != h.Maintenance.Launch.SHA256 {
			if e := controller.FastForward(ctx, d.config.Workspace, h.Maintenance.IntegrationRef, h.Maintenance.DeployedTip); e != nil {
				m := h.Maintenance
				m.State = "failed"
				m.NextAction = "restore previous binary; reconcile divergent integration reference"
				return nil, errors.Join(e, record("controller-maintenance-deployment-rejected", m, true))
			}
		}
		accepted := *h.Maintenance
		accepted.State = "accepted"
		accepted.NextAction = "none"
		if e = record("controller-maintenance-accepted", &accepted, false); e != nil {
			return nil, e
		}
		if e = os.Remove(path); e != nil {
			return nil, e
		}
		d.draining = false
		d.publishStatus()
		d.signal()
		return d.controllerHealth(), nil
	case "abort":
		m := h.Maintenance
		if m == nil {
			return nil, errors.New("maintenance marker required")
		}
		m.State = "failed"
		m.NextAction = p.Note
		if e := record("controller-maintenance-failed", m, true); e != nil {
			return nil, e
		}
		return d.controllerHealth(), nil
	case "alert":
		m := h.Maintenance
		if m != nil && m.State != "preflight-failed" {
			return h, nil
		}
		audit.Action = "controller-watchdog-alert"
		body, _ := json.Marshal(map[string]string{"cause": p.Cause, "detail": p.Note})
		if e := d.store.ControllerAction(ctx, audit, body, true); e != nil {
			return nil, e
		}
		d.notifyEvents()
		return h, nil
	case "stop-drive":
		if h.Maintenance != nil && h.Maintenance.State != "preflight-failed" {
			return nil, errors.New("drive stop unavailable during maintenance")
		}
		audit.Action = "controller-drive-stop"
		body, _ := json.Marshal(map[string]string{"cause": "operator-stop"})
		if err := d.store.ControllerAction(ctx, audit, body, false); err != nil {
			return nil, err
		}
		d.driveStop.Store(true)
		d.signal()
		return d.controllerHealth(), nil
	case "rearm":
		if h.Maintenance != nil && h.Maintenance.State != "preflight-failed" {
			return nil, errors.New("drive rearm suppressed during maintenance")
		}
		if h.DriveAlive {
			audit.Action = "controller-watchdog-rearm-noop"
			if err := d.store.ControllerAction(ctx, audit, json.RawMessage(`{"alive":true}`), false); err != nil {
				return nil, err
			}
			return h, nil
		}
		if err := d.requestDriveRearm(ctx, audit); err != nil {
			return nil, err
		}
		return d.controllerHealth(), nil
	default:
		return nil, errors.New("unknown controller action")
	}
}
func (d *Daemon) rollOverlap(ctx context.Context, binary, gated string, h *controller.Health) error {
	if !strings.HasPrefix(gated, "refs/fae/gated/") {
		return errors.New("changed binary requires explicit refs/fae/gated reference")
	}
	executable, _, e := d.deploymentExecutable()
	if e != nil {
		return e
	}
	old, e := controller.SourceRevision(executable)
	if e != nil {
		return e
	}
	next, e := controller.SourceRevision(binary)
	if e != nil {
		return e
	}
	git := func(args ...string) (string, error) { return controller.Git(ctx, d.config.Workspace, args...) }
	tip, e := git("rev-parse", "--verify", gated+"^{commit}")
	if e != nil {
		return e
	}
	if tip != next {
		return errors.New("new binary differs from gated source revision")
	}
	changed, e := git("diff", "--name-only", old, next)
	if e != nil {
		return e
	}
	files := map[string]bool{}
	for _, name := range strings.Split(changed, "\n") {
		files[name] = true
	}
	for id, snapshot := range d.jobs {
		var env envelope
		if decodeEnvelope(snapshot.Payload, &env) != nil {
			return errors.New("unreadable task")
		}
		if _, runnable := env.Job.Document.Stages[string(env.Job.State)]; !runnable {
			continue
		}
		if d.live == nil {
			h.Overlap = append(h.Overlap, id)
			h.Holds = append(h.Holds, controller.OverlapHold{Task: id, QueryFailed: true})
			continue
		}
		binding, e := d.live.manager.Ensure(ctx, env.Admission.RunID, id, d.live.config.Base)
		if e != nil {
			h.Overlap = append(h.Overlap, id)
			h.Holds = append(h.Holds, controller.OverlapHold{Task: id, QueryFailed: true})
			continue
		}
		touchedString, e := controller.Git(ctx, binding.Path, "diff", "--name-only", env.Job.Base)
		touched := []byte(touchedString)
		untracked, ue := controller.Git(ctx, binding.Path, "ls-files", "--others", "--exclude-standard")
		if e != nil || ue != nil {
			h.Overlap = append(h.Overlap, id)
			h.Holds = append(h.Holds, controller.OverlapHold{Task: id, QueryFailed: true})
			continue
		}
		touched = append(touched, []byte("\n"+untracked)...)
		for _, name := range strings.Split(strings.TrimSpace(string(touched)), "\n") {
			if files[name] {
				h.Overlap = append(h.Overlap, id)
				break
			}
		}
	}
	return nil
}

// Recover the atomic plane checkpoint before any drive round. The filesystem
// mirror may be absent or partial after a crash; it never overrides the plane.
func (d *Daemon) restoreControllerMarker(ctx context.Context) error {
	raw, e := d.store.GetMarker(ctx, "controller-maintenance")
	if errors.Is(e, sql.ErrNoRows) {
		if _, stat := os.Lstat(d.controllerMarkerPath()); stat == nil {
			return errors.New("maintenance mirror lacks durable plane authorization")
		} else if !os.IsNotExist(stat) {
			return stat
		}
		return nil
	}
	if e != nil {
		return e
	}
	var marker *controller.Marker
	if e = json.Unmarshal(raw, &marker); e != nil {
		return e
	}
	if marker == nil {
		if _, e := os.Lstat(d.controllerMarkerPath()); e == nil {
			return errors.New("maintenance mirror lacks durable plane authorization")
		} else if !os.IsNotExist(e) {
			return e
		}
		return nil
	}
	if marker.ID == "" || marker.Message == "" {
		return errors.New("invalid durable maintenance marker")
	}
	if marker.State == "accepted" {
		existing, e := controller.ReadMarker(d.controllerMarkerPath())
		if e != nil {
			return e
		}
		if existing != nil {
			if existing.ID != marker.ID {
				return errors.New("maintenance acceptance identity mismatch")
			}
			if e = os.Remove(d.controllerMarkerPath()); e != nil {
				return e
			}
		}
		return nil
	}
	if e = writePrivate(d.controllerMarkerPath(), raw); e != nil {
		return e
	}
	d.draining = marker.State != "preflight-failed"
	return nil
}

func (d *Daemon) publicMaintenance() map[string]any {
	marker, e := controller.ReadMarker(d.controllerMarkerPath())
	if e != nil {
		return map[string]any{"state": "unreadable", "next_action": "inspect private maintenance marker"}
	}
	if marker == nil {
		return nil
	}
	return map[string]any{"id": marker.ID, "owner": marker.Owner, "reason": marker.Reason, "state": marker.State, "started": marker.Started, "next_action": marker.NextAction}
}

func maintenanceEffect(primitive workflow.Primitive) bool {
	return primitive == workflow.VcsCommit || primitive == workflow.Integrate || primitive == workflow.ExternalEffect
}
