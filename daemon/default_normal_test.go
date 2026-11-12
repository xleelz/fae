package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

// normalVendor is a localhost review and ticket service for the shipped
// default's Normal sub-workflow. Vendor tool names differ from the content's
// subjects, so operations come from the subjects, never from tool names.
type normalVendor struct {
	mu         sync.Mutex
	posts      []external.Intent
	requests   []string
	receipts   map[string]external.Receipt
	revision   int
	published  bool
	merged     bool
	mergeCheck string
	remediate  bool
	refuse     string // a tool whose effect call the vendor refuses (403)
}

func (v *normalVendor) count(tool string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	n := 0
	for _, i := range v.posts {
		if i.Tool == tool {
			n++
		}
	}
	return n
}

func (v *normalVendor) handler(t *testing.T, schemas []external.ToolSchema) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		defer v.mu.Unlock()
		if r.URL.Path == "/schemas" {
			json.NewEncoder(w).Encode(external.SchemaCatalog{Server: "vendor", Tools: schemas})
			return
		}
		v.requests = append(v.requests, r.Method+" "+r.URL.Path)
		if strings.HasPrefix(r.URL.Path, "/rooms/") {
			action := func(name, tool, phase string) external.RoomAction {
				return external.RoomAction{Name: name, Tool: tool, Arguments: map[string]any{"phase": phase}}
			}
			json.NewEncoder(w).Encode(external.RoomResources{Server: "vendor", Room: "room-9", Actions: []external.RoomAction{action("open", "vendor_ticket_new", "open"), action("in-review", "vendor_ticket_change", "review"), action("closed", "vendor_ticket_change", "closed")}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/reads/") {
			var i external.Intent
			if err := json.Unmarshal([]byte(r.URL.Query().Get("intent")), &i); err != nil {
				t.Error(err)
			}
			var data any
			if i.Tool == "review_baseline" {
				data = map[string]string{"package": "sample", "integration_tip": i.Arguments["integration_tip"], "status": "Pass"}
			} else {
				if i.Arguments["review_id"] != "R-1" {
					t.Error("status read of another review", i.Arguments)
				}
				data = external.ReviewStatus{ReviewID: "R-1", Tree: i.Tree, Build: external.ReviewAnalyzer{Name: "fleet-build", Status: "Pass", RequiredFor: []string{"publish"}}, Analyzers: []external.ReviewAnalyzer{{Name: "merge-check", Status: v.mergeCheck, RequiredFor: []string{"merge"}}}, Comments: []external.ReviewComment{{ID: "auto-1", Blocking: v.remediate, Addressed: v.revision >= 2}}}
			}
			raw, _ := json.Marshal(data)
			json.NewEncoder(w).Encode(external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, ExternalID: "R-1", State: "performed", Tree: i.Tree, Data: raw})
			return
		}
		key := strings.TrimPrefix(r.URL.Path, "/effects/")
		if r.Method == "GET" {
			receipt, ok := v.receipts[key]
			if !ok {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(receipt)
			return
		}
		var i external.Intent
		if err := json.NewDecoder(r.Body).Decode(&i); err != nil {
			t.Error(err)
		}
		v.posts = append(v.posts, i)
		if i.Tool == v.refuse {
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, ExternalID: "refusal-7", State: "refused", Tree: i.Tree})
			return
		}
		id := "R-1"
		switch i.Tool {
		case "vendor_ticket_new", "vendor_ticket_change":
			id = "T-1"
		case "vendor_review_create":
			v.revision = 1
		case "vendor_review_revision":
			v.revision++
		case "vendor_review_publish":
			v.published = true
		case "vendor_review_merge":
			v.merged = true
		}
		if strings.HasPrefix(i.Tool, "vendor_review_") && i.Tool != "vendor_review_create" && i.Arguments["review_id"] != "R-1" {
			t.Error("review mutation on another review", i.Tool, i.Arguments)
		}
		receipt := external.Receipt{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, ExternalID: id, State: "performed", Tree: i.Tree}
		v.receipts[i.Key] = receipt
		json.NewEncoder(w).Encode(receipt)
	}
}

type normalRun struct {
	task                    string
	live                    bool
	instanceOptOut, taskOpt bool
	mergeCheck              string
	remediate               bool
	maxPolls                int
}

