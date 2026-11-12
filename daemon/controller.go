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
	if d.live != nil {
		h.HostLedger = d.live.config.HostLedger
	}
	h.DriveGeneration = d.driveGeneration.Load()
	h.DriveCause, _ = d.driveCause.Load().(string)
	if d.pendingCheckpoint != nil || d.loopCheckpoint != nil || d.backlogUncertain {
		h.Consistent = false
	}
	var markerErr error
	h.Maintenance, markerErr = d.maintenanceMarker()
	if _, uncertain := d.maintenanceHeld(); markerErr != nil || uncertain != nil {
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
		row := d.healthRowOf(id, snapshot)
		if !row.valid {
			h.Consistent = false
			continue
		}
		if row.allowed {
			h.InFlight++
		}
		if row.unsafe {
			h.Unsafe = append(h.Unsafe, id)
		}
		if row.qa {
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
		for id, snapshot := range d.jobs {
			if d.healthRowOf(id, snapshot).inFlight && !d.active[snapshot.ID] {
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
	if d.landingActive() {
		h.InFlight++
		h.Unsafe = append(h.Unsafe, "landing")
	}
	return h
}

// faultAt is a test seam: a fault injected at a named point, or nil.
func (d *Daemon) faultAt(point, id string) error {
	if d.testFault == nil {
		return nil
	}
	return d.testFault(point, id)
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
		if e := d.sealMarker(m); e != nil {
			return e
		}
		body, _ := json.Marshal(m)
		e := d.faultAt("maintenance-commit", kind)
		if e == nil {
			e = d.store.ControllerAction(ctx, audit, body, alert)
		}
		if e != nil {
			d.maintenanceUncertain(m, e)
			return e
		}
		// The gate follows the committed plane marker at once (FAE-GEN-011);
		// a failed mirror write below cannot reopen it.
		if e = d.adoptMaintenance(body); e != nil {
			return e
		}
		if m != nil {
			e := d.faultAt("maintenance-mirror", kind)
			if e == nil {
				e = writePrivate(path, body)
			}
			if e != nil {
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
		m := &controller.Marker{ID: fmt.Sprintf("maintenance-%d", time.Now().UnixNano()), Owner: "operator", Reason: p.Note, Message: p.AuthorizingMessage, State: "preflight-failed", NextAction: "daemon remains running; inspect failure and retry authorized roll or maintenance stop", Started: time.Now().UTC(), OldPID: os.Getpid(), OldBirth: birth, Launcher: launcher, Launch: controller.LaunchSpec{Binary: executable, Arguments: RelaunchArgs(os.Args[1:]), Directory: d.config.Workspace, EnvironmentDigest: controller.EnvironmentHash(os.Environ()), SHA256: hash}}
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
		m := &controller.Marker{ID: fmt.Sprintf("maintenance-%d", time.Now().UnixNano()), Owner: "operator", Reason: p.Note, Message: p.AuthorizingMessage, State: "draining", NextAction: "wait for graceful stop; start with the same authorization", Started: time.Now().UTC(), OldPID: os.Getpid(), Launcher: p.Marker.Launcher, AckOverlap: p.AckOverlap, Launch: controller.LaunchSpec{Binary: executable, Arguments: RelaunchArgs(os.Args[1:]), Directory: d.config.Workspace, Environment: os.Environ(), SHA256: hash}}
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
		if h.BinaryHash != authorized.SHA256 || h.EnvironmentHash != authorized.Digest() || h.ProcessBirth == h.Maintenance.OldBirth && h.PID == h.Maintenance.OldPID {
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
			if e := fastForwardDeployed(ctx, d.config.Workspace, h.Maintenance, record); e != nil {
				return nil, e
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
		// The relaunch consumed its sealed environment; a failed removal is
		// retried by the next startup sweep and must not fail the acceptance.
		if e = controller.DiscardEnvironments(d.layout.Root); e != nil && d.log != nil {
			fmt.Fprintf(d.log, "controller launch environment removal deferred: %v\n", e)
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
	if e = d.verifyGatedTip(ctx, gated, tip); e != nil {
		return e
	}
	changed, e := rollChangedNames(ctx, d.config.Workspace, old, next)
	if e != nil {
		return e
	}
	files := map[string]bool{}
	for _, name := range changed {
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
		touched, e := rollChangedNames(ctx, binding.Path, env.Job.Base)
		untracked, ue := rollNulNames(controller.Git(ctx, binding.Path, "ls-files", "--others", "--exclude-standard", "-z"))
		if e != nil || ue != nil {
			h.Overlap = append(h.Overlap, id)
			h.Holds = append(h.Holds, controller.OverlapHold{Task: id, QueryFailed: true})
			continue
		}
		for _, name := range append(touched, untracked...) {
			if files[name] {
				h.Overlap = append(h.Overlap, id)
				break
			}
		}
	}
	return nil
}

// rollChangedNames lists the paths git diff names between the given
// revisions (or between one and the worktree), for the roll's overlap guard
// (FAE-CTL-001). A rename or copy is listed as both its paths, since a task
// editing the old one overlaps the change (--no-renames), and the names are
// NUL-separated, so a path git would otherwise quote is listed as itself.
func rollChangedNames(ctx context.Context, dir string, revisions ...string) ([]string, error) {
	return rollNulNames(controller.Git(ctx, dir, append([]string{"diff", "--name-only", "-z", "--no-renames"}, revisions...)...))
}

// rollNulNames splits git's NUL-separated output into names, passing a git
// error through.
func rollNulNames(out string, err error) ([]string, error) {
	if err != nil {
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(out, "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// sealMarker keeps launch environment values out of the plane, events and the
// marker mirror (FAE-SEC-003): they move to an owner-only file in the instance
// root that the offline start resolves by digest.
func (d *Daemon) sealMarker(m *controller.Marker) error {
	if m == nil {
		return nil
	}
	if e := controller.SealEnvironment(d.layout.Root, &m.Launch); e != nil {
		return e
	}
	if m.Replacement != nil {
		return controller.SealEnvironment(d.layout.Root, m.Replacement)
	}
	return nil
}

// Recover the atomic plane checkpoint before any drive round. The filesystem
// mirror may be absent or partial after a crash; it never overrides the plane.
// Sealed launch environments outlive only the maintenance that references them.
func (d *Daemon) restoreControllerMarker(ctx context.Context) error {
	var keep []string
	e := d.restoreControllerMarkerMirror(ctx, &keep)
	if e == nil {
		e = controller.DiscardEnvironments(d.layout.Root, keep...)
	}
	return e
}
func (d *Daemon) restoreControllerMarkerMirror(ctx context.Context, keep *[]string) error {
	raw, e := d.store.GetMarker(ctx, "controller-maintenance")
	if errors.Is(e, sql.ErrNoRows) {
		raw, e = nil, nil
	}
	if e != nil {
		return e
	}
	if e = d.adoptMaintenance(raw); e != nil {
		return e
	}
	marker, _ := decodeMaintenance(raw)
	if marker == nil {
		if _, stat := os.Lstat(d.controllerMarkerPath()); stat == nil {
			return errors.New("maintenance mirror lacks durable plane authorization")
		} else if !os.IsNotExist(stat) {
			return stat
		}
		return nil
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
	*keep = append(*keep, marker.Launch.EnvironmentDigest)
	if marker.Replacement != nil {
		*keep = append(*keep, marker.Replacement.EnvironmentDigest)
	}
	d.draining = marker.State != "preflight-failed"
	return nil
}

func (d *Daemon) publicMaintenance() map[string]any {
	marker, e := d.maintenanceMarker()
	if _, uncertain := d.maintenanceHeld(); e == nil && uncertain != nil && marker == nil {
		return map[string]any{"state": "uncertain", "next_action": "retry the maintenance action or restart to read the plane marker"}
	}
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
