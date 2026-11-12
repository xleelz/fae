package daemon

import (
	"encoding/json"

	"github.com/xleelz/fae/controller"
)

type (
	noParams     struct{}
	messageCited struct {
		AuthorizingMessage string `json:"authorizing_message"`
	}
	idRevisionParams struct {
		ID       string `json:"id"`
		Revision int64  `json:"revision"`
	}
	statusParams struct {
		IncludeRetired bool `json:"include_retired"`
		Tasks          bool `json:"tasks"`
	}
	eventsParams struct {
		EventFilter
		IncludeRetired bool  `json:"include_retired"`
		After          int64 `json:"after"`
		Limit          int   `json:"limit"`
	}
	cursorParams struct {
		After int64 `json:"after"`
		Limit int   `json:"limit"`
	}
	providerHealthParams struct {
		WindowSeconds int64 `json:"window_seconds"`
	}
	targetRetireParams struct {
		ID                 string `json:"id"`
		Reason             string `json:"reason"`
		AuthorizingMessage string `json:"authorizing_message"`
	}
	reconcileDoneParams struct {
		DryRun             bool   `json:"dry_run"`
		AuthorizingMessage string `json:"authorizing_message"`
	}
	backlogShowParams struct {
		State          string `json:"state"`
		IncludeRetired bool   `json:"include_retired"`
	}
	taskParam struct {
		Task string `json:"task"`
	}
	landStatusParams struct {
		ID string `json:"id"`
	}
	targetValidateParams struct {
		Document json.RawMessage `json:"document"`
	}
	targetDiffParams struct {
		ID   string `json:"id"`
		From int64  `json:"from"`
		To   int64  `json:"to"`
	}
)

// rpcParams is the schema-first registry of every RPC's parameter message
// (FAE-ARC-004): api/schema.json describes each, and a test keeps the two
// equal. Add a row with every new method.
var rpcParams = map[string]any{
	"ChangeRequests":           ChangeRequestsParams{},
	"ChangeRequestDisposition": ChangeRequestDispositionParams{},
	"ResumeAtStage":            TaskControlParams{},
	"SupplyArtifact":           SupplyArtifactParams{},
	"Tree":                     TreeParams{},
	"WorkflowLimit":            noParams{}, "WorkflowLimitSet": WorkflowLimitParams{},
	"TreeLimit":           TreeLimitParams{},
	"ControllerOperation": ControllerOperationParams{},
	"Status":              statusParams{}, "Board": BoardParams{}, "History": HistoryParams{}, "TaskHistory": HistoryParams{}, "Events": eventsParams{}, "Show": ShowParams{},
	"Topology": TopologyParams{}, "ProvenanceChain": ChainParams{}, "ProviderHealth": providerHealthParams{}, "ProviderObservations": cursorParams{},
	"Leases": noParams{}, "Panes": noParams{}, "HostMetrics": noParams{}, "Metrics": MetricsParams{}, "Alerts": AlertsParams{}, "AlertAcknowledge": AlertAcknowledgeParams{},
	"Inbox": InboxParams{}, "AnalyzeSession": AnalyzeParams{}, "UIToken": noParams{}, "GatesSelfTest": noParams{},
	"Admit": AdmitParams{}, "ResumeRun": ResumeRunParams{}, "ResolveUncertain": ResolveUncertainParams{}, "RuntimeConfig": RuntimeConfig{},
	"Drain": messageCited{}, "Shutdown": messageCited{}, "Message": MessageParams{}, "Decide": DecideParams{}, "Migrate": MigrateParams{},
	"AttachDesignDoc": DesignDocParams{}, "Reclaim": ReclaimParams{},
	"QuarantineInspect": QuarantineParams{}, "QuarantineRelease": QuarantineParams{}, "QuarantineRetire": QuarantineParams{},
	"Retire": TaskControlParams{}, "Resume": TaskControlParams{}, "Escalate": TaskControlParams{}, "Block": TaskControlParams{}, "Cancel": TaskControlParams{},
	"Defer": TaskControlParams{}, "Undefer": TaskControlParams{}, "Reprioritize": TaskControlParams{}, "SetAttemptLimit": TaskControlParams{},
	"RestartWithInformation": TaskControlParams{}, "ResumeWithContext": TaskControlParams{},
	"Notify": TaskNotifyParams{}, "FollowUp": TaskFollowUpParams{},
	"BacklogAdmit": BacklogParams{}, "BacklogEdge": BacklogEdgeParams{}, "BacklogShow": backlogShowParams{}, "ReconcileDone": reconcileDoneParams{},
	"TaskCreate": TaskCreateParams{}, "TaskUpdate": TaskUpdateParams{}, "TaskDelete": TaskDeleteParams{}, "TaskGet": TaskGetParams{},
	"ProphetSession": ProphetParams{}, "ProphetChatRead": ProphetParams{}, "ProphetChat": ProphetParams{},
	"WorkflowInstall": InstallParams{}, "WorkflowRetire": WorkflowParams{}, "Workflows": WorkflowParams{}, "WorkflowList": WorkflowParams{},
	"WorkflowDiff": WorkflowParams{}, "WorkflowValidate": WorkflowParams{}, "WorkflowSimulate": WorkflowParams{},
	"TargetConfigPut": TargetPutParams{}, "TargetConfigGet": idRevisionParams{}, "TargetConfigList": noParams{}, "TargetConfigRetire": targetRetireParams{},
	"TargetConfigValidate": targetValidateParams{}, "TargetConfigDiff": targetDiffParams{}, "TargetConfigSimulate": TargetSimulateParams{},
	"AdapterPut": AdapterPutParams{}, "AdapterGet": idRevisionParams{}, "AdapterList": noParams{}, "AdapterRetire": AdapterRetireParams{},
	"Schedule": ScheduleParams{}, "Controller": controller.Params{}, "Land": LandParams{}, "LandStatus": landStatusParams{},
	"Census": CensusParams{}, "CensusApply": CensusApplyParams{}, "PlaneBackup": CleanupParams{}, "CleanupRetired": CleanupParams{},
	"BackupQuota": BackupQuotaParams{}, "BackupReplicationUnpin": BackupReplicationUnpinParams{}, "BackupSchedule": BackupScheduleParams{},
	"BackupReplicationSchedule": ReplicationScheduleParams{}, "BackupRetentionSchedule": RetentionScheduleParams{}, "BackupRetention": BackupRetentionParams{},
	"BackupReplicate": BackupReplicationParams{}, "ScratchCleanupAuthorize": messageCited{}, "ScratchArchiveCleanup": ScratchArchiveRPCParams{},
	"ReviewWorkspacePlan": ReviewWorkspaceParams{}, "ReviewWorkspacePrepare": ReviewWorkspaceParams{}, "ReviewTargetHealth": taskParam{},
	"PoolResize": PoolResizeParams{}, "QuotaSet": QuotaSetParams{}, "QuotaClear": QuotaClearParams{},
	"DispatchPause": DispatchPauseParams{}, "DispatchUnpause": DispatchPauseParams{}, "DispatchPauseList": noParams{},
}