// defaultNormalFixture starts a live daemon whose installed default is the
// shipped content, with the vendor installed under the content's subjects.
func defaultNormalFixture(t *testing.T, run normalRun) (*Daemon, Config, context.Context, *normalVendor) {
	cfg, ctx, base := liveDaemonFixture(t)
	layout, e := Paths(cfg.Root, cfg.Instance)
	if e != nil {
		t.Fatal(e)
	}
	manager := worktree.Manager{Repository: cfg.Workspace, Root: layout.Worktrees}
	binding, e := manager.Ensure(ctx, "normal-run", run.task, base)
	if e != nil {
		t.Fatal(e)
	}
	vendor := &normalVendor{receipts: map[string]external.Receipt{}, mergeCheck: run.mergeCheck, remediate: run.remediate}
	common := map[string]string{"destination": "main", "push_namespace": "fae/"}
	args := map[string]map[string]string{
		defaults.TicketCreate:   {"target": "main", "space": "fae/"},
		defaults.TicketUpdate:   {"target": "main", "space": "fae/"},
		defaults.ReviewCreate:   {"workspace": "$workspace", "branch": "$branch", "package": "$package", "draft": "true", "auto_publish": "false", "auto_merge": "false", "auto_merge_with_rerun": "false"},
		defaults.ReviewRevision: {"workspace": "$workspace", "branch": "$branch", "package": "$package", "review_id": "$review", "revision": "$revision"},
		defaults.ReviewRead:     {"review_id": "$review"},
		defaults.ReviewPublish:  {"review_id": "$review"},
		defaults.ReviewMerge:    {"review_id": "$review"},
		"review_baseline":       {"package": "$package", "integration_tip": "$integration_tip"},
	}
	names := map[string]string{defaults.TicketCreate: "vendor_ticket_new", defaults.TicketUpdate: "vendor_ticket_change", "review_baseline": "review_baseline"}
	schemas := []external.ToolSchema{}
	server := httptest.NewUnstartedServer(nil)
	url := "http://" + server.Listener.Addr().String()
	cfg.Live.ExternalTools = map[string]external.Tool{}
	for alias, pins := range args {
		name := names[alias]
		if name == "" {
			name = "vendor_" + alias
			for k, v := range common {
				pins[k] = v
			}
		}
		props := map[string]any{}
		for k := range pins {
			props[k] = map[string]string{"type": map[bool]string{true: "integer", false: "string"}[k == "revision"]}
		}
		if alias == defaults.TicketCreate || alias == defaults.TicketUpdate {
			for _, k := range []string{"room", "item", "reference", "phase"} {
				props[k] = map[string]string{"type": "string"}
			}
		}
		input, _ := json.Marshal(map[string]any{"type": "object", "properties": props, "additionalProperties": false})
		schemas = append(schemas, external.ToolSchema{Name: name, InputSchema: input, OutputSchema: json.RawMessage(`{"type":"object"}`)})
		cfg.Live.ExternalTools[alias] = external.Tool{Endpoint: url, Server: "vendor", Name: name, Role: "planner", Arguments: pins}
	}
	server.Config.Handler = vendor.handler(t, schemas)
	server.Start()
	t.Cleanup(server.Close)
	bindings := func(update bool) map[string]string {
		b := map[string]string{"room": "room", "item": "task", "target": "destination", "space": "push_namespace"}
		if update {
			b["reference"] = "ticket"
		}
		return b
	}
	create := TicketAction{Tool: defaults.TicketCreate, Action: "open", Bindings: bindings(false)}
	cfg.Live.TicketWorkflow = &TicketWorkflowConfig{RefreshSeconds: 1, Workflows: []string{"default"}, Actions: map[string]TicketAction{"Pending": create, "ticket-open": create, "ticket-in-review": {Tool: defaults.TicketUpdate, Action: "in-review", Bindings: bindings(true)}, "ticket-closed": {Tool: defaults.TicketUpdate, Action: "closed", Bindings: bindings(true)}}}
	cfg.Live.ReviewPolicies = map[string]external.ReviewPolicy{run.task: {Package: "sample", Workspace: binding.Path, Branch: binding.Branch, IntegrationTip: base, BaselineTool: "review_baseline", StatusTool: defaults.ReviewRead, BaselineApproved: true, PollSeconds: 1, MaxPolls: run.maxPolls, DisablePublishMerge: run.taskOpt}}
	cfg.Live.DisableReviewPublishMerge = run.instanceOptOut
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	if e = d.store.RecordMessage(ctx, "normal-live", "I approve live external effects for the localhost review and ticket fixture"); e != nil {
		t.Fatal(e)
	}
	if status, ok := d.defaultWorkflow.Load().(DefaultWorkflowStatus); !ok || status.State != "installed" || status.Revision != 1 || status.Generation != defaults.Generation {
		t.Fatal("shipped default not installed as revision 1", status)
	}
	return d, cfg, ctx, vendor
}

