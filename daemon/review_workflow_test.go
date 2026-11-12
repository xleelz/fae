package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// This uses the production daemon drive, actual bridge source edits, host tests,
// VCS commits and integration twice, against a pre-provisioned task workspace.
func TestReviewWorkflowActualRemediationAndRevision(t *testing.T) {
	cfg, _, base := liveDaemonFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	layout, e := Paths(cfg.Root, cfg.Instance)
	if e != nil {
		t.Fatal(e)
	}
	manager := worktree.Manager{Repository: cfg.Workspace, Root: layout.Worktrees}
	binding, e := manager.Ensure(ctx, "review-run", "review-delivery", base)
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	receipts := map[string]external.Receipt{}
	posts := map[string]int{}
	revision := 0
	tools := map[string]external.Tool{}
	schemas := []external.ToolSchema{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/schemas" {
			json.NewEncoder(w).Encode(external.SchemaCatalog{Server: "review-fixture", Tools: schemas})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/reads/") {
			var i external.Intent
			json.Unmarshal([]byte(r.URL.Query().Get("intent")), &i)
			var data any
			if i.Tool == "review_baseline" {
				data = map[string]string{"package": "sample", "integration_tip": i.Arguments["integration_tip"], "status": "Pass"}
			} else {
				data = external.ReviewStatus{ReviewID: "one-review", Tree: i.Tree, Build: external.ReviewAnalyzer{Name: "build", Status: "Pass", RequiredFor: []string{"publish"}}, Analyzers: []external.ReviewAnalyzer{{Name: "dynamic-check", Status: "Pass", RequiredFor: []string{"merge"}}}, Comments: []external.ReviewComment{{ID: "automated-comment", Blocking: true, Addressed: revision >= 2}}}
			}
			raw, _ := json.Marshal(data)
			json.NewEncoder(w).Encode(external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, ExternalID: "one-review", State: "performed", Tree: i.Tree, Data: raw})
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
		json.NewDecoder(r.Body).Decode(&i)
		posts[i.Tool]++
		if i.Tool == "review_create" {
			revision = 1
		}
		if i.Tool == "review_revision" {
			revision = 2
			if i.Arguments["review_id"] != "one-review" {
				t.Error("revision changed review")
			}
		}
		if i.Tool == "review_create" || i.Tool == "review_revision" {
			if i.Arguments["workspace"] != binding.Path || i.Arguments["branch"] != binding.Branch {
				t.Error("workspace not pinned")
			}
		}
		receipt := external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, ExternalID: "one-review", State: "performed", Tree: i.Tree}
		receipts[i.Key] = receipt
		json.NewEncoder(w).Encode(receipt)
	}))
	defer server.Close()
	for _, name := range []string{"review_create", "review_read", "review_revision", "review_publish", "review_merge", "review_baseline"} {
		args := map[string]string{"destination": "main", "push_namespace": "fae/"}
		if name == "review_create" || name == "review_revision" {
			args["workspace"] = "$workspace"
			args["branch"] = "$branch"
			args["package"] = "$package"
		}
		if name == "review_create" {
			args["draft"] = "true"
		} else if name != "review_baseline" {
			args["review_id"] = "$review"
		}
		if name == "review_revision" {
			args["revision"] = "$revision"
		}
		if name == "review_baseline" {
			args["package"] = "$package"
			args["integration_tip"] = "$integration_tip"
		}
		props := map[string]any{}
		for k := range args {
			typ := "string"
			if k == "revision" {
				typ = "integer"
			}
			props[k] = map[string]string{"type": typ}
		}
		input, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "additionalProperties": false})
		schemas = append(schemas, external.ToolSchema{Name: name, InputSchema: input, OutputSchema: json.RawMessage(`{"type":"object"}`)})
		tools[name] = external.Tool{Endpoint: server.URL, Server: "review-fixture", Name: name, Role: "planner", Arguments: args}
	}
	cfg.Live.ExternalTools = tools
	cfg.Live.ReviewPolicies = map[string]external.ReviewPolicy{"review-delivery": {Package: "sample", Workspace: binding.Path, Branch: binding.Branch, IntegrationTip: base, BaselineTool: "review_baseline", StatusTool: "review_read", BaselineApproved: true, RemediationStage: "implement-loop"}}
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	authorize(t, d)
	if e = d.store.RecordMessage(ctx, "review-live", "I approve live external effects for the isolated review lifecycle fixture"); e != nil {
		t.Fatal(e)
	}
	doc := defaults.Workflow()
	doc.Artifacts["ReviewReceipt"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
	role := doc.Roles["planner"]
	role.Outputs = append(role.Outputs, "ReviewReceipt")
	role.Profile.Effects = []string{"review_create", "review_read", "review_revision", "review_publish", "review_merge"}
	doc.Roles["planner"] = role
	for id, s := range doc.Stages {
		if s.Primitive == workflow.HostCommand && s.TestRun {
			s.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[id] = s
		}
	}
	for i, edge := range doc.Edges {
		if edge.To == "Done" {
			doc.Edges[i].To = "review_create"
		}
	}
	for _, name := range []string{"review_create", "review_read", "review_revision", "review_publish", "review_merge"} {
		doc.Stages[name] = workflow.Stage{Primitive: workflow.ExternalEffect, Role: "planner", NormalOnly: true, Subject: name, Pins: tools[name].Arguments, IdempotenceKey: name, Output: "ReviewReceipt"}
	}
	create := doc.Stages["review_create"]
	create.Failure = "review_revision"
	doc.Stages["review_create"] = create
	read := doc.Stages["review_read"]
	read.Failure = "implement-loop"
	doc.Stages["review_read"] = read
	doc.Edges = append(doc.Edges, workflow.Edge{From: "review_create", To: "review_read"}, workflow.Edge{From: "review_create", To: "review_revision"}, workflow.Edge{From: "review_revision", To: "review_read"}, workflow.Edge{From: "review_read", To: "review_publish"}, workflow.Edge{From: "review_read", To: "implement-loop"}, workflow.Edge{From: "review_publish", To: "review_merge"}, workflow.Edge{From: "review_merge", To: "Done"})
	if _, e = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "Admit", AdmitParams{ID: "review-delivery", RunID: "review-run", Mode: "Normal", CommitExternal: true, Destination: "main", PushNamespace: "fae/", AuthorizingMessage: "review-live"}, "review-delivery"); e != nil {
		t.Fatal(e)
	}
	snapshot, _ := awaitJob(t, d, "review-delivery", 150*time.Second, func(s controlplane.JobSnapshot, _ envelope) bool {
		return s.State == "Done" || s.State == "Escalated"
	})
	var env envelope
	if e = json.Unmarshal(snapshot.Payload, &env); e != nil {
		t.Fatal(e)
	}
	if env.Job.State != domain.Done || env.Job.Counts["implement"] != 2 || env.Job.Counts["run-tests"] < 2 || !env.Job.ReviewMerged || env.Job.ReviewID != "one-review" || env.Job.ReviewRevision != 2 {
		t.Fatal(env.Job.State, env.Job.Reason, env.Job.Counts, env.Job.ReviewID, env.Job.ReviewRevision)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts["review_create"] != 1 || posts["review_revision"] != 1 || posts["review_publish"] != 1 || posts["review_merge"] != 1 {
		t.Fatal(posts)
	}
}
