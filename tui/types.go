package tui

import "github.com/xleelz/fae/tui/client"

type (
	ParentRef                 = client.ParentRef
	ChildRef                  = client.ChildRef
	WorkflowRef               = client.WorkflowRef
	BoardItem                 = client.Task
	TaskView                  = client.TaskDetail
	TopologyStage             = client.Stage
	TurnBudgetView            = client.TurnBudgetView
	ReadPolicyView            = client.ReadPolicyView
	NotificationView          = client.NotificationView
	TurnCheckpointView        = client.TurnCheckpointView
	FanBranchView             = client.FanBranchView
	FanView                   = client.FanView
	PendingRequestView        = client.PendingRequestView
	ProviderPoolView          = client.ProviderPoolView
	ProviderBindingHealthView = client.ProviderBindingHealthView
	DispatchPauseView         = client.DispatchPauseView
	PaneSessionView           = client.PaneSessionView
	SessionAnalysisView       = client.SessionAnalysisView
	ReadRootView              = client.ReadRootView
	TargetConfigView          = client.TargetConfigView
	EventView                 = client.EventView
	ProvenanceEntryView       = client.ProvenanceEntryView
	LandStatusView            = client.LandStatusView
	LinearIssueView           = client.LinearIssueView
	TicketOutboxView          = client.TicketOutboxView
	SymphonyStateView         = client.SymphonyStateView
)

type streamState struct {
	cursor int
	scroll int
}

type poolsState struct {
	cursor int
	scroll int
}

type symphonyState struct {
	cursor int
	scroll int
}

type securityState struct {
	cursor int
	scroll int
}

