package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

type AdmitParams struct {
	maxSessionsSet bool
	Inputs         map[string]any    `json:"inputs,omitempty"`
	ParentTask     string            `json:"parent_task,omitempty"`
	FromParent     map[string]string `json:"from_parent,omitempty"`
	MaxSessions    int               `json:"max_sessions,omitempty"`

	Writes         []string `json:"writes,omitempty"`
	TargetConfig   string   `json:"target_config,omitempty"`
	TargetRevision int64    `json:"target_revision,omitempty"`
	TargetHash     string   `json:"target_hash,omitempty"`
	CommitExternal bool     `json:"commit_external,omitempty"`
	// DryRun states a Normal dry run; a Normal submission names exactly one
	// of DryRun and CommitExternal (FAE-CLI-011).
	DryRun             bool   `json:"dry_run,omitempty"`
	TicketRoom         string `json:"ticket_room,omitempty"`
	Destination        string `json:"destination,omitempty"`
	PushNamespace      string `json:"push_namespace,omitempty"`
	ID                 string `json:"id"`
	WorkflowID         string `json:"workflow_id"`
	Revision           int64  `json:"revision"`
	Tree               string `json:"tree"`
	Title              string `json:"title,omitempty"`
	Content            string `json:"content,omitempty"`
	Critical           bool   `json:"critical,omitempty"`
	Priority           int    `json:"priority,omitempty"`
	RunID              string `json:"run_id,omitempty"`
	Mode               string `json:"mode,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
	// TaskClass selects a declared task class; empty takes the default.
	TaskClass string `json:"task_class,omitempty"`
	// CarryFrom re-admits an inactive earlier task of the same item, carrying
	// its accepted pre-code evidence forward (FAE-WF-086).
	CarryFrom string `json:"carry_from,omitempty"`
	// Optional structured fields (FAE-CLI-014): the files the work declares,
	// its required test filter and test command shape, pinned at admission;
	// landing uses them in place of the target's defaults.
	DeclaredFiles []string               `json:"declared_files,omitempty"`
	ReadOnlyFiles []string               `json:"read_only_files,omitempty"`
	Reads         *workflow.ReadPolicy   `json:"reads,omitempty"`
	TestFilter    string                 `json:"test_filter,omitempty"`
	TestShape     string                 `json:"test_shape,omitempty"`
	FollowUpOf    string                 `json:"follow_up_of,omitempty"`
	TurnBudget    *engine.TurnBudgetSpec `json:"turn_budget,omitempty"`
}
type InstallParams struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name,omitempty"`
	Document           workflow.Document `json:"document"`
	AuthorizingMessage string            `json:"authorizing_message"`
}

func decode(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return errors.New("trailing parameters")
	}
	return nil
}
func validWorkflow(raw []byte) error {
	d, err := workflow.Parse(raw)
	if err != nil {
		return err
	}
	if issues := workflow.Validate(d); len(issues) > 0 {
		return fmt.Errorf("invalid workflow: %v", issues)
	}
	return nil
}

