package daemon

import (
	"bytes"
	"context"
	"database/sql"
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
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
)

type AdmitParams struct {
	TargetConfig       string `json:"target_config,omitempty"`
	TargetRevision     int64  `json:"target_revision,omitempty"`
	TargetHash         string `json:"target_hash,omitempty"`
	CommitExternal     bool   `json:"commit_external,omitempty"`
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
}
type InstallParams struct {
	ID                 string            `json:"id"`
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
func (d *Daemon) installDefault(ctx context.Context) error {
	doc := defaults.Workflow()
	if d.live != nil && d.live.guard.panes != nil {
		if err := validatePaneFloor(doc, d.live.guard.effectivePaneLimit()); err != nil {
			return err
		}
	}
	_, _, err := d.store.GetWorkflow(ctx, doc.ID, 0)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	raw, _ := json.Marshal(doc)
	_, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "daemon", Action: "install builtin workflow", Outcome: "ok"}, doc.ID, doc.ID, raw, validWorkflow)
	return err
}
func (d *Daemon) handleDirect(ctx context.Context, r api.Request) (any, error) {
	principal, permission, verb, authErr := authorizePrincipal(ctx, r)
	if authErr != nil {
		return nil, authErr
	}
	if r.Method == "AdapterPut" {
		return d.adapterInstallRPC(ctx, r, principal)
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
	if r.Method == "Census" {
		return d.census(ctx, r.Params)
	}
	if r.Method == "CensusApply" {
		return d.censusApply(ctx, r.Params)
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
	if value, handled, err := d.cachedRead(r); handled {
		return value, err
	}
	if r.Method == "BacklogShow" {
		return d.backlogShow(ctx, r.Params)
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

	if r.Method == "Status" {
		var p struct {
			IncludeRetired bool `json:"include_retired"`
			Tasks          bool `json:"tasks"`
		}
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		d.childMu.RLock()
		defer d.childMu.RUnlock()
		adapter := "scripted"
		if d.config.Live != nil {
			adapter = d.config.Live.Adapter
			if adapter == "" {
				adapter = "direct"
			}
		}
		return map[string]any{"instance": d.config.Instance, "pid": os.Getpid(), "uptime_seconds": time.Since(d.started).Seconds(), "adapter": adapter, "plane": map[string]any{"kind": "Dolt", "pid": d.child.PID(), "port": d.child.Port(), "data_dir": d.layout.Data, "schema_version": controlplane.SchemaVersion}, "socket": d.layout.Socket, "shutting_down": d.ctx.Err() != nil, "snapshot": d.statusSummary(p.IncludeRetired)}, nil
	}
	if r.Method == "Board" {
		return d.board(r.Params)
	}
	switch r.Method {
	case "TargetConfigGet", "TargetConfigList", "History", "TaskHistory", "Events", "Workflows", "ProviderHealth", "ProviderObservations":
		switch r.Method {
		case "ProviderHealth":
			if err := decode(r.Params, &struct{}{}); err != nil {
				return nil, err
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
			return map[string]any{"at": now, "bindings": rows}, nil
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
			var p struct {
				IncludeRetired bool  `json:"include_retired"`
				After          int64 `json:"after"`
				Limit          int   `json:"limit"`
			}
			if err := decode(r.Params, &p); err != nil {
				return nil, err
			}
			if p.Limit == 0 {
				p.Limit = 100
			}
			return d.eventRead(ctx, p.After, p.Limit, p.IncludeRetired)
		case "TargetConfigList":
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
		case "Workflows":
			var p struct {
				ID       string `json:"id"`
				Revision int64  `json:"revision"`
			}
			if err := decode(r.Params, &p); err != nil {
				return nil, err
			}
			revision, raw, err := d.store.GetWorkflow(ctx, p.ID, p.Revision)
			return struct {
				Revision controlplane.Revision `json:"revision"`
				Document json.RawMessage       `json:"document"`
			}{revision, raw}, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	defer d.publishStatus()
	if d.ctx.Err() != nil {
		return nil, errors.New("shutting-down")
	}
	if _, err := os.Stat(filepath.Join(d.layout.Root, "maintenance")); err == nil {
		return nil, errors.New("maintenance")
	} else if !os.IsNotExist(err) {
		return nil, err
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
		var p struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		}
		if err := decode(r.Params, &p); err != nil {
			return nil, err
		}
		return map[string]string{"id": p.ID}, d.store.RecordMessage(ctx, p.ID, p.Text)
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
			marker, err := controller.ReadMarker(d.controllerMarkerPath())
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
	var authErr2 error
	ctx, authErr2 = d.store.AuthorizeAction(ctx, authority.AuthorizingMessage, principal.Actor, verb, permission.Class, continuation, actionClasses())
	if authErr2 != nil {
		return nil, authErr2
	}
	audit := controlplane.Audit{Actor: principal.Actor, Action: r.Method, MessageID: authority.AuthorizingMessage, Inputs: r.Params, Outcome: "ok"}

	switch r.Method {
	case "BacklogAdmit":
		return d.backlogAdmit(ctx, r.Params, audit)
	case "BacklogEdge":
		return d.backlogEdge(ctx, r.Params, audit)
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
	case "Controller":
		return d.controllerAction(ctx, r.Params, audit)
	case "ResolveUncertain":
		return d.resolveUncertain(ctx, r.Params, audit)
	case "RuntimeConfig":
		return d.updateRuntimeConfig(ctx, r.Params, audit)
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
		raw, _ := json.Marshal(p.Document)
		return d.store.WriteWorkflow(ctx, audit, p.ID, p.ID, raw, validWorkflow)
	case "Admit":
		return d.admit(ctx, r.Key, r.Params, audit)
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

func (d *Daemon) statusSummary(include ...bool) any {
	value := d.status.Load()
	if value == nil {
		return nil
	}
	s := value.(statusSnapshot)
	if len(include) == 0 || !include[0] {
		s.Counts = copyCountsWithoutRetired(s.Counts)
		visible := []BoardItem{}
		for _, t := range s.Tasks {
			if t.State != "Retired" {
				visible = append(visible, t)
			}
		}
		s.Tasks = visible
	}
	return struct {
		TicketOutbox  ticketStatus   `json:"ticket_outbox"`
		Configuration map[string]any `json:"configuration"`
		Drained       bool           `json:"drained"`
		Draining      bool           `json:"draining"`
		Active        int            `json:"active"`
		Counts        map[string]int `json:"counts"`
		Total         int            `json:"total"`
		Updated       time.Time      `json:"updated"`
		AgeMillis     int64          `json:"age_ms"`
	}{s.TicketOutbox, s.Configuration, s.Drained, s.Draining, s.Active, s.Counts, len(s.Tasks), s.Updated, time.Since(s.Updated).Milliseconds()}
}
