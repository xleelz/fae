package provider

import (
	"testing"
	"time"
)

func TestResumeTaskSessionRequiresProviderConfirmationAndMatchingRoute(t *testing.T) {
	r := NewSessionRegistry("")
	req := AcquireRequest{TaskID: "linear-task", WorkflowID: "default", WorkflowRevision: "revision", Role: "planner", Provider: "claude", Model: "model", Scope: ReuseWorkflowGlobal}
	for i := 0; i < 2; i++ {
		if _, err := r.Acquire(req); err != nil {
			t.Fatal(err)
		}
		if got := r.ResumeTaskSession(req.TaskID, req.Role, req.Provider, req.Model); got != "" {
			t.Fatal("allocated ID treated as a real provider conversation")
		}
	}
	r.BindThreadID(req.TaskID, req.WorkflowID, req.WorkflowRevision, req.Role, req.Provider, req.Model, "local", "provider-conversation", false, time.Now())
	if got := r.ResumeTaskSession(req.TaskID, req.Role, req.Provider, req.Model); got != "provider-conversation" {
		t.Fatal("confirmed conversation unavailable")
	}
	for _, route := range [][3]string{{"coder", "claude", "model"}, {"planner", "codex", "model"}, {"planner", "claude", "other"}} {
		if got := r.ResumeTaskSession(req.TaskID, route[0], route[1], route[2]); got != "" {
			t.Fatal("cross-role/provider/model conversation reused")
		}
	}
}
