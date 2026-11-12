package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/api"
)

type actionPermission struct {
	Class   string
	Read    bool
	Prophet bool
}

var actionPermissions = map[string]actionPermission{
	"WorkflowLimit": {Read: true}, "WorkflowLimitSet": {Class: "capacity"},
	"ChangeRequests":           {Read: true},
	"ChangeRequestDisposition": {Class: "task"},
	"ResumeAtStage":            {Class: "task"},
	"SupplyArtifact":           {Class: "task"},
	"Tree":                     {Read: true},
	"TreeLimit":                {Class: "runtime"},
	"ControllerOperation":      {Class: "controller"},
	"Topology":                 {Read: true}, "ProvenanceChain": {Read: true},
	"ScratchCleanupAuthorize": {Class: "runtime"},
	"ScratchArchiveCleanup":   {Class: "runtime"},
	"ReviewWorkspacePlan":     {Read: true}, "ReviewWorkspacePrepare": {Class: "governance"},
	"AdapterRetire": {Class: "governance"}, "AdapterPut": {Class: "governance"}, "AdapterGet": {Read: true}, "AdapterList": {Read: true},
	"BackupQuota": {Class: "runtime"}, "BackupReplicationUnpin": {Class: "runtime"}, "BackupSchedule": {Class: "runtime"}, "BackupReplicationSchedule": {Class: "runtime"}, "BackupRetentionSchedule": {Class: "runtime"}, "BackupRetention": {Class: "runtime"}, "BackupReplicate": {Class: "runtime"}, "PlaneBackup": {Class: "runtime"}, "CleanupRetired": {Class: "runtime"},
	"Census": {Read: true}, "CensusApply": {Class: "task"},
	"Land": {Class: "controller"}, "LandStatus": {Read: true},
	"Alerts": {Read: true}, "Metrics": {Read: true}, "HostMetrics": {Read: true}, "AlertAcknowledge": {Class: "runtime"},
	"TargetConfigRetire": {Class: "governance"},
	"TargetConfigPut":    {Class: "governance"}, "TargetConfigGet": {Read: true}, "TargetConfigList": {Read: true},
	"BacklogAdmit": {Class: "task"}, "BacklogEdge": {Class: "task"}, "BacklogShow": {Read: true}, "ReconcileDone": {Class: "task"},
	"TaskCreate": {Class: "task"}, "TaskUpdate": {Class: "task"}, "TaskDelete": {Class: "task"}, "TaskGet": {Read: true},
	"ProphetSession": {Read: true}, "ProphetChatRead": {Read: true}, "ProphetChat": {Class: "message"},
	"Status": {Read: true}, "Board": {Read: true}, "History": {Read: true}, "TaskHistory": {Read: true}, "Events": {Read: true}, "Workflows": {Read: true}, "Show": {Read: true}, "WorkflowList": {Read: true}, "WorkflowDiff": {Read: true}, "WorkflowValidate": {Read: true}, "WorkflowSimulate": {Read: true}, "ProviderHealth": {Read: true}, "ProviderObservations": {Read: true}, "ReviewTargetHealth": {Read: true},
	"Message": {Class: "message"}, "WorkflowInstall": {Class: "governance"}, "WorkflowRetire": {Class: "governance"}, "Admit": {Class: "task"}, "Drain": {Class: "runtime"}, "Shutdown": {Class: "runtime"}, "RuntimeConfig": {Class: "runtime"}, "ResolveUncertain": {Class: "task"}, "Schedule": {Class: "schedule"}, "Controller": {Class: "controller"},
	"Retire": {Class: "task"}, "Resume": {Class: "task"}, "ResumeRun": {Class: "task"}, "Escalate": {Class: "task"}, "Block": {Class: "task"}, "Cancel": {Class: "task"}, "Defer": {Class: "task"}, "Undefer": {Class: "task"}, "Reprioritize": {Class: "task"}, "SetAttemptLimit": {Class: "task"}, "RestartWithInformation": {Class: "task", Prophet: true}, "ResumeWithContext": {Class: "task", Prophet: true},
	"Notify": {Class: "task"}, "FollowUp": {Class: "task"},
	"Inbox": {Read: true}, "AnalyzeSession": {Read: true}, "AttachDesignDoc": {Class: "task"}, "Reclaim": {Class: "runtime"}, "Migrate": {Class: "task"},
	"GatesSelfTest": {Read: true}, "Leases": {Read: true}, "Panes": {Read: true}, "TargetConfigValidate": {Read: true}, "TargetConfigDiff": {Read: true}, "TargetConfigSimulate": {Read: true},
	"Decide":            {Class: "task"},
	"UIToken":           {Read: true}, // the handler admits only the instance operator
	"QuarantineInspect": {Read: true}, "QuarantineRelease": {Class: "task"}, "QuarantineRetire": {Class: "task"},
	"PoolResize": {Class: "runtime"}, "QuotaSet": {Class: "runtime"}, "QuotaClear": {Class: "runtime"},
	"DispatchPause": {Class: "runtime"}, "DispatchUnpause": {Class: "runtime"}, "DispatchPauseList": {Read: true},
}

