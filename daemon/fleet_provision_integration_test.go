package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/external"
)

func TestFleetProvisionNativeLostAckRetireRestartAdopt(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	p := ReviewWorkspaceParams{Task: "fleet-bound", RunID: "fleet-run", Adapter: AdapterBinding{ID: "fleet"}, AuthorizingMessage: "message"}
	plan, err := d.reviewWorkspacePlan(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	posts, queries, baselineReads := 0, 0, 0
	green := false
	hideReceipt := false
	wrongReceipt := false
	receipts := map[string]external.Receipt{}
	schemas := []external.ToolSchema{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/schemas" {
			json.NewEncoder(w).Encode(external.SchemaCatalog{Server: "fleetfixture", Tools: schemas})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/reads/") {
			baselineReads++
			var i external.Intent
			json.Unmarshal([]byte(r.URL.Query().Get("intent")), &i)
			status := "Fail"
			if green {
				status = "Pass"
			}
			data, _ := json.Marshal(map[string]string{"package": "sample", "integration_tip": plan.IntegrationTip, "status": status})
			json.NewEncoder(w).Encode(external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, Tree: i.Tree, ExternalID: "baseline", State: "performed", Data: data})
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/effects/")
		if r.Method == "GET" {
			queries++
			receipt, ok := receipts[key]
			if !ok || hideReceipt {
				w.WriteHeader(404)
				return
			}
			if wrongReceipt {
				receipt.Data = json.RawMessage(`{"package":"foreign","workspace":"foreign","branch":"foreign","integration_tip":"foreign"}`)
			}
			json.NewEncoder(w).Encode(receipt)
			return
		}
		var i external.Intent
		if json.NewDecoder(r.Body).Decode(&i) != nil {
			w.WriteHeader(400)
			return
		}
		posts++
		data, _ := json.Marshal(map[string]string{"package": "sample", "workspace": plan.Workspace, "branch": plan.Branch, "integration_tip": plan.IntegrationTip})
		receipts[key] = external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, Tree: i.Tree, ExternalID: "fleet-workspace", State: "performed", Data: data}
		// The vendor performed the operation but its acknowledgment was lost.
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	tools := map[string]external.Tool{}
	for _, name := range []string{"fleet_prepare", "review_baseline", "review_read"} {
		args := map[string]string{}
		switch name {
		case "fleet_prepare":
			args = map[string]string{"package": "sample", "workspace": plan.Workspace, "branch": plan.Branch, "integration_tip": plan.IntegrationTip}
		case "review_baseline":
			args = map[string]string{"package": "$package", "integration_tip": "$integration_tip"}
		case "review_read":
			args = map[string]string{"review_id": "$review"}
		}
		props := map[string]any{}
		for k := range args {
			props[k] = map[string]string{"type": "string"}
		}
		schema, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "properties": props})
		schemas = append(schemas, external.ToolSchema{Name: name, InputSchema: schema, OutputSchema: json.RawMessage(`{"type":"object"}`)})
		tools[name] = external.Tool{Endpoint: srv.URL, Server: "fleetfixture", Name: name, Role: "planner", Arguments: args}
	}
	tools["fleet_alias"] = tools["fleet_prepare"]
	slashTool := tools["fleet_prepare"]
	slashTool.Endpoint += "///"
	tools["fleet_slash_alias"] = slashTool
	policy := external.ReviewPolicy{Package: "sample", Workspace: plan.Workspace, Branch: plan.Branch, IntegrationTip: plan.IntegrationTip, BaselineTool: "review_baseline", StatusTool: "review_read", BaselineApproved: true, MaxRevisions: 3, ProvisionTool: "fleet_prepare"}
	doc := AdapterDocument{SchemaVersion: 1, Tools: tools, ReviewPolicies: map[string]external.ReviewPolicy{p.Task: policy}}
	if _, err = call(t, d, "AdapterPut", AdapterPutParams{ID: "fleet", Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	// An installed fleet capability remains unavailable to workflow stages,
	// even through a second alias of the exact same server operation.
	b, _, err := d.resolveAdapter(ctx, p.Adapter)
	if err != nil {
		t.Fatal(err)
	}
	target := targetFixture(cfg.Workspace)
	target.SchemaVersion = 3
	target.IntegrationRef = cfg.Live.IntegrationRef
	target.ProtectedBranches = []string{cfg.Live.IntegrationRef}
	target.Normal = &NormalTargetPolicy{Adapter: &b, Destination: "main", PushNamespace: "fae/fleet", Tools: tools}
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "fleet-stage-target", Document: target, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	for _, alias := range []string{"fleet_prepare", "fleet_alias", "fleet_slash_alias"} {
		workflow := externalDocument()
		workflow.ID = "forbidden-" + alias
		stage := workflow.Stages["external"]
		stage.Subject = alias
		stage.Pins = tools[alias].Arguments
		workflow.Stages["external"] = stage
		raw, _ := json.Marshal(workflow)
		if _, err = d.store.WriteWorkflow(ctx, controlplane.Audit{Actor: "operator", Action: "test workflow", Outcome: "ok"}, workflow.ID, workflow.ID, raw, validWorkflow); err != nil {
			t.Fatal(err)
		}
		admission := AdmitParams{ID: "forbidden-" + alias, Mode: "Normal", WorkflowID: workflow.ID, TargetConfig: "fleet-stage-target", Destination: "main", PushNamespace: "fae/fleet", Content: "must refuse fleet hot path", AuthorizingMessage: "message"}
		if _, err = call(t, d, "Admit", admission, admission.ID); err == nil || !strings.Contains(err.Error(), "provision") {
			t.Fatal("fleet stage not explicitly rejected", alias, err)
		}
	}
	mu.Lock()
	beforePrepare := posts
	mu.Unlock()
	if beforePrepare != 0 {
		t.Fatal("hot path provisioned fleet")
	}
	unauth := p
	unauth.AuthorizingMessage = ""
	if _, err = call(t, d, "ReviewWorkspacePrepare", unauth, ""); err == nil {
		t.Fatal("unauthorized fleet provisioning accepted")
	}
	if _, err = d.reviewWorkspaceRecord(ctx, p.Task); err == nil {
		t.Fatal("unauthorized preparation persisted intent")
	}
	if _, err = call(t, d, "ReviewWorkspacePrepare", p, ""); err == nil {
		t.Fatal("lost acknowledgment published ready")
	}
	pending, err := d.reviewWorkspaceRecord(ctx, p.Task)
	if err != nil || pending.State != "pending" || pending.Provision == nil || pending.Provision.Receipt != nil {
		t.Fatal("missing durable uncertain intent", err)
	}
	mu.Lock()
	gotPosts, gotBaseline := posts, baselineReads
	mu.Unlock()
	if gotPosts != 1 || gotBaseline != 0 {
		t.Fatal("baseline ran before durable provisioning receipt", gotPosts, gotBaseline)
	}
	if _, err = d.reviewWorkspaceHealth(ctx, p.Task); err == nil {
		t.Fatal("uncertain fleet intent reported healthy")
	}
	mu.Lock()
	hideReceipt = true
	mu.Unlock()
	if _, err = call(t, d, "ReviewWorkspacePrepare", p, ""); err == nil {
		t.Fatal("absent receipt authorized replay")
	}
	mu.Lock()
	hideReceipt = false
	gotPosts = posts
	mu.Unlock()
	if gotPosts != 1 {
		t.Fatal("404 retried fleet mutation")
	}
	if _, err = call(t, d, "AdapterRetire", map[string]string{"id": "fleet", "reason": "new vendor suspended", "authorizing_message": "message"}, ""); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	d.driveStop.Store(true)
	d.signal()
	mu.Lock()
	if posts != 1 || baselineReads != 0 {
		t.Error("startup provisioned or baseline-checked workspace")
	}
	mu.Unlock()
	mu.Lock()
	wrongReceipt = true
	mu.Unlock()
	if _, err = call(t, d, "ReviewWorkspacePrepare", p, ""); err == nil {
		t.Fatal("foreign binding receipt adopted")
	}
	mu.Lock()
	wrongReceipt = false
	gotBaseline = baselineReads
	mu.Unlock()
	if gotBaseline != 0 {
		t.Fatal("foreign receipt reached baseline")
	}
	if _, err = call(t, d, "ReviewWorkspacePrepare", p, ""); err == nil {
		t.Fatal("red baseline published ready")
	}
	pending, err = d.reviewWorkspaceRecord(ctx, p.Task)
	if err != nil || pending.Provision == nil || pending.Provision.Receipt == nil || pending.State != "pending" {
		t.Fatal("adopted receipt not durable", err)
	}
	mu.Lock()
	green = true
	mu.Unlock()
	if _, err = call(t, d, "ReviewWorkspacePrepare", p, ""); err != nil {
		t.Fatal("retired pending binding failed completion", err)
	}
	ready, err := d.reviewWorkspaceRecord(ctx, p.Task)
	if err != nil || ready.State != "ready" {
		t.Fatal("no ready binding", err)
	}
	mu.Lock()
	gotPosts, gotQueries := posts, queries
	mu.Unlock()
	if gotPosts != 1 || gotQueries < 1 {
		t.Fatal("replayed mutation or missed recovery query", gotPosts, gotQueries)
	}
	fresh := p
	fresh.Task = "new-fleet-task"
	freshPlan, err := d.reviewWorkspacePlan(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "ReviewWorkspacePrepare", fresh, ""); err == nil {
		t.Fatal("retired adapter started new preparation")
	}
	if _, err = os.Stat(freshPlan.Workspace); !os.IsNotExist(err) {
		t.Fatal("retired preparation created worktree", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 {
		t.Fatal("new preparation mutated fleet")
	}
}
