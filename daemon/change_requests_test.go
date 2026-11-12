package daemon

import (
	"bytes"
	"encoding/json"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"testing"
	"time"
)

func TestChangeRequestNativePairedHistoryDispositionAndRetention(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(map[string][]engine.ScriptStep{"draft": {{Output: json.RawMessage(`{"text":"original note"}`)}}})
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	parent := childWorkflow()
	parent.ID = "request-parent"
	parent.Stages = map[string]workflow.Stage{"draft": parent.Stages["draft"]}
	parent.Edges = []workflow.Edge{{From: "draft", To: "Done"}}
	child := childWorkflow()
	child.ID = "request-child"
	child.Stages = map[string]workflow.Stage{"draft": child.Stages["draft"], "request": {Primitive: workflow.ChangeRequest, Target: "parent", Artifact: "Note", Value: &workflow.Projection{Stage: "draft"}}}
	child.Edges = []workflow.Edge{{From: "draft", To: "request"}, {From: "request", To: "Done"}}
	for _, doc := range []workflow.Document{parent, child} {
		if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "request-root", WorkflowID: parent.ID, Tree: "tree", AuthorizingMessage: "message"}, "request-root"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "request-root", "Done")
	before, err := d.store.GetJob(ctx, "request-root")
	if err != nil {
		t.Fatal(err)
	}
	response, err := call(t, d, "Admit", AdmitParams{WorkflowID: child.ID, ParentTask: "request-root", Tree: "tree", AuthorizingMessage: "message"}, "request-source")
	if err != nil {
		t.Fatal(err)
	}
	var admitted map[string]any
	_ = json.Unmarshal(response.Result, &admitted)
	var source string
	rows, err := d.store.TreeJobs(ctx, "request-root")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID != "request-root" {
			source = r.ID
		}
	}
	if source == "" {
		t.Fatalf("no child: %s", response.Result)
	}
	awaitState(t, d, source, "Done")
	after, err := d.store.GetJob(ctx, "request-root")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Payload, after.Payload) {
		t.Fatal("request modified closed ancestor payload/brief inputs")
	}
	requests, err := d.changeRequestsForTask(ctx, "request-root")
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests=%+v err=%v", requests, err)
	}
	for _, id := range []string{source, "request-root"} {
		events, e := d.store.JobEvents(ctx, id, 0, 1000)
		if e != nil {
			t.Fatal(e)
		}
		found := false
		for _, ev := range events {
			if ev.Kind == "change-request" {
				found = true
			}
		}
		if !found {
			t.Fatal("missing paired history", id)
		}
	}
	if _, err = call(t, d, "ChangeRequestDisposition", ChangeRequestDispositionParams{Task: "request-root", ID: requests[0].ID, State: "accepted", Reason: "reviewed", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}

	middle := childWorkflow()
	middle.ID = "request-forwarder"
	decision := middle.Stages["approve"]
	decision.Options[0].Next = "forward"
	middle.Stages["approve"] = decision
	middle.Stages["forward"] = workflow.Stage{Primitive: workflow.ChangeRequest, Target: "parent", Artifact: "Note", Value: &workflow.Projection{Source: "change_requests"}}
	middle.Edges = []workflow.Edge{{From: "draft", To: "approve"}, {From: "approve", To: "forward"}, {From: "approve", To: "halt"}, {From: "forward", To: "Done"}}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: middle.ID, Document: middle, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "middle", WorkflowID: middle.ID, ParentTask: "request-root", Tree: "tree", AuthorizingMessage: "message"}, "middle"); err != nil {
		t.Fatal(err)
	}
	awaitDecision(t, d, "middle", 1)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "leaf", WorkflowID: child.ID, ParentTask: "middle", Tree: "tree", AuthorizingMessage: "message"}, "leaf"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "leaf", "Done")
	if _, err = call(t, d, "Decide", DecideParams{ID: "middle", Option: "use", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "middle", "Done")
	forwarded, err := d.changeRequestsForTask(ctx, "request-root")
	if err != nil || len(forwarded) != 2 {
		t.Fatalf("forwarded requests=%+v err=%v", forwarded, err)
	}
	foundForward := false
	for _, r := range forwarded {
		if r.Source == "middle" && r.ForwardedBy == "middle" {
			items, ok := r.Value.([]any)
			if ok && len(items) == 1 && items[0].(map[string]any)["source"] == "leaf" {
				foundForward = true
			}
		}
	}
	if !foundForward {
		t.Fatal("forwarded request lost sender/value", forwarded)
	}
	testJob := engine.Job{Ancestors: []string{"request-root"}}
	if err = d.validateChangeRequest(testJob, workflow.Stage{Target: "request-root", Artifact: "Missing"}); err == nil {
		t.Fatal("nonexistent ancestor artifact accepted")
	}
	if _, err = d.store.EventWindow(ctx, time.Nanosecond, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	requests, err = d.changeRequestsForTask(ctx, "request-root")
	if err != nil || len(requests) != 2 {
		t.Fatalf("retained disposition=%+v err=%v", requests, err)
	}
	retainedAccepted := false
	for _, r := range requests {
		if r.State == "accepted" && r.Message == "message" && r.Actor != "" {
			retainedAccepted = true
		}
	}
	if !retainedAccepted {
		t.Fatal("audited disposition lost after retention", requests)
	}
	after, err = d.store.GetJob(ctx, "request-root")
	if err != nil || !bytes.Equal(before.Payload, after.Payload) {
		t.Fatal("disposition changed ancestor payload", err)
	}
}