func permissionFor(r api.Request) (actionPermission, string, bool) {
	p, ok := actionPermissions[r.Method]
	verb := r.Method
	if r.Method == "Schedule" || r.Method == "Controller" {
		var v struct {
			Action string `json:"action"`
		}
		if json.Unmarshal(r.Params, &v) != nil {
			return p, verb, false
		}
		verb += "." + v.Action
		p.Read = (r.Method == "Schedule" && (v.Action == "list" || v.Action == "inspect")) || (r.Method == "Controller" && v.Action == "health")
	}
	if r.Method == "Reclaim" {
		var v struct {
			HostWide bool `json:"host_wide"`
		}
		if json.Unmarshal(r.Params, &v) == nil && v.HostWide {
			verb, p.Class = "Reclaim.host-wide", "host-wide"
		}
	}
	return p, verb, ok
}
func authorizePrincipal(ctx context.Context, r api.Request) (api.Principal, actionPermission, string, error) {
	p, verb, known := permissionFor(r)
	principal, authenticated := api.AuthenticatedPrincipal(ctx)
	// Direct in-process read helpers remain usable; mutation requires transport identity.
	if !authenticated && p.Read {
		principal.Actor = "reader"
		authenticated = true
	}
	allowed := false
	switch principal.Actor {
	case "operator":
		allowed = true
	case "controller":
		// A controller Message is only a relay; messageRPC refuses anything else.
		allowed = r.Method != "ProphetChat"
	case "prophet":
		allowed = p.Read || p.Prophet
	case "workflow-role", "qa", "reader", "ui":
		allowed = p.Read
	case "ui-writer":
		// FAE-API-032: the UI write capability; the cited message must still
		// direct the exact verb, and operator-only handlers still refuse it.
		allowed = r.Method != "ProphetChat"
	}
	if p.Class == "capacity" && principal.Actor != "operator" && principal.Actor != "controller" {
		allowed = false
	}
	if !MayPerformAction(authenticated, known, allowed) {
		return principal, p, verb, errors.New("actor is not permitted to perform action")
	}
	if principal.Task != "" {
		var params struct {
			ID   string `json:"id"`
			Task string `json:"task"`
		}
		_ = json.Unmarshal(r.Params, &params)
		if r.Method != "ProphetSession" && r.Method != "ProphetChatRead" && r.Method != "TaskHistory" && r.Method != "RestartWithInformation" && r.Method != "ResumeWithContext" {
			return principal, p, verb, errors.New("task principal cannot access instance-wide action")
		}
		if (params.ID == "" && params.Task == "") || (params.ID != "" && params.ID != principal.Task) || (params.Task != "" && params.Task != principal.Task) {
			return principal, p, verb, errors.New("task principal scope mismatch")
		}
	}
	return principal, p, verb, nil
}

func actionClasses() map[string]string {
	classes := map[string]string{"CleanupScratch": "runtime", "Reclaim.host-wide": "host-wide"}
	for verb, p := range actionPermissions {
		classes[verb] = p.Class
	}
	for _, action := range []string{"prepare", "accept", "abort", "stop", "rearm", "alert", "watchdog-register", "watchdog-revoke", "preflight-failed", "stop-drive"} {
		classes["Controller."+action] = "controller"
	}
	for _, action := range []string{"create", "pause", "resume", "retire"} {
		classes["Schedule."+action] = "schedule"
	}
	return classes
}
