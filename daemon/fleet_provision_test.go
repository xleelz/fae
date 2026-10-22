package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/external"
	"testing"
)

func TestFleetProvisionGuard(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			for _, c := range []bool{false, true} {
				if CanFinishFleetProvision(a, b, c) != (a && b && c) {
					t.Fatal(a, b, c)
				}
			}
		}
	}
}
func fleetUnitRecord() (ReviewWorkspaceRecord, AdapterDocument) {
	r := ReviewWorkspaceRecord{ReviewWorkspacePlan: ReviewWorkspacePlan{Task: "task", RunID: "run", Workspace: "/workspace/task", Branch: "fae/task", IntegrationTip: "tip"}, Policy: external.ReviewPolicy{Package: "package", Workspace: "/workspace/task", Branch: "fae/task", IntegrationTip: "tip", ProvisionTool: "fleet"}, State: "pending"}
	schema := external.ToolSchema{InputSchema: json.RawMessage(`{"type":"object","properties":{"package":{"type":"string"},"workspace":{"type":"string"},"branch":{"type":"string"},"integration_tip":{"type":"string"}},"required":["package","workspace","branch","integration_tip"],"additionalProperties":false}`)}
	b := AdapterDocument{Tools: map[string]external.Tool{"fleet": {Endpoint: "http://127.0.0.1:1", Server: "fixture", Name: "fleet_prepare", Role: "daemon", Arguments: map[string]string{"package": "package", "workspace": "/workspace/task", "branch": "fae/task", "integration_tip": "tip"}, DiscoveredSchema: &schema}}}
	return r, b
}
func TestFleetProvisionPinsAndReceipt(t *testing.T) {
	r, b := fleetUnitRecord()
	if err := initializeReviewProvision(&r, b); err != nil {
		t.Fatal(err)
	}
	original := r.Provision.Intent
	if err := initializeReviewProvision(&r, b); err != nil || !targetEqual(original, r.Provision.Intent) {
		t.Fatal("intent drift", err)
	}
	for _, field := range []string{"package", "workspace", "branch", "integration_tip"} {
		t.Run(field, func(t *testing.T) {
			other, bundle := fleetUnitRecord()
			bundle.Tools["fleet"].Arguments[field] = "other"
			if err := initializeReviewProvision(&other, bundle); err == nil {
				t.Fatal("pin mismatch accepted")
			}
		})
	}
	data, _ := json.Marshal(provisionBinding(r))
	i := r.Provision.Intent
	receipt := external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, Tree: i.Tree, State: "performed", ExternalID: "W1", Data: data}
	if err := matchProvision(b.Tools["fleet"], r, receipt); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`{"package":"package","workspace":"/workspace/task","branch":"fae/task","integration_tip":"other"}`), json.RawMessage(`{"package":"package","workspace":"/workspace/task","branch":"fae/task","integration_tip":"tip","extra":true}`)} {
		broken := receipt
		broken.Data = bad
		if err := matchProvision(b.Tools["fleet"], r, broken); err == nil {
			t.Fatal("unproved binding accepted", string(bad))
		}
	}
	r.Provision.Intent.Tree = "drift"
	if err := initializeReviewProvision(&r, b); err == nil {
		t.Fatal("durable intent drift accepted")
	}
	r, b = fleetUnitRecord()
	r.State = "ready"
	if err := initializeReviewProvision(&r, b); err == nil {
		t.Fatal("ready without proof accepted")
	}
}
