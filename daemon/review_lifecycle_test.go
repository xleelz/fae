package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReviewLifecycleNativeRestartAndGates(t *testing.T)  { testReviewLifecycleNative(t, false) }
func TestDynamicAdapterReviewLifecycleRecovery(t *testing.T) { testReviewLifecycleNative(t, true) }
func testReviewLifecycleNative(t *testing.T, dynamic bool) {
	cfg, ctx, base := liveDaemonFixture(t)
	branch := liveGit(t, cfg.Workspace, "symbolic-ref", "--short", "HEAD")
	tree := liveGit(t, cfg.Workspace, "rev-parse", "HEAD^{tree}")
	var mu sync.Mutex
	posts := map[string]int{}
	receipts := map[string]external.Receipt{}
	baseline := "Pass"
	check := "Pass"
	blocking := false
	dropTool := ""
	operations := map[string]string{"create": "review_create", "read": "review_read", "publish": "review_publish", "merge": "review_merge", "revision": "review_revision", "baseline": "review_baseline"}
	schemas := map[string]external.ToolSchema{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/schemas" {
			list := []external.ToolSchema{}
			for _, s := range schemas {
				list = append(list, s)
			}
			json.NewEncoder(w).Encode(external.SchemaCatalog{Server: "fixture", Tools: list})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/reads/") {
			var i external.Intent
			if e := json.Unmarshal([]byte(r.URL.Query().Get("intent")), &i); e != nil {
				t.Error(e)
			}
			var data any
			if i.Tool == "vendor_baseline" {
				data = map[string]any{"package": "sample", "integration_tip": base, "status": baseline}
			} else {
				data = external.ReviewStatus{ReviewID: "R1", Tree: i.Tree, Build: external.ReviewAnalyzer{Name: "fleet", Status: "Pass", RequiredFor: []string{"publish"}}, Analyzers: []external.ReviewAnalyzer{{Name: "new-check", Status: check, RequiredFor: []string{"merge"}}}, Comments: []external.ReviewComment{{ID: "C1", Blocking: blocking, Addressed: false}}}
			}
			raw, _ := json.Marshal(data)
			json.NewEncoder(w).Encode(external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, Tree: i.Tree, ExternalID: "R1", State: "performed", Data: raw})
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/effects/")
		if r.Method == "GET" {
			receipt, ok := receipts[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(receipt)
			return
		}
		var i external.Intent
		json.NewDecoder(r.Body).Decode(&i)
		posts[i.Tool]++
		if i.Tool == "vendor_create" {
			if i.Arguments["is_draft"] != "true" || i.Arguments["publish_now"] != "false" || i.Arguments["merge_now"] != "false" || i.Arguments["rerun_merge"] != "false" {
				t.Error("unsafe creation flags", i.Arguments)
			}
		}
		if i.Tool != "vendor_create" && i.Arguments["review_id"] != "R1" {
			t.Error("wrong review identity", i.Arguments)
		}
		receipt := external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, Tree: i.Tree, ExternalID: "R1", State: "performed"}
		receipts[i.Key] = receipt
		if i.Tool == dropTool {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
			} else {
				conn.Close()
			}
			return
		}
		json.NewEncoder(w).Encode(receipt)
	}))
	defer server.Close()
	cfg.Live.ExternalTools = map[string]external.Tool{}
	for alias, op := range operations {
		args := map[string]string{"destination": "main", "push_namespace": "fae/"}
		if op == "review_create" || op == "review_revision" {
			args["cwd"] = "$workspace"
			args["branch"] = "$branch"
			args["package"] = "$package"
		}
		if op == "review_create" {
			args["is_draft"] = "true"
			args["publish_now"] = "false"
			args["merge_now"] = "false"
			args["rerun_merge"] = "false"
		} else if op != "review_baseline" {
			args["review_id"] = "$review"
		}
		if op == "review_revision" {
			args["revision"] = "$revision"
			args["epoch"] = "9007199254740993"
		}
		if op == "review_baseline" {
			args["package"] = "$package"
			args["integration_tip"] = "$integration_tip"
		}
		props := map[string]any{}
		for k := range args {
			typ := "string"
			if k == "revision" || k == "epoch" {
				typ = "integer"
			}
			props[k] = map[string]string{"type": typ}
		}
		input, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "additionalProperties": false})
		schema := external.ToolSchema{Name: "vendor_" + alias, InputSchema: input, OutputSchema: json.RawMessage(`{"type":"object"}`)}
		schemas[schema.Name] = schema
		cfg.Live.ExternalTools[alias] = external.Tool{Endpoint: server.URL, Server: "fixture", Name: schema.Name, Role: "planner", Arguments: args}
	}
	policy := external.ReviewPolicy{Package: "sample", Workspace: cfg.Workspace, Branch: branch, IntegrationTip: base, BaselineTool: "baseline", StatusTool: "read", BaselineApproved: true, MaxRevisions: 2, Operations: operations, ArgumentKeys: map[string]string{"workspace": "cwd", "draft": "is_draft", "auto_publish": "publish_now", "auto_merge": "merge_now", "auto_merge_with_rerun": "rerun_merge"}}
	cfg.Live.ReviewPolicies = map[string]external.ReviewPolicy{"review-task": policy}
	bundle := AdapterDocument{SchemaVersion: 1, Tools: cfg.Live.ExternalTools, ReviewPolicies: cfg.Live.ReviewPolicies}
	if dynamic {
		cfg.Live.ExternalTools = nil
		cfg.Live.ReviewPolicies = nil
	}
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	var ref AdapterBinding
	if dynamic {
		authorize(t, d)
		result, err := call(t, d, "AdapterPut", AdapterPutParams{ID: "review-adapter", Document: bundle, AuthorizingMessage: "message"}, "")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(result.Result)
		if err = json.Unmarshal(raw, &ref); err != nil {
			t.Fatal(err)
		}
	}
	selectLive := func() *liveExecutor {
		if !dynamic {
			return d.live
		}
		_, doc, err := d.resolveAdapter(ctx, ref)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.live.config.ExternalTools) != 0 {
			t.Fatal("dynamic review mutated shared config")
		}
		x := *d.live
		x.config = adapterConfig(x.config, doc)
		return &x
	}
	selected := selectLive()
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()
	if !dynamic {
		target := targetFixture(cfg.Workspace)
		target.SchemaVersion = 2
		target.Normal = &NormalTargetPolicy{Destination: "main", PushNamespace: "fae/fixture", Tools: cfg.Live.ExternalTools, ReviewTask: "review-task", Review: &policy}
		if err := validateNormalTargetShape(target); err != nil {
			t.Fatal(err)
		}
		if err := d.validateNormalTarget(ctx, target, AdmitParams{}, workflow.Document{}, true); err != nil {
			t.Fatal("versioned review target health", err)
		}

	}
	if h, e := selected.checkReviewTarget(ctx, "review-task"); e != nil || !h.Green {
		t.Fatal(h, e)
	}
	mu.Lock()
	baseline = "Fail"
	mu.Unlock()
	if _, e = selected.checkReviewTarget(ctx, "review-task"); e == nil || !strings.Contains(e.Error(), "TargetConfigurationDefect") {
		t.Fatal(e)
	}
	if !dynamic {
		target := targetFixture(cfg.Workspace)
		target.SchemaVersion = 2
		target.Normal = &NormalTargetPolicy{Destination: "main", PushNamespace: "fae/fixture", Tools: cfg.Live.ExternalTools, ReviewTask: "review-task", Review: &policy}
		if err := d.validateNormalTarget(ctx, target, AdmitParams{}, workflow.Document{}, true); err == nil {
			t.Fatal("versioned target accepted red baseline")
		}
	}
	mu.Lock()
	baseline = "Pass"
	mu.Unlock()
	env := envelope{Job: engine.Job{Disposition: domain.Disposition{Mode: domain.Normal, Live: true}}}
	sequence := 0
	var lastDispatch engine.Dispatch
	run := func(alias string) (any, error) {
		sequence++
		tool := selected.config.ExternalTools[alias]
		dispatch := engine.Dispatch{ID: fmt.Sprintf("review-task/hash/%d", sequence), TaskID: "review-task", Stage: alias, Tree: tree, Config: workflow.Stage{Subject: alias, Role: "planner", IdempotenceKey: alias, Pins: tool.Arguments}, Sessions: []engine.Session{{ID: fmt.Sprintf("session-%d", sequence), Role: "planner"}}}
		lastDispatch = dispatch
		return selected.executeReview(ctx, dispatch, env, tool)
	}
	missing := selected.config.ReviewPolicies["review-task"]
	delete(selected.config.ReviewPolicies, "review-task")
	if _, e = run("create"); e == nil {
		t.Fatal("missing workspace permitted")
	}
	selected.config.ReviewPolicies["review-task"] = missing
	for _, instance := range []bool{false, true} {
		p := missing
		if instance {
			selected.config.DisableReviewPublishMerge = true
		} else {
			p.DisablePublishMerge = true
		}
		selected.config.ReviewPolicies["review-task"] = p
		if _, e = run("publish"); e != nil {
			t.Fatal(e)
		}
		if _, e = run("merge"); e != nil {
			t.Fatal(e)
		}
		mu.Lock()
		if posts["vendor_publish"] != 0 || posts["vendor_merge"] != 0 {
			t.Fatal("disabled mutation sent")
		}
		mu.Unlock()
		selected.config.DisableReviewPublishMerge = false
		selected.config.ReviewPolicies["review-task"] = missing
	}
	// FAE-EXT-010: a vendor flag no argument key maps fails closed when it
	// would publish or merge.
	create := selected.config.ExternalTools["create"]
	flagged := create
	flagged.Arguments = maps.Clone(create.Arguments)
	flagged.Arguments["autoMergeOnGreen"] = "true"
	selected.config.ExternalTools["create"] = flagged
	if _, e = run("create"); e == nil || !strings.Contains(e.Error(), "implicit publication") {
		t.Fatal("unmapped auto-merge flag accepted", e)
	}
	selected.config.ExternalTools["create"] = create
	if _, e = run("create"); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	check = "Blocked"
	mu.Unlock()
	out, e := run("read")
	if e != nil || out.(map[string]any)["review_decision"] != "ready" {
		t.Fatal("merge-only blocked check prevented publish readiness", out, e)
	}
	if _, e = run("merge"); e == nil {
		t.Fatal("blocked merge allowed")
	} else {
		var refused *reviewRefusal
		if !errors.As(e, &refused) || !refused.wait {
			t.Fatal("known pre-mutation gate refusal lost", e)
		}
	}

	mu.Lock()
	check = "Pass"
	blocking = true
	mu.Unlock()
	out, e = run("read")
	if e != nil || out.(map[string]any)["review_decision"] != "remediation" {
		t.Fatal(out, e)
	}
	readDispatch := lastDispatch
	if _, e = run("revision"); e != nil {
		t.Fatal(e)
	}
	revisionDispatch := lastDispatch
	if _, e = run("revision"); e == nil || !strings.Contains(e.Error(), "ReviewRevisionCap") {
		t.Fatal(e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	selected = selectLive()
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()
	state, e := d.reviewCheckpoint(ctx, "review-task")
	if e != nil || state.ID != "R1" || state.Revision != 2 {
		t.Fatal(state, e)
	}
	// A started read has a durable intent and can be collected again using
	// only GET /reads after restart. It does not query/create an effect.
	readTool, _, readErr := selected.reviewRecoveryIntent(ctx, readDispatch, selected.config.ExternalTools["read"])
	if readErr != nil {
		t.Fatal(readErr)
	}
	if _, readErr = selected.executeReview(ctx, readDispatch, env, readTool); readErr != nil {
		t.Fatal(readErr)
	}
	// Restart after the checkpoint write but before workflow completion must
	// adopt the original revision receipt without allocating revision three.
	bound, intent, err := selected.reviewRecoveryIntent(ctx, revisionDispatch, selected.config.ExternalTools["revision"])
	if err != nil {
		t.Fatal(err)
	}
	receipt, found, err := external.Query(ctx, bound, intent)
	if err != nil || !found {
		t.Fatal(found, err)
	}
	if _, err = selected.finishReview(ctx, revisionDispatch, intent, receipt, state); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	blocking = false
	mu.Unlock()
	if _, e = run("publish"); e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	dropTool = "vendor_merge"
	mu.Unlock()
	if _, e = run("merge"); e == nil {
		t.Fatal("dropped response accepted")
	}
	mergeDispatch := lastDispatch
	state, e = d.reviewCheckpoint(ctx, "review-task")
	if e != nil || state.Merged {
		t.Fatal("ambiguous merge applied without query", state, e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	selected = selectLive()
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()
	bound, intent, e = selected.reviewRecoveryIntent(ctx, mergeDispatch, selected.config.ExternalTools["merge"])
	if e != nil {
		t.Fatal(e)
	}
	receipt, found, e = external.Query(ctx, bound, intent)
	if e != nil || !found {
		t.Fatal(found, e)
	}
	if _, e = selected.finishReview(ctx, mergeDispatch, intent, receipt, state); e != nil {
		t.Fatal(e)
	}
	state, e = d.reviewCheckpoint(ctx, "review-task")
	if e != nil || !state.Merged {
		t.Fatal(state, e)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, name := range []string{"vendor_create", "vendor_revision", "vendor_publish", "vendor_merge"} {
		if posts[name] != 1 {
			t.Fatal("mutation count", posts)
		}
	}
}

func TestReviewDisabledDocumentIsIdentical(t *testing.T) {
	doc := workflow.Document{Start: "a", Stages: map[string]workflow.Stage{"a": {Primitive: workflow.Gate}, "publish": {Primitive: workflow.ExternalEffect, Subject: "p"}, "merge": {Primitive: workflow.ExternalEffect, Subject: "m"}}, Edges: []workflow.Edge{{From: "a", To: "publish"}, {From: "publish", To: "merge"}, {From: "merge", To: "Done"}}}
	c := LiveConfig{DisableReviewPublishMerge: true, ExternalTools: map[string]external.Tool{"p": {Name: "review_publish"}, "m": {Name: "review_merge"}}}
	actual, e := reviewDocument(c, "task", doc)
	if e != nil {
		t.Fatal(e)
	}
	expected := workflow.Document{Start: "a", Stages: map[string]workflow.Stage{"a": {Primitive: workflow.Gate}}, Edges: []workflow.Edge{{From: "a", To: "Done"}}}
	a, _ := json.Marshal(actual)
	b, _ := json.Marshal(expected)
	if string(a) != string(b) {
		t.Fatal(string(a), string(b))
	}
}

var _ = context.Background

func TestReviewEngineMergeBarrierAndRemediation(t *testing.T) {
	for _, tc := range []struct {
		name, operation, decision, id string
		merged                        bool
		required                      bool
		revision                      int
		want                          domain.State
		reason                        string
	}{
		{name: "missing merge stays out of Done", operation: "review_read", decision: "ready", id: "R1", required: true, revision: 1, want: domain.Escalated, reason: "ReviewMergeRequired"},
		{name: "successful merge delivers", operation: "review_merge", decision: "performed", id: "R1", merged: true, required: true, revision: 1, want: domain.Done},
		{name: "missing merge receipt fails", operation: "review_merge", decision: "performed", id: "R1", required: true, revision: 1, want: "external", reason: "ReviewGateFailed"},
		{name: "changed review identity fails", operation: "review_merge", decision: "performed", id: "R2", merged: true, required: true, revision: 1, want: "external", reason: "ReviewGateFailed"},
		{name: "blocking comment routes implementer", operation: "review_read", decision: "remediation", id: "R1", revision: 1, want: "implement-loop"},
		{name: "revision exhaustion escalates", operation: "review_read", decision: "remediation", id: "R1", revision: 5, want: domain.Escalated, reason: "ReviewRevisionCap"},
		{name: "pending does not advance", operation: "review_read", decision: "waiting", id: "R1", revision: 1, want: "external", reason: "ReviewPending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := externalDocument()
			doc.Artifacts["ExternalReceipt"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object"}`)}
			doc.Edges = append(doc.Edges, workflow.Edge{From: "external", To: "implement-loop"})
			stage := doc.Stages["external"]
			stage.Failure = "implement-loop"
			doc.Stages["external"] = stage
			j, e := engine.NewLive("review-engine", 1, doc, "tree")
			if e != nil {
				t.Fatal(e)
			}
			j.Disposition = domain.Disposition{Mode: domain.Normal, Live: true, Destination: "main", PushNamespace: "fae/"}
			runner := engine.NewScripted(defaults.HappyScript())
			for n := 0; n < 150; n++ {
				next, dispatch, _, err := engine.Prepare(j)
				if err != nil {
					t.Fatal(err)
				}
				j = next
				if dispatch == nil {
					continue
				}
				if dispatch.Stage == "external" {
					break
				}
				r, err := runner.Execute(context.Background(), *dispatch)
				if err != nil {
					t.Fatal(err)
				}
				r.ExecutionKind = "live"
				j, _, err = engine.Complete(j, dispatch.ID, r)
				if err != nil {
					t.Fatal(err)
				}
			}
			if j.InFlight == nil || j.InFlight.Stage != "external" {
				t.Fatal("did not reach external", j.State)
			}
			j.ReviewStages = map[string]string{"external": tc.operation}
			j.ReviewMergeRequired = tc.required
			j.ReviewID = "R1"
			j.ReviewRevision = tc.revision
			j.ReviewMaxRevisions = 5
			j.ReviewPollCount = 2
			j.ReviewMaxPolls = 5
			j.ReviewRemediation = "implement-loop"
			dispatch := *j.InFlight
			raw, _ := json.Marshal(map[string]any{"review_commit": "verified-committed-head", "review_id": tc.id, "review_merged": tc.merged, "review_decision": tc.decision, "reconciliation_state": "reconciled"})
			result := engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: j.TaskID, Tree: j.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now(), Output: raw}
			j, events, e := engine.Complete(j, dispatch.ID, result)
			if e != nil {
				t.Fatal(e)
			}
			if j.State != tc.want || tc.reason != "" && !strings.Contains(j.Reason, tc.reason) {
				t.Fatal(j.State, j.Reason)
			}
			if tc.name == "blocking comment routes implementer" || tc.name == "successful merge delivers" || tc.name == "missing merge stays out of Done" {
				if j.ReviewPollCount != 0 {
					t.Fatal("ready/progress did not reset poll budget", j.ReviewPollCount)
				}
			}
			if tc.name == "pending does not advance" && j.ReviewPollCount != 3 {
				t.Fatal("pending observation not counted", j.ReviewPollCount)
			}
			if tc.want == domain.Done {
				found := false
				for _, event := range events {
					if event.Kind == "review-merged" {
						found = true
					}
				}
				if !found || !j.ReviewMerged {
					t.Fatal("merge event absent")
				}
			}
		})
	}
}

func TestReviewCheckpointKeysAreTaskScoped(t *testing.T) {
	if reviewKey("alpha") == reviewKey("beta") || reviewKey("alpha") == "review-lifecycle/" {
		t.Fatal("review checkpoint task collision")
	}
}