func admitNormal(t *testing.T, d *Daemon, run normalRun) controlplane.JobSnapshot {
	t.Helper()
	p := AdmitParams{ID: run.task, RunID: "normal-run", Mode: "Normal", DryRun: !run.live, Destination: "main", PushNamespace: "fae/", TicketRoom: "room-9", Content: "deliver the answer through a draft review", AuthorizingMessage: "message"}
	if run.live {
		p.CommitExternal, p.AuthorizingMessage = true, "normal-live"
	}
	if _, e := call(t, d, "Admit", p, run.task); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(170 * time.Second)
	for time.Now().Before(deadline) {
		s, e := d.store.GetJob(context.Background(), run.task)
		if e == nil && (s.State == "Done" || s.State == "Escalated") {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("task did not finish")
	return controlplane.JobSnapshot{}
}

func jobOf(t *testing.T, s controlplane.JobSnapshot) envelope {
	t.Helper()
	var env envelope
	if e := json.Unmarshal(s.Payload, &env); e != nil {
		t.Fatal(e)
	}
	return env
}

// FAE-WF-080, FAE-EXT-010..013 and Appendix C step 20 on the production drive:
// the shipped default, unmodified, delivers a Normal live task through a
// draft review, one same-review remediation revision, separate publish and
// merge, and ticket lifecycle mirroring closed by the merge before Done.
func TestShippedDefaultNormalReviewLifecycle(t *testing.T) {
	run := normalRun{task: "normal-delivery", live: true, mergeCheck: "Pass", remediate: true}
	d, _, _, vendor := defaultNormalFixture(t, run)
	env := jobOf(t, admitNormal(t, d, run))
	j := env.Job
	if j.State != domain.Done || j.Counts["implement"] != 2 || !j.ReviewMerged || j.ReviewID != "R-1" || j.ReviewRevision != 2 || j.ReviewRemediation != "review-remediation-loop" {
		t.Fatal(j.State, j.Reason, j.Counts, j.ReviewID, j.ReviewRevision, j.ReviewRemediation)
	}
	vendor.mu.Lock()
	order := []string{}
	for _, i := range vendor.posts {
		order = append(order, i.Tool+"/"+i.Arguments["phase"])
		if i.Tool == "vendor_review_create" {
			// FAE-EXT-010: every creation call is a draft with no implicit publication.
			if i.Arguments["draft"] != "true" || i.Arguments["auto_publish"] != "false" || i.Arguments["auto_merge"] != "false" || i.Arguments["auto_merge_with_rerun"] != "false" || i.Arguments["branch"] == "" || i.Arguments["workspace"] == "" {
				t.Error("unsafe review creation", i.Arguments)
			}
		}
		if i.Tool == "vendor_review_revision" && (i.Arguments["review_id"] != "R-1" || i.Arguments["revision"] != "2") {
			t.Error("remediation is not revision 2 of the same review", i.Arguments)
		}
	}
	assertOneEffectRowPerCall(t, d, run.task, vendor.posts)
	vendor.mu.Unlock()
	assertReviewProjected(t, d, run.task, j.ReviewID, j.ReviewRevision)
	want := "vendor_ticket_new/open,vendor_ticket_change/review,vendor_review_create/,vendor_ticket_change/review,vendor_review_revision/,vendor_review_publish/,vendor_review_merge/,vendor_ticket_change/closed"
	if got := strings.Join(order, ","); got != want {
		t.Fatalf("effect order\n got %s\nwant %s", got, want)
	}
	// The merge event precedes the ticket's closed transition and Done.
	merged, closed, done := -1, -1, -1
	for n, e := range j.History {
		switch {
		case e.Kind == "review-merged":
			merged = n
		case e.Kind == "transition" && e.To == "ticket-closed":
			closed = n
		case e.Kind == "transition" && e.To == domain.Done:
			done = n
		}
	}
	if merged < 0 || closed < merged || done < closed {
		t.Fatal("merge does not drive ticket closure and Done", merged, closed, done)
	}
	// The implementer received the blocking comment as remediation evidence.
	found := false
	for _, a := range j.Evidence {
		if a.Stage == "review-read" && a.Fields["review_decision"] == "remediation" {
			status, _ := a.Fields["review_status"].(map[string]any)
			found = status != nil && strings.Contains(func() string { b, _ := json.Marshal(status); return string(b) }(), `"blocking":true`)
		}
	}
	if !found {
		t.Fatal("remediation read lacks the review status the implementer needs")
	}
	if j.Document.Stages["review-create"].Pins["draft"] != "true" || workflow.IsTargetPinned(j.Document.Stages["ticket-closed"].Pins) {
		t.Fatal("task did not pin daemon-bound arguments")
	}
}

// FAE-EXT-014 on the production drive: an instance or task opt-out keeps the
// review a draft and makes no publish or merge call (call absence), and the
// pinned document is the shipped one without those stages.
func TestShippedDefaultNormalReviewOptOut(t *testing.T) {
	for _, scope := range []string{"instance", "task"} {
		t.Run(scope, func(t *testing.T) {
			run := normalRun{task: "normal-optout", live: true, mergeCheck: "Pass", instanceOptOut: scope == "instance", taskOpt: scope == "task"}
			d, _, _, vendor := defaultNormalFixture(t, run)
			j := jobOf(t, admitNormal(t, d, run)).Job
			if j.State != domain.Done || j.ReviewMergeRequired || j.ReviewMerged {
				t.Fatal(j.State, j.Reason)
			}
			if _, ok := j.Document.Stages["review-publish"]; ok {
				t.Fatal("opted-out publish stage pinned")
			}
			if _, ok := j.Document.Stages["review-merge"]; ok {
				t.Fatal("opted-out merge stage pinned")
			}
			if vendor.count("vendor_review_publish") != 0 || vendor.count("vendor_review_merge") != 0 || vendor.count("vendor_review_create") != 1 {
				t.Fatal("opt-out made a publish or merge call", vendor.posts)
			}
			vendor.mu.Lock()
			defer vendor.mu.Unlock()
			if vendor.published || vendor.merged {
				t.Fatal("review left draft")
			}
		})
	}
}

// FAE-EXT-011/-013: a Blocked merge-required analyzer refuses the merge before
// any mutation; the bounded observation escalates ReviewGateFailed, the task
// never reaches Done and the ticket is never closed.
func TestShippedDefaultNormalMergeBarrier(t *testing.T) {
	run := normalRun{task: "normal-blocked", live: true, mergeCheck: "Blocked", maxPolls: 2}
	d, _, _, vendor := defaultNormalFixture(t, run)
	j := jobOf(t, admitNormal(t, d, run)).Job
	if j.State != domain.Escalated || j.EscalationKind != domain.ReviewGateFailed {
		t.Fatal(j.State, j.Reason)
	}
	for _, e := range j.History {
		if e.To == domain.Done || e.To == "ticket-closed" {
			t.Fatal("merge barrier crossed", e)
		}
	}
	if vendor.count("vendor_review_publish") != 1 || vendor.count("vendor_review_merge") != 0 {
		t.Fatal("merge mutated without passing gate", vendor.posts)
	}
	vendor.mu.Lock()
	defer vendor.mu.Unlock()
	for _, i := range vendor.posts {
		if i.Arguments["phase"] == "closed" {
			t.Fatal("ticket closed without merge")
		}
	}
}

// FAE-MODE-001/-003 and FAE-WF-080: a Local task pins the Local projection
// (it never reaches a Normal-only stage), a Normal admission missing a subject
// is refused naming it, and a Normal dry-run walks every Normal stage,
// records each intended effect and performs no vendor request at all.
func TestShippedDefaultLocalAndDryRun(t *testing.T) {
	run := normalRun{task: "normal-dry", mergeCheck: "Pass"}
	d, _, ctx, vendor := defaultNormalFixture(t, run)
	raw, _ := json.Marshal(AdmitParams{ID: "local-probe", RunID: "normal-run", Content: "local work"})
	d.mu.Lock()
	snapshot, err := d.prepareAdmission(ctx, raw)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	local := jobOf(t, snapshot).Job
	a, _ := json.Marshal(local.Document)
	b, _ := json.Marshal(defaults.Workflow())
	if string(a) != string(b) || workflow.HasNormalOnly(local.Document) {
		t.Fatal("Local task can reach the Normal sub-workflow")
	}
	installed := d.live.config.ExternalTools[defaults.ReviewMerge]
	delete(d.live.config.ExternalTools, defaults.ReviewMerge)
	raw, _ = json.Marshal(AdmitParams{ID: "missing-subject", RunID: "normal-run", Mode: "Normal", DryRun: true, Destination: "main", PushNamespace: "fae/", Content: "x"})
	d.mu.Lock()
	_, err = d.prepareAdmission(ctx, raw)
	d.mu.Unlock()
	d.live.config.ExternalTools[defaults.ReviewMerge] = installed
	if err == nil || !strings.Contains(err.Error(), defaults.ReviewMerge) {
		t.Fatal("missing Normal subject admitted or unnamed", err)
	}
	env := jobOf(t, admitNormal(t, d, run))
	j := env.Job
	if j.State != domain.Escalated || j.EscalationKind != domain.DryRunComplete {
		t.Fatal(j.State, j.Reason)
	}
	vendor.mu.Lock()
	requests := append([]string(nil), vendor.requests...)
	vendor.mu.Unlock()
	if len(requests) != 0 {
		t.Fatal("dry run contacted the vendor", requests)
	}
	recorded := map[string]bool{}
	for _, e := range j.Evidence {
		s, ok := j.Document.Stages[e.Stage]
		if !ok || s.Primitive != workflow.ExternalEffect {
			continue
		}
		tool := d.live.config.ExternalTools[s.Subject]
		tool.DiscoveredSchema = nil
		intent, err := external.NewIntent(tool, j.TaskID, e.Stage, s.IdempotenceKey, s.Role, j.Tree, "session", s.Pins)
		if err != nil {
			t.Fatal(err)
		}
		if e.Fields["reconciliation_state"] != "dry-run" || e.Fields["tool"] != tool.Name || e.Fields["argument_digest"] != intent.Digest {
			t.Fatal("dry-run artifact does not record the intended effect", e.Stage, e.Fields)
		}
		recorded[e.Stage] = true
	}
	for _, id := range defaults.NormalStages {
		if !recorded[id] {
			t.Fatal("dry run skipped Normal stage", id)
		}
	}
}

// FAE-ROB-042 acceptance: a dry-run item resumed across a daemon restart stays
// dry: the restarted daemon reads the persisted disposition, records intents
// only and emits nothing live.
func TestNormalDryRunResumesDryAcrossRestart(t *testing.T) {
	run := normalRun{task: "dry-restart", mergeCheck: "Pass"}
	d, cfg, ctx, vendor := defaultNormalFixture(t, run)
	p := AdmitParams{ID: run.task, RunID: "normal-run", Mode: "Normal", DryRun: true, Destination: "main", PushNamespace: "fae/", TicketRoom: "room-9", Content: "deliver the answer through a draft review", AuthorizingMessage: "message"}
	if _, e := call(t, d, "Admit", p, run.task); e != nil {
		t.Fatal(e)
	}
	normal := map[string]bool{}
	for _, id := range defaults.NormalStages {
		normal[id] = true
	}
	finished := func(s controlplane.JobSnapshot) bool { return s.State == "Done" || s.State == "Escalated" }
	// Restart once the task has entered the Normal sub-workflow; the waits
	// progress through host-pressure deferrals (pressure helpers).
	if s, _ := awaitJob(t, d, run.task, 170*time.Second, func(s controlplane.JobSnapshot, _ envelope) bool { return normal[s.State] || finished(s) }); finished(s) {
		t.Fatal("task finished before the restart", s.State)
	}
	stop(t, d)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	s, _ := awaitJob(t, d, run.task, 170*time.Second, func(s controlplane.JobSnapshot, _ envelope) bool { return finished(s) })
	j := jobOf(t, s).Job
	want := domain.Disposition{Mode: domain.Normal, TicketRoom: "room-9", Destination: "main", PushNamespace: "fae/"}
	if j.State != domain.Escalated || j.EscalationKind != domain.DryRunComplete || j.Disposition != want {
		t.Fatal("resumed dry run", j.State, j.Reason, j.Disposition)
	}
	vendor.mu.Lock()
	requests := append([]string(nil), vendor.requests...)
	vendor.mu.Unlock()
	if len(requests) != 0 {
		t.Fatal("resumed dry run emitted live effects", requests)
	}
	for _, a := range j.Evidence {
		if normal[a.Stage] && a.Fields["reconciliation_state"] != "dry-run" {
			t.Fatal("resumed dry run recorded a live effect", a.Stage, a.Fields)
		}
	}
}
