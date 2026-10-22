package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
)

func TestReviewWorkspaceGuard(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			if CanPrepareReviewWorkspace(a, b) != (a && b) {
				t.Fatal(a, b)
			}
		}
	}
}
func TestReviewWorkspaceNativePrepareRestartAndEffects(t *testing.T) {
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
	task := "prepared-review"
	p := ReviewWorkspaceParams{Task: task, RunID: "prepared-run", Adapter: AdapterBinding{ID: "review"}, AuthorizingMessage: "message"}
	plan, err := d.reviewWorkspacePlan(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(plan.Workspace); !os.IsNotExist(err) {
		t.Fatal("plan mutated filesystem", err)
	}
	var mu sync.Mutex
	baseline := "Fail"
	reads, posts := 0, 0
	receipts := map[string]external.Receipt{}
	schemas := []external.ToolSchema{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/schemas" {
			json.NewEncoder(w).Encode(external.SchemaCatalog{Server: "fixture", Tools: schemas})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/reads/") {
			reads++
			var i external.Intent
			json.Unmarshal([]byte(r.URL.Query().Get("intent")), &i)
			raw, _ := json.Marshal(map[string]string{"package": "sample", "integration_tip": plan.IntegrationTip, "status": baseline})
			json.NewEncoder(w).Encode(external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, Tree: i.Tree, ExternalID: "R1", State: "performed", Data: raw})
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/effects/")
		if r.Method == "GET" {
			v, ok := receipts[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(v)
			return
		}
		var i external.Intent
		if err := json.NewDecoder(r.Body).Decode(&i); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if i.Arguments["workspace"] != plan.Workspace || i.Arguments["branch"] != plan.Branch || i.Arguments["package"] != "sample" {
			t.Error("effect escaped prepared binding", i.Arguments)
		}
		posts++
		v := external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, Tree: i.Tree, ExternalID: "R1", State: "performed"}
		receipts[key] = v
		json.NewEncoder(w).Encode(v)
	}))
	defer server.Close()
	tools := map[string]external.Tool{}
	for _, name := range []string{"review_baseline", "review_read", "review_create", "review_revision"} {
		args := map[string]string{"destination": "main", "push_namespace": "fae/"}
		switch name {
		case "review_baseline":
			args["package"] = "$package"
			args["integration_tip"] = "$integration_tip"
		case "review_read":
			args["review_id"] = "$review"
		default:
			args["workspace"] = "$workspace"
			args["branch"] = "$branch"
			args["package"] = "$package"
			if name == "review_create" {
				args["draft"] = "true"
			} else {
				args["review_id"] = "$review"
				args["revision"] = "$revision"
			}
		}
		props := map[string]any{}
		for k := range args {
			typ := "string"
			if k == "revision" {
				typ = "integer"
			}
			props[k] = map[string]string{"type": typ}
		}
		schema, _ := json.Marshal(map[string]any{"type": "object", "additionalProperties": false, "properties": props})
		schemas = append(schemas, external.ToolSchema{Name: name, InputSchema: schema, OutputSchema: json.RawMessage(`{"type":"object"}`)})
		tools[name] = external.Tool{Endpoint: server.URL, Server: "fixture", Name: name, Role: "planner", Arguments: args}
	}
	policy := external.ReviewPolicy{Package: "sample", Workspace: plan.Workspace, Branch: plan.Branch, IntegrationTip: plan.IntegrationTip, BaselineTool: "review_baseline", StatusTool: "review_read", BaselineApproved: true, MaxRevisions: 3}
	bundle := AdapterDocument{SchemaVersion: 1, Tools: tools, ReviewPolicies: map[string]external.ReviewPolicy{task: policy}}
	if _, err = call(t, d, "AdapterPut", AdapterPutParams{ID: "review", Document: bundle, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	// Refuse branch mismatch before any workspace or adapter read.
	wrong := p
	wrong.RunID = "other-run"
	if _, err = call(t, d, "ReviewWorkspacePrepare", wrong, ""); err == nil {
		t.Fatal("foreign binding accepted")
	}
	if reads != 0 {
		t.Fatal("binding refusal called adapter")
	}
	if _, err = call(t, d, "ReviewWorkspacePrepare", p, ""); err == nil || !strings.Contains(err.Error(), "baseline") {
		t.Fatal("red baseline accepted", err)
	}
	pending, err := d.reviewWorkspaceRecord(ctx, task)
	if err != nil || pending.State != "pending" {
		t.Fatal(pending, err)
	}
	if _, err = os.Stat(plan.Workspace); err != nil {
		t.Fatal("pending workspace missing", err)
	}
	cancelledPrepare, cancelPrepare := context.WithCancel(ctx)
	cancelPrepare()
	if _, err = d.prepareReviewWorkspace(cancelledPrepare, p, controlplane.Audit{Actor: "operator", Action: "test cancelled prepare", MessageID: "message", Outcome: "refused"}); err == nil {
		t.Fatal("cancelled preparation accepted")
	}
	pending, err = d.reviewWorkspaceRecord(ctx, task)
	if err != nil || pending.State != "pending" {
		t.Fatal("cancelled preparation published ready", pending, err)
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
	baseline = "Pass"
	mu.Unlock()
	result, err := call(t, d, "ReviewWorkspacePrepare", p, "")
	if err != nil {
		t.Fatal(err)
	}
	var ready ReviewWorkspaceRecord
	if err = json.Unmarshal(result.Result, &ready); err != nil || ready.State != "ready" || !ready.Health.Green {
		t.Fatal(ready, err)
	}
	if _, err = call(t, d, "ReviewWorkspacePrepare", p, ""); err != nil {
		t.Fatal("idempotent retry", err)
	}
	target := targetFixture(cfg.Workspace)
	target.SchemaVersion = 3
	target.IntegrationRef = cfg.Live.IntegrationRef
	target.ProtectedBranches = []string{cfg.Live.IntegrationRef}
	target.Normal = &NormalTargetPolicy{Adapter: &ready.Adapter, Tools: tools, Destination: "main", PushNamespace: "fae/", ReviewTask: task, Review: &policy}
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "review-target", Document: target, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	// A ready database record is insufficient if its physical worktree disappeared.
	if err = os.Rename(plan.Workspace, plan.Workspace+"-moved"); err != nil {
		t.Fatal(err)
	}
	lost := AdmitParams{ID: task, RunID: p.RunID, Mode: "Normal", TargetConfig: "review-target", TargetRevision: 1, Destination: "main", PushNamespace: "fae/", Content: "must refuse", AuthorizingMessage: "message"}
	if _, err = call(t, d, "Admit", lost, task); err == nil {
		t.Fatal("missing ready workspace recreated in admission")
	}
	if _, err = os.Stat(plan.Workspace); !os.IsNotExist(err) {
		t.Fatal("admission created lost workspace", err)
	}
	if _, err = call(t, d, "ReviewWorkspacePrepare", p, ""); err == nil {
		t.Fatal("ready retry recreated missing workspace")
	}
	if err = os.Rename(plan.Workspace+"-moved", plan.Workspace); err != nil {
		t.Fatal(err)
	}
	foreign := ready
	foreign.Workspace = cfg.Workspace
	if d.validatePreparedWorkspace(ctx, foreign) == nil {
		t.Fatal("foreign prepared path accepted")
	}
	foreign = ready
	foreign.Branch = "main"
	if d.validatePreparedWorkspace(ctx, foreign) == nil {
		t.Fatal("foreign prepared branch accepted")
	}
	admission := AdmitParams{ID: task, RunID: p.RunID, Mode: "Normal", TargetConfig: "review-target", TargetRevision: 1, Destination: "main", PushNamespace: "fae/", Content: "test prepared binding", AuthorizingMessage: "message"}
	if _, err = call(t, d, "Admit", admission, task); err != nil {
		t.Fatal(err)
	}
	var admitted envelope
	if err = decodeEnvelope(d.jobs[task].Payload, &admitted); err != nil {
		t.Fatal(err)
	}
	selected, err := d.liveForAdmission(ctx, admitted.Admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := d.live.config.ReviewPolicies[task]; ok {
		t.Fatal("shared policy mutated")
	}
	env := envelope{Job: engine.Job{Disposition: domain.Disposition{Mode: domain.Normal, Live: true}}}
	tree := liveGit(t, plan.Workspace, "rev-parse", "HEAD^{tree}")
	for i, alias := range []string{"review_create", "review_revision"} {
		tool := selected.config.ExternalTools[alias]
		dispatch := engine.Dispatch{ID: fmt.Sprintf("%s/hash/%d", task, i), TaskID: task, Stage: alias, Tree: tree, Config: workflow.Stage{Subject: alias, Role: "planner", Pins: tool.Arguments, IdempotenceKey: alias}, Sessions: []engine.Session{{ID: fmt.Sprintf("session-%d", i), Role: "planner"}}}
		if _, err = selected.executeReview(ctx, dispatch, env, tool); err != nil {
			t.Fatal(alias, err)
		}
	}
	mu.Lock()
	if posts != 2 {
		t.Fatal("review calls", posts)
	}
	mu.Unlock()

	// A legitimate retirement archives/removes the prepared workspace; startup
	// retains immutable policy checks without requiring a deliberately removed tree.
	for _, control := range []struct{ method, reason string }{{"Escalate", "operator review"}, {"Retire", "obsolete ruling/prepared-review"}} {
		if _, err = call(t, d, control.method, TaskControlParams{ID: task, Reason: control.reason, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(control.method, err)
		}
	}
	response, err := call(t, d, "PlaneBackup", CleanupParams{AuthorizingMessage: "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var backup planeBackup
	if err = json.Unmarshal(response.Result, &backup); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "CleanupRetired", CleanupParams{Apply: true, Backup: backup.Path, PaceMS: 10, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(plan.Workspace); !os.IsNotExist(err) {
		t.Fatal("retired workspace not removed", err)
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal("retired dynamic target restart", err)
	}
	if d.jobs[task].State != "Retired" {
		t.Fatal("retired state lost")
	}
	if err = d.checkPinnedTarget(ctx, admitted.Admission, admitted.Job.Document); err == nil {
		t.Fatal("ordinary target validation bypassed physical check")
	}
	tampered := admitted.Admission
	tampered.TargetHash = strings.Repeat("0", 64)
	if err = d.checkPinnedTarget(context.WithValue(ctx, retiredTargetRecoveryKey{}, true), tampered, admitted.Job.Document); err == nil {
		t.Fatal("retired startup ignored immutable target hash")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = d.reviewWorkspacePlan(cancelled, p); err == nil {
		t.Fatal("cancelled preparation accepted")
	}
}