// validWorkflowWrite applies current authoring rules only to new revisions.
func validWorkflowWrite(raw []byte) error {
	_, err := workflow.ParseWrite(raw)
	return err
}
func (d *Daemon) handleDirect(ctx context.Context, r api.Request) (any, error) {
	principal, permission, verb, authErr := authorizePrincipal(ctx, r)
	if authErr != nil {
		return nil, authErr
	}
	if !permission.Read {
		// An early refusal needs no lock; the check repeats under d.mu below.
		if refusal := d.mutationRefusal(verb, false); refusal != nil {
			return nil, refusal
		}
	}
	if r.Method == "AdapterPut" {
		return d.adapterInstallRPC(ctx, r, principal)
	}
	if r.Method == "import_legacy" {
		return d.importLegacyRPC(ctx, r, principal)
	}
	if r.Method == "UIToken" {
		return d.uiTokenRPC(ctx, r, principal)
	}
	if r.Method == "AdapterGet" {
		var p struct {
			ID       string `json:"id"`
			Revision int64  `json:"revision"`
		}
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		return d.getAdapter(ctx, p.ID, p.Revision)
	}
	if r.Method == "AdapterList" {
		if err := decode(r.Params, &struct{}{}); err != nil {
			return nil, err
		}
		return d.store.ListAdapters(ctx)
	}
	if r.Method == "ReviewWorkspacePlan" || r.Method == "ReviewWorkspacePrepare" {
		return d.reviewWorkspaceRPC(ctx, r)
	}
	if r.Method == "DispatchPauseList" {
		return d.dispatchPauseList(ctx, r.Params)
	}
	if r.Method == "Census" {
		return d.census(ctx, r.Params)
	}
	if r.Method == "Land" || r.Method == "LandStatus" {
		return d.landRPC(ctx, r, principal)
	}
	if r.Method == "CensusApply" {
		return d.censusApply(ctx, r.Params)
	}
	if r.Method == "ScratchArchiveCleanup" {
		return d.scratchArchiveRPC(ctx, r)
	}
	if r.Method == "GatesSelfTest" {
		return gatesSelfTest(r.Params)
	}
	if r.Method == "ScratchCleanupAuthorize" {
		if principal.Actor != "operator" || principal.Task != "" {
			return nil, errors.New("scratch cleanup requires instance operator")
		}
		var p struct {
			AuthorizingMessage string `json:"authorizing_message"`
		}
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		identity, err := scratchIdentity(d.layout)
		if err != nil {
			return nil, err
		}
		if err = d.store.AuthorizeReadOnlyEffect(ctx, p.AuthorizingMessage, principal.Actor, "CleanupScratch", "runtime", actionClasses()); err != nil {
			return nil, err
		}
		return identity, nil
	}
	if r.Method == "BackupQuota" {
		return d.backupQuotaRPC(ctx, r)
	}
	if r.Method == "BackupReplicationUnpin" {
		return d.backupReplicationUnpinRPC(ctx, r)
	}
	if r.Method == "BackupSchedule" {
		return d.backupScheduleRPC(ctx, r)
	}
	if r.Method == "BackupReplicationSchedule" {
		return d.backupReplicationScheduleRPC(ctx, r)
	}
	if r.Method == "BackupRetentionSchedule" {
		return d.backupRetentionScheduleRPC(ctx, r)
	}
	if r.Method == "BackupRetention" {
		return d.backupRetentionRPC(ctx, r)
	}
	if r.Method == "BackupReplicate" {
		return d.backupReplicateRPC(ctx, r)
	}
	if r.Method == "PlaneBackup" || r.Method == "CleanupRetired" {
		return d.cleanupRPC(ctx, r)
	}
	if r.Method == "ProphetSession" || r.Method == "ProphetChatRead" || r.Method == "ProphetChat" {
		return d.prophetRPC(ctx, r)
	}
	if r.Method == "Alerts" || r.Method == "Metrics" || r.Method == "HostMetrics" {
		return d.observabilityRead(ctx, r)
	}
	if value, handled, err := d.cachedRead(r); handled {
		return value, err
	}
	if r.Method == "BacklogShow" {
		return d.backlogShow(ctx, r.Params)
	}
	if r.Method == "ChangeRequests" {
		return d.changeRequestsRead(ctx, r.Params)
	}
	if r.Method == "WorkflowLimit" {
		return d.workflowSlotSnapshot(ctx)
	}
	if r.Method == "Tree" {
		return d.treeRead(ctx, r.Params)
	}
	if r.Method == "TaskGet" {
		return d.taskGet(ctx, r.Params)
	}
	if value, handled, err := d.operatorRead(ctx, r); handled {
		return value, err
	}
	if r.Method == "ReviewTargetHealth" {
		var p struct {
			Task string `json:"task"`
		}
		if e := decode(r.Params, &p); e != nil {
			return nil, e
		}
		if d.live == nil {
			return nil, errors.New("review health requires live runtime")
		}
		return d.reviewWorkspaceHealth(ctx, p.Task)
	}
	if workflowReads[r.Method] {
		return d.workflowRead(ctx, r)
	}
	if apiReads[r.Method] {
		return d.apiRead(ctx, r.Method, r.Params)
	}
	if r.Method == "Show" {
		return d.show(ctx, r.Params)
	}
	if r.Method == "Status" {
		var p struct {
			IncludeRetired bool `json:"include_retired"`
			Tasks          bool `json:"tasks"`
		}
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		// childMu guards only the child's identity: a task-row read below may
		// wait on the plane, and must not hold up a plane restart, shutdown or
		// the Status calls queued behind their writer lock.
		d.childMu.RLock()
		pid, port := d.child.PID(), d.child.Port()
		d.childMu.RUnlock()
		adapter := "scripted"
		if d.config.Live != nil {
			adapter = d.config.Live.Adapter
			if adapter == "" {
				adapter = "direct"
			}
		}
		version := BuildVersion()
		version["binary_sha256"] = d.binaryHash
		out := d.statusExtraFields()
		out["default_workflow"] = d.defaultWorkflow.Load()
		out["startup_checks"] = d.startupReport.Load()
		out["symphony"] = map[string]any{"configured": d.linearOrch != nil}
		plane := map[string]any{"kind": "Dolt", "pid": pid, "port": port, "endpoint": fmt.Sprintf("127.0.0.1:%d", port), "data_dir": d.layout.Data, "schema_version": controlplane.SchemaVersion, "last_commit": nil}
		if x := d.statusExtras.value.Load(); x != nil {
			plane["last_commit"] = x.LastCommit
		}
		snapshot, err := d.statusRead(ctx, p.IncludeRetired, p.Tasks)
		if err != nil {
			return nil, err
		}
		for k, v := range map[string]any{"instance": d.config.Instance, "pid": os.Getpid(), "uptime_seconds": time.Since(d.started).Seconds(), "version": version, "adapter": adapter, "plane": plane, "socket": d.layout.Socket, "shutting_down": d.ctx.Err() != nil, "snapshot": snapshot} {
			out[k] = v
		}
		return out, nil
	}
	if r.Method == "Board" {
		return d.board(ctx, r.Params)
	}
	if out, ok, err := d.recordRead(ctx, r.Method, r.Params); ok {
		return out, err
	}
	switch r.Method {
	case "TargetConfigGet", "TargetConfigList", "History", "TaskHistory", "Events", "ProviderHealth", "ProviderObservations":
		switch r.Method {
		case "ProviderHealth":
			var p struct {
				WindowSeconds int64 `json:"window_seconds"`
			}
			if err := decode(r.Params, &p); err != nil {
				return nil, err
			}
			// API-001's window defaults to 15 minutes. An absent window takes
			// it; an out-of-range one falls back to it, as `provider-health
			// --window` does (REQUIREMENTS.md conventions), and the response
			// says so. A window that is not an integer is still refused.
			var windowWarning string
			if p.WindowSeconds < 0 || p.WindowSeconds > 86400 {
				windowWarning = fmt.Sprintf("window_seconds %d is out of range (1..86400); using the default 900", p.WindowSeconds)
				p.WindowSeconds = 0
			}
			if p.WindowSeconds == 0 {
				p.WindowSeconds = 900
			}
			health, err := d.store.ProviderHealth(ctx)
			if err != nil {
				return nil, err
			}
			if len(health) > 1000 {
				return nil, errors.New("provider health summary exceeds 1000 bindings")
			}
			now := time.Now().UTC()
			rows := make([]map[string]any, 0, len(health))
			for _, h := range health {
				rows = append(rows, map[string]any{"health": h, "effective_score": h.At(now), "eligible": h.Eligible(now)})
			}
			series, truncated, err := d.providerHealthSeries(ctx, time.Duration(p.WindowSeconds)*time.Second)
			if err != nil {
				return nil, err
			}
			result := map[string]any{"at": now, "bindings": rows, "window_seconds": p.WindowSeconds, "series": series, "series_truncated": truncated}
			if windowWarning != "" {
				result["window_warning"] = windowWarning
			}
			return result, nil
		case "ProviderObservations":
			var p struct {
				After int64 `json:"after"`
				Limit int   `json:"limit"`
			}
			if err := decode(r.Params, &p); err != nil {
				return nil, err
			}
			if p.Limit == 0 {
				p.Limit = 100
			}
			return d.store.ProviderObservations(ctx, p.After, p.Limit)
		case "History", "TaskHistory":
			return d.history(ctx, r.Params)
		case "Events":
			var p eventsParams
			if err := decode(r.Params, &p); err != nil {
				return nil, err
			}
			if p.Limit == 0 {
				p.Limit = 100
			}
			return d.eventReadFiltered(ctx, p.After, p.Limit, p.IncludeRetired, p.EventFilter)
		case "TargetConfigList":
			if err := decode(r.Params, &struct{}{}); err != nil {
				return nil, err
			}
			return d.store.ListTargets(ctx)
		case "TargetConfigGet":
			var p struct {
				ID       string `json:"id"`
				Revision int64  `json:"revision"`
			}
			if err := decode(r.Params, &p); err != nil {
				return nil, err
			}
			rev, raw, err := d.store.GetTarget(ctx, p.ID, p.Revision)
			return struct {
				Revision controlplane.TargetRevision `json:"revision"`
				Document json.RawMessage             `json:"document"`
			}{rev, raw}, err
		}
	}
	// Reads a writer only checks run before the writer lock and are re-checked
	// under it (FAE-ROB-020/-021).
	ctx = d.readBeforeWriterLock(ctx, r.Method, r.Params)
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.publishStatus()
	if err := d.mutationRefusal(verb, permission.Read); err != nil {
		return nil, err
	}
	if r.Method != "Admit" {
		d.awaitCheckpoint()
	}
	if r.Method == "Schedule" {
		var p ScheduleParams
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		if p.Action == "list" || p.Action == "inspect" {
			return d.schedule(ctx, p, controlplane.Audit{})
		}
	}
	if r.Method == "Message" {
		return d.messageRPC(ctx, principal, r.Params)
	}
	if err := d.reconcileBacklogCheckpoint(ctx); err != nil {
		return nil, fmt.Errorf("backlog outcome unresolved: %w", err)
	}
	continuation := false
	if r.Method == "Controller" {
		var p controller.Params
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		if p.Action == "health" {
			return d.controllerAction(ctx, r.Params, controlplane.Audit{})
		}
		if p.Action == "alert" || p.Action == "rearm" || p.Action == "watchdog-register" {
			var err error
			continuation, err = d.watchdogAuthorized(ctx, p.AuthorizingMessage)
			if err != nil {
				return nil, err
			}
		}
		if p.Action == "accept" || p.Action == "stop" || p.Action == "abort" {
			marker, err := d.maintenanceMarker()
			if err != nil {
				return nil, err
			}
			continuation = marker != nil && marker.Message == p.AuthorizingMessage
		}
	}
	var authority struct {
		AuthorizingMessage string `json:"authorizing_message"`
	}
	if err := json.Unmarshal(r.Params, &authority); err != nil {
		return nil, err
	}
	skipAuthorize := false
	if authority.AuthorizingMessage == "" {
		if r.Method == "Notify" {
			var np TaskNotifyParams
			_ = json.Unmarshal(r.Params, &np)
			if (principal.Actor == "controller" || np.Actor == "controller") && np.Kind == "note" && strings.TrimSpace(np.FromTask) != "" {
				skipAuthorize = true
			} else if principal.Actor == "operator" && np.Actor != "controller" {
				skipAuthorize = true
			}
		} else if r.Method == "FollowUp" && principal.Actor == "operator" {
			skipAuthorize = true
		}
	}
	if !skipAuthorize {
		var authErr2 error
		ctx, authErr2 = d.store.AuthorizeAction(ctx, authority.AuthorizingMessage, principal.Actor, verb, permission.Class, continuation, actionClasses())
		if authErr2 != nil {
			return nil, authErr2
		}
	}
	audit := controlplane.Audit{Actor: principal.Actor, Action: r.Method, MessageID: authority.AuthorizingMessage, Inputs: r.Params, Outcome: "ok"}

	switch r.Method {
	case "WorkflowLimitSet":
		return d.workflowLimitSet(ctx, r.Params, audit)
	case "TreeLimit":
		return d.treeLimit(ctx, r.Params, audit)
	case "ControllerOperation":
		return d.controllerOperation(ctx, r.Params, audit)
	case "Notify":
		return d.taskNotify(ctx, r.Params, audit)
	case "ResumeAtStage":
		return d.taskControl(ctx, r.Method, r.Params, audit)
	case "SupplyArtifact":
		return d.supplyArtifact(ctx, r.Params, audit)
	case "ChangeRequestDisposition":
		return d.changeRequestDisposition(ctx, r.Params, audit)
	case "FollowUp":
		return d.taskFollowUp(ctx, r.Key, r.Params, audit)
	case "BacklogAdmit":
		return d.backlogAdmit(ctx, r.Params, audit)
	case "BacklogEdge":
		return d.backlogEdge(ctx, r.Params, audit)
	case "TaskCreate":
		return d.taskCreate(ctx, r.Params, audit)
	case "TaskUpdate":
		return d.taskUpdate(ctx, r.Params, audit)
	case "TaskDelete":
		return d.taskDelete(ctx, r.Params, audit)
	case "ReconcileDone":
		var p struct {
			DryRun             bool   `json:"dry_run"`
			AuthorizingMessage string `json:"authorizing_message"`
		}
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		return d.reconcileBacklogDone(ctx, p.DryRun, audit)
	case "Retire", "Escalate", "Block", "Cancel", "Defer", "Undefer", "Reprioritize", "SetAttemptLimit", "RestartWithInformation", "ResumeWithContext", "Resume":
		return d.taskControl(ctx, r.Method, r.Params, audit)
	case "ResumeRun":
		return d.resumeRun(ctx, r.Params, audit)
	case "Controller":
		return d.controllerAction(ctx, r.Params, audit)
	case "AlertAcknowledge":
		return d.alertAcknowledge(ctx, r.Params, audit)
	case "ResolveUncertain":
		return d.resolveUncertain(ctx, r.Params, audit)
	case "RuntimeConfig":
		return d.updateRuntimeConfig(ctx, r.Params, audit)
	case "PoolResize":
		return d.poolResize(ctx, r.Params, audit)
	case "QuotaSet":
		return d.quotaSet(ctx, r.Params, audit)
	case "QuotaClear":
		return d.quotaClear(ctx, r.Params, audit)
	case "DispatchPause":
		return d.dispatchPause(ctx, r.Params, audit)
	case "DispatchUnpause":
		return d.dispatchUnpause(ctx, r.Params, audit)
	case "Schedule":
		var p ScheduleParams
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		return d.schedule(ctx, p, audit)
	case "Drain":
		if d.loopCheckpoint != nil || d.pendingCheckpoint != nil || d.backlogUncertain {
			return nil, errors.New("drain requires unresolved checkpoints to recover first")
		}
		if err := decode(r.Params, &authority); err != nil {
			return nil, err
		}
		if err := d.store.PutMarker(ctx, audit, "drain", json.RawMessage(`{"requested":true}`)); err != nil {
			return nil, err
		}
		d.draining = true
		d.publishStatus()
		return d.statusSummary(), nil
	case "Shutdown":
		if err := d.store.PutMarker(ctx, audit, "shutdown", json.RawMessage(`{"requested":true}`)); err != nil {
			return nil, err
		}
		d.cancel()
		return map[string]bool{"shutting_down": true}, nil
	case "AdapterRetire":
		if principal.Actor != "operator" || principal.Task != "" {
			return nil, errors.New("adapter retirement requires instance operator")
		}
		var p AdapterRetireParams
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		if err := d.store.RetireAdapter(ctx, audit, p.ID, p.Reason); err != nil {
			return nil, err
		}
		return d.store.AdapterRetirement(ctx, p.ID)
	case "TargetConfigRetire":
		var p struct {
			ID                 string `json:"id"`
			Reason             string `json:"reason"`
			AuthorizingMessage string `json:"authorizing_message"`
		}
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		return map[string]bool{"retired": true}, d.store.RetireTarget(ctx, audit, p.ID, p.Reason)
	case "TargetConfigPut":
		return d.putTarget(ctx, audit, r.Params)
	case "WorkflowInstall":
		var p InstallParams
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		if p.ID != p.Document.ID {
			return nil, errors.New("workflow ID differs from document")
		}
		if d.live != nil && d.live.guard.panes != nil {
			if err := validatePaneFloor(p.Document, d.live.guard.effectivePaneLimit()); err != nil {
				return nil, err
			}
		}
		if p.Name == "" {
			p.Name = p.ID
		}
		if len(p.Name) > 255 {
			return nil, errors.New("workflow name exceeds 255 bytes")
		}
		raw, _ := json.Marshal(p.Document)
		parsed, err := d.parseWorkflowWrite(raw)
		if err != nil {
			return nil, err
		}
		footprint, err := d.resolveFootprint(ctx, parsed, 0)
		if err != nil {
			return nil, fmt.Errorf("workflow footprint: %w", err)
		}
		raw, err = json.Marshal(parsed)
		if err != nil {
			return nil, err
		}
		footprintJSON, _ := json.Marshal(footprint)
		return d.store.WriteWorkflow(ctx, audit, p.ID, p.Name, raw, d.validWorkflowWrite, footprintJSON)
	case "WorkflowRetire":
		return d.workflowRetire(ctx, r.Params, audit)
	case "Admit":
		return d.admit(ctx, r.Key, r.Params, audit)
	case "AttachDesignDoc", "Reclaim", "Migrate", "Decide", "QuarantineRelease", "QuarantineRetire":
		return d.operatorVerb(ctx, r, audit)
	default:
		return nil, fmt.Errorf("unknown RPC method %q", r.Method)
	}
}

func cleanGitEnv() []string {
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	return env
}

func decodeEnvelope(raw []byte, e *envelope) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	d.DisallowUnknownFields()
	return d.Decode(e)
}
