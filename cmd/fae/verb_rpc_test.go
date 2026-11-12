package main

// verbRPC is FAE-API-010's coverage map: every daemon-backed CLI verb path
// and the one RPC it calls (Controller and Schedule name their action). The
// conformance test fails on a verb that calls another RPC, several RPCs, or
// none. Offline verbs (version, help, doctor, workflow, template
// workflow-validate, gates self-test --offline) and the host lease brokers
// (gate run, heavy-op) mutate no instance state. Roll and maintenance
// stop/start orchestrate the Controller RPC's audited actions from outside
// the daemon, which they replace; cleanup scratch previews read the stopped
// instance, and apply authorizes and stops a running one through the daemon
// before its offline phase under the instance lock.
var verbRPC = map[string]string{
	"workflow-limit": "WorkflowLimit", "workflow-limit --max": "WorkflowLimitSet",
	"resume-at-stage": "ResumeAtStage", "change-request list": "ChangeRequests", "change-request disposition": "ChangeRequestDisposition",
	"tree": "Tree", "tree-limit": "TreeLimit",
	"status": "Status", "board": "Board", "history": "History", "show": "Show", "events": "Events", "events --follow": "Events.stream",
	"topology": "Topology", "provenance": "ProvenanceChain", "provider-health": "ProviderHealth", "provider-log": "ProviderObservations",
	"message": "Message", "reply": "Message", "inbox": "Inbox", "attach-design-doc": "AttachDesignDoc", "migrate": "Migrate",
	"analyze-session": "AnalyzeSession", "decide": "Decide", "supply-artifact": "SupplyArtifact", "reclaim": "Reclaim",
	"quarantine": "QuarantineInspect", "quarantine inspect": "QuarantineInspect", "quarantine release": "QuarantineRelease", "quarantine retire": "QuarantineRetire",
	"alerts": "Alerts", "alerts list": "Alerts", "alerts ack": "AlertAcknowledge", "metrics": "Metrics", "host-metrics": "HostMetrics", "ui token": "UIToken",
	"resolve-uncertain": "ResolveUncertain", "runtime-config": "RuntimeConfig", "drain": "Drain", "shutdown": "Shutdown",
	"run": "Admit", "start-work-item": "BacklogAdmit", "resume": "ResumeRun",
	"retire": "Retire", "escalate": "Escalate", "block": "Block", "cancel": "Cancel", "defer": "Defer", "undefer": "Undefer",
	"reprioritize": "Reprioritize", "set-attempt-limit": "SetAttemptLimit", "restart": "RestartWithInformation", "resume-with-context": "ResumeWithContext",
	"notify": "Notify", "follow-up": "FollowUp",
	"backlog show": "BacklogShow", "backlog admit": "BacklogAdmit", "backlog edge add": "BacklogEdge", "backlog edge remove": "BacklogEdge", "reconcile-done": "ReconcileDone",
	"task create": "TaskCreate", "task update": "TaskUpdate", "task delete": "TaskDelete", "task show": "TaskGet",
	"prophet session": "ProphetSession", "prophet chat": "ProphetChat", "prophet global": "ProphetSession",
	"prophet restart": "RestartWithInformation", "prophet resume": "ResumeWithContext",
	"template workflow-put": "WorkflowInstall", "template workflow-get": "Workflows", "template workflow-latest": "Workflows", "template workflow-list": "WorkflowList",
	"template workflow-retire": "WorkflowRetire", "template workflow-diff": "WorkflowDiff", "template workflow-simulate": "WorkflowSimulate",
	"leases": "Leases", "panes": "Panes",
	"template target-config-validate": "TargetConfigValidate", "template target-config-diff": "TargetConfigDiff", "template target-config-simulate": "TargetConfigSimulate",
	"template target-config-put": "TargetConfigPut", "template target-config-get": "TargetConfigGet", "template target-config-list": "TargetConfigList", "template target-config-retire": "TargetConfigRetire",
	"template adapter-put": "AdapterPut", "template adapter-get": "AdapterGet", "template adapter-list": "AdapterList", "template adapter-retire": "AdapterRetire",
	"schedule create": "Schedule.create", "schedule list": "Schedule.list", "schedule inspect": "Schedule.inspect", "schedule pause": "Schedule.pause", "schedule resume": "Schedule.resume", "schedule retire": "Schedule.retire",
	"drive status": "Controller.health", "drive stop": "Controller.stop-drive", "drive rearm": "Controller.rearm", "maintenance status": "Controller.health",
	"workspace review-plan": "ReviewWorkspacePlan", "workspace review-prepare": "ReviewWorkspacePrepare", "review-health": "ReviewTargetHealth",
	"census": "Census", "census apply": "CensusApply", "cleanup retired": "CleanupRetired", "cleanup done": "CleanupRetired", "backup plane": "PlaneBackup",
	"backup quota status": "BackupQuota", "backup quota configure": "BackupQuota", "backup retention": "BackupRetention", "backup replicate": "BackupReplicate", "backup unpin": "BackupReplicationUnpin",
	"backup retention-schedule status": "BackupRetentionSchedule", "backup creation-schedule status": "BackupSchedule", "backup replication-schedule status": "BackupReplicationSchedule",
	"cleanup scratch-archive": "ScratchArchiveCleanup", "gates self-test": "GatesSelfTest", "land": "Land",
	"pool resize": "PoolResize", "quota set": "QuotaSet", "quota clear": "QuotaClear",
	"pause": "DispatchPause", "pause list": "DispatchPauseList", "unpause": "DispatchUnpause",
}
