package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
)

func externalTool(endpoint string) external.Tool {
	return external.Tool{Endpoint: endpoint, Server: "fixture", Name: "fixture_create", Role: "planner", Arguments: map[string]string{"draft": "true", "branch": "fae/fixture", "destination": "main", "ticket_room": "fixture", "push_namespace": "fae/fixture"}}
}
func externalDocument() workflow.Document {
	doc := defaults.Workflow()
	doc.Artifacts["ExternalReceipt"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["tool","argument_digest","external_id","reconciliation_state","idempotency_key"],"properties":{"tool":{"type":"string"},"argument_digest":{"type":"string"},"external_id":{"type":"string"},"reconciliation_state":{"type":"string"},"idempotency_key":{"type":"string"}}}`)}
	role := doc.Roles["planner"]
	role.Outputs = append(role.Outputs, "ExternalReceipt")
	doc.Roles["planner"] = role
	doc.Stages["external"] = workflow.Stage{Primitive: workflow.ExternalEffect, Role: "planner", NormalOnly: true, Subject: "fixture", Pins: externalTool("").Arguments, IdempotenceKey: "review-create", Output: "ExternalReceipt"}
	for n, e := range doc.Edges {
		if e.To == "Done" {
			doc.Edges[n].To = "external"
		}
	}
	doc.Edges = append(doc.Edges, workflow.Edge{From: "external", To: "Done"})
	return doc
}
func externalJob(t *testing.T, tree, base string, live bool) engine.Job {
	t.Helper()
	j, e := engine.NewLive("external-work", 1, externalDocument(), tree)
	if e != nil {
		t.Fatal(e)
	}
	j.Base = base
	j.Disposition = domain.Disposition{Mode: domain.Normal, Live: live, TicketRoom: "fixture", Destination: "main", PushNamespace: "fae/fixture"}
	runner := engine.NewScripted(defaults.HappyScript())
	for n := 0; n < 150; n++ {
		var dispatch *engine.Dispatch
		j, dispatch, _, e = engine.Prepare(j)
		if e != nil {
			t.Fatal(e)
		}
		if dispatch == nil {
			continue
		}
		if dispatch.Stage == "external" {
			return j
		}
		r, e := runner.Execute(context.Background(), *dispatch)
		if e != nil {
			t.Fatal(e)
		}
		r.ExecutionKind = "live"
		j, _, e = engine.Complete(j, dispatch.ID, r)
		if e != nil {
			t.Fatal(e)
		}
	}
	t.Fatal("external stage not reached", j.State, j.Reason)
	return j
}
func TestExternalModeAndDryRunTerminal(t *testing.T) {
	j := externalJob(t, "tree", "base", false)
	dispatch := *j.InFlight
	r := engine.Result{ExecutionKind: "live", TaskID: j.TaskID, DispatchID: dispatch.ID, Tree: j.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: json.RawMessage(`{"tool":"fixture_create","argument_digest":"digest","external_id":"","reconciliation_state":"dry-run","idempotency_key":"key"}`)}
	j, _, e := engine.Complete(j, dispatch.ID, r)
	if e != nil {
		t.Fatal(e)
	}
	if j.State != domain.Escalated || j.Reason != "DryRunComplete" {
		t.Fatal(j.State, j.Reason)
	}
	local := externalJob(t, "tree", "base", true)
	local.InFlight = nil
	local.Disposition = domain.Disposition{Mode: domain.Local}
	local, dispatchPtr, _, e := engine.Prepare(local)
	if e != nil || dispatchPtr != nil || local.State != domain.Escalated {
		t.Fatal(local.State, e)
	}
}
func TestExternalCrashHelper(t *testing.T) {
	path := os.Getenv("FAE_EXTERNAL_CRASH")
	if path == "" {
		return
	}
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var cfg Config
	if e = json.Unmarshal(raw, &cfg); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()
	authorize(t, d)
	w, e := d.live.manager.Ensure(ctx, "external-run", "external-work", cfg.Live.Base)
	if e != nil {
		t.Fatal(e)
	}
	tree, e := d.live.broker.Tree(ctx, w)
	if e != nil {
		t.Fatal(e)
	}
	j := externalJob(t, tree, cfg.Live.Base, true)
	env := envelope{Job: j, Admission: AdmitParams{ID: j.TaskID, RunID: "external-run", Mode: "Normal", CommitExternal: true, AuthorizingMessage: "message"}}
	payload, _ := json.Marshal(env)
	stored, e := d.store.AdmitJob(ctx, controlplane.Audit{Actor: "test", Action: "external crash fixture", Outcome: "ok"}, "external-fixture", controlplane.JobSnapshot{ID: j.TaskID, WorkflowID: j.Document.ID, WorkflowRevision: 1, TargetID: cfg.Workspace, TargetRevision: 1, State: string(j.State), Payload: payload})
	if e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	d.jobs[j.TaskID] = stored
	d.mu.Unlock()
	if e = os.WriteFile(path+".ready", payload, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = d.live.Execute(ctx, *j.InFlight); e != nil {
		t.Fatal(e)
	}
	t.Fatal("blocked response unexpectedly returned")
}
func TestExternalSIGKILLQueryAdoption(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	var mu sync.Mutex
	var receipt external.Receipt
	posts, gets := 0, 0
	performed := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if serveExternalFixtureSchema(w, r) {
			return
		}
		if r.Method == http.MethodGet {
			mu.Lock()
			gets++
			saved := receipt
			mu.Unlock()
			if saved.ExternalID == "" {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(saved)
			return
		}
		var i external.Intent
		if e := json.NewDecoder(r.Body).Decode(&i); e != nil {
			t.Error(e)
			return
		}
		mu.Lock()
		posts++
		receipt = external.Receipt{Key: i.Key, Task: i.Task, Tool: "mcp__fixture__fixture_create", Digest: i.Digest, ExternalID: "fixture-review-1", State: "performed", Tree: i.Tree}
		mu.Unlock()
		close(performed)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	cfg.Live.ExternalTools = map[string]external.Tool{"fixture": externalTool(server.URL)}
	path := filepath.Join(filepath.Dir(cfg.Workspace), "external-crash.json")
	raw, _ := json.Marshal(cfg)
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestExternalCrashHelper$")
	cmd.Env = append(os.Environ(), "FAE_EXTERNAL_CRASH="+path)
	log, e := os.Create(path + ".log")
	if e != nil {
		t.Fatal(e)
	}
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	select {
	case <-performed:
	case <-time.After(40 * time.Second):
		b, _ := os.ReadFile(path + ".log")
		t.Fatalf("helper: %s", b)
	}
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	cmd.Wait()
	raw, e = os.ReadFile(path + ".ready")
	if e != nil {
		t.Fatal(e)
	}
	var before envelope
	if e = decodeEnvelope(raw, &before); e != nil {
		t.Fatal(e)
	}
	var d *Daemon
	deadline := time.Now().Add(10 * time.Second)
	for {
		d, e = Start(ctx, cfg)
		if e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(e)
		}
		time.Sleep(50 * time.Millisecond)
	}
	defer stop(t, d)
	if _, e = call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); e != nil {
		t.Fatal(e)
	}
	p := ResolveUncertainParams{ID: before.Job.TaskID, DispatchID: before.Job.InFlight.ID, Action: "redispatch", AuthorizingMessage: "message"}
	if _, e = call(t, d, "ResolveUncertain", p, ""); e == nil {
		t.Fatal("replayed external effect")
	}
	p.Action = "accept"
	p.Result = &engine.Result{}
	if _, e = call(t, d, "ResolveUncertain", p, ""); e == nil {
		t.Fatal("accepted forged receipt")
	}
	p.Result = nil
	mu.Lock()
	receipt.Digest = "wrong"
	mu.Unlock()
	if _, e = call(t, d, "ResolveUncertain", p, ""); e == nil {
		t.Fatal("accepted foreign receipt")
	}
	_, intentForObservation, _ := externalIntent(*cfg.Live, before.Job.Disposition, *before.Job.InFlight)
	mu.Lock()
	badReceipt := receipt
	mu.Unlock()
	observed, _ := json.Marshal(map[string]any{"intent": intentForObservation, "reported": badReceipt, "reconciliation_state": "unreconciled"})
	observedHash, _ := domain.ContentHash(observed)
	if raw, e := d.store.GetMarker(ctx, effectKey(*before.Job.InFlight)+"/observed/"+observedHash); e != nil || !strings.Contains(string(raw), "fixture-review-1") {
		t.Fatal("lost refused external id", e)
	}
	mu.Lock()
	receipt.ExternalID = ""
	mu.Unlock()
	if _, e = call(t, d, "ResolveUncertain", p, ""); e == nil {
		t.Fatal("absent query authorized recovery")
	}
	mu.Lock()
	receipt.ExternalID = "fixture-review-1"
	mu.Unlock()
	mu.Lock()
	_, intent, err := externalIntent(*cfg.Live, before.Job.Disposition, *before.Job.InFlight)
	receipt.Digest = intent.Digest
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if _, e = call(t, d, "ResolveUncertain", p, ""); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	saved := d.jobs[p.ID]
	d.mu.Unlock()
	var after envelope
	if e = decodeEnvelope(saved.Payload, &after); e != nil {
		t.Fatal(e)
	}
	if after.Job.State != domain.Done || len(after.Job.ResolvedDispatches) != 1 {
		t.Fatal(after.Job.State, after.Job.Reason)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 1 || gets != 4 {
		t.Fatal("duplicate effect or missing verified query", posts, gets)
	}
	proofs, e := d.live.guard.journal.Entries()
	if e != nil || len(proofs) != 0 {
		t.Fatal("remaining lease", proofs, e)
	}
}

func TestExternalLiveDryRunAndExistingReceipt(t *testing.T) {
	for _, mode := range []string{"dry-run", "existing", "create", "malformed"} {
		t.Run(mode, func(t *testing.T) {
			cfg, ctx, _ := liveDaemonFixture(t)
			var mu sync.Mutex
			gets, posts := 0, 0
			var intent external.Intent
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveExternalFixtureSchema(w, r) {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if r.Method == "GET" {
					gets++
					if mode == "create" {
						w.WriteHeader(404)
						return
					}
				} else {
					posts++
					if e := json.NewDecoder(r.Body).Decode(&intent); e != nil {
						t.Error(e)
					}
				}
				response := external.Receipt{Key: intent.Key, Task: intent.Task, Tool: intent.Tool, Digest: intent.Digest, ExternalID: "review-existing", Tree: intent.Tree, State: "performed", CommitSHA: "different-sha-same-tree"}
				if mode == "malformed" {
					raw, _ := json.Marshal(response)
					var malformed map[string]any
					json.Unmarshal(raw, &malformed)
					malformed["unknown"] = true
					json.NewEncoder(w).Encode(malformed)
					return
				}
				json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			cfg.Live.ExternalTools = map[string]external.Tool{"fixture": externalTool(server.URL)}
			d, e := Start(ctx, cfg)
			if e != nil {
				t.Fatal(e)
			}
			defer stop(t, d)
			d.mu.Lock()
			d.draining = true
			d.mu.Unlock()
			w, e := d.live.manager.Ensure(ctx, "external-run", "external-work", cfg.Live.Base)
			if e != nil {
				t.Fatal(e)
			}
			tree, e := d.live.broker.Tree(ctx, w)
			if e != nil {
				t.Fatal(e)
			}
			j := externalJob(t, tree, cfg.Live.Base, mode != "dry-run")
			_, intent, e = externalIntent(*cfg.Live, j.Disposition, *j.InFlight)
			if e != nil {
				t.Fatal(e)
			}
			target := normalTargetFixture(cfg.Workspace, server.URL)
			target.IntegrationRef = cfg.Live.IntegrationRef
			target.ProtectedBranches = []string{target.IntegrationRef}
			authorize(t, d)
			if _, e = call(t, d, "TargetConfigPut", TargetPutParams{ID: "normal", Document: target, AuthorizingMessage: "message"}, ""); e != nil {
				t.Fatal(e)
			}
			admission := AdmitParams{ID: j.TaskID, RunID: "external-run", Mode: "Normal", TargetConfig: "normal", Destination: "main", PushNamespace: "fae/fixture", TicketRoom: "fixture", CommitExternal: mode != "dry-run"}
			if e = d.pinTarget(ctx, &admission, j.Document); e != nil {
				t.Fatal(e)
			}
			payload, _ := json.Marshal(envelope{Job: j, Admission: admission})
			saved, e := d.store.AdmitJob(ctx, controlplane.Audit{Actor: "test", Action: "external execution fixture", Outcome: "ok"}, "external-fixture", controlplane.JobSnapshot{ID: j.TaskID, WorkflowID: j.Document.ID, WorkflowRevision: 1, TargetID: "normal", TargetRevision: 1, State: string(j.State), Payload: payload})
			if e != nil {
				t.Fatal(e)
			}
			d.mu.Lock()
			d.jobs[j.TaskID] = saved
			d.mu.Unlock()
			r, e := d.live.Execute(ctx, *j.InFlight)
			if mode == "malformed" {
				if e == nil {
					t.Fatal("accepted malformed response")
				}
				reported := external.Receipt{Key: intent.Key, Task: intent.Task, Tool: intent.Tool, Digest: intent.Digest, ExternalID: "review-existing", Tree: intent.Tree, State: "performed", CommitSHA: "different-sha-same-tree"}
				observed, _ := json.Marshal(map[string]any{"intent": intent, "reported": reported, "reconciliation_state": "unreconciled"})
				hash, _ := domain.ContentHash(observed)
				if raw, e := d.store.GetMarker(ctx, effectKey(*j.InFlight)+"/observed/"+hash); e != nil || !strings.Contains(string(raw), "review-existing") {
					t.Fatal("lost malformed reported id", e)
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			j, _, e = engine.Complete(j, j.InFlight.ID, r)
			if e != nil {
				t.Fatal(e)
			}
			mu.Lock()
			defer mu.Unlock()
			if mode == "dry-run" {
				if gets != 0 || posts != 0 || j.State != domain.Escalated || j.Reason != "DryRunComplete" {
					t.Fatal(gets, posts, j.State, j.Reason)
				}
			} else if j.State != domain.Done || gets != 1 || (mode == "existing" && posts != 0) || (mode == "create" && posts != 1) {
				t.Fatal(gets, posts, j.State)
			}
		})
	}
}

func TestExternalAdmissionAuthorityAndPins(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveExternalFixtureSchema(w, r) {
			t.Error("unexpected effect call")
			w.WriteHeader(500)
		}
	}))
	defer server.Close()
	cfg.Live.ExternalTools = map[string]external.Tool{"fixture": externalTool(server.URL)}
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	for _, tc := range []struct {
		id, text string
		want     bool
	}{{"general", "run the task", false}, {"explicit", "I approve live external effects for this local fixture", true}} {
		if e = d.store.RecordMessage(ctx, tc.id, tc.text); e != nil {
			t.Fatal(e)
		}
		if e = d.store.RequireExternalAuthority(ctx, tc.id); (e == nil) != tc.want {
			t.Fatal(tc, e)
		}
	}
	doc := externalDocument()
	p := AdmitParams{Mode: "Normal", CommitExternal: true, TicketRoom: "fixture", Destination: "main", PushNamespace: "fae/fixture"}
	if e = validateExternalStages(*cfg.Live, doc, p); e != nil {
		t.Fatal(e)
	}
	p.Destination = "different"
	if validateExternalStages(*cfg.Live, doc, p) == nil {
		t.Fatal("admission destination ignored")
	}
	p.Destination = "main"
	stage := doc.Stages["external"]
	stage.Role = "implementer"
	doc.Stages["external"] = stage
	if validateExternalStages(*cfg.Live, doc, p) == nil {
		t.Fatal("role grant widened")
	}
	for _, params := range []AdmitParams{{ID: "bad", Mode: "Normal", CommitExternal: true}, {ID: "bad", Mode: "Local", CommitExternal: true}, {ID: "bad", Mode: "Other"}} {
		raw, _ := json.Marshal(params)
		d.mu.Lock()
		_, e = d.admit(ctx, "bad", raw, controlplane.Audit{Actor: "operator", MessageID: "general", Action: "admit", Outcome: "ok"})
		d.mu.Unlock()
		if e == nil {
			t.Fatal("accepted invalid mode", params)
		}
	}
}

func TestExternalReviewWithoutTicketRoom(t *testing.T) {
	tool := externalTool("http://127.0.0.1:1")
	delete(tool.Arguments, "ticket_room")
	disposition := domain.Disposition{Mode: domain.Normal, Live: true, Destination: "main", PushNamespace: "fae/fixture"}
	if e := validateExternalDestinations(tool, tool.Arguments, disposition); e != nil {
		t.Fatal(e)
	}
	tool.Name = "ticket_create"
	if e := validateExternalDestinations(tool, tool.Arguments, disposition); e == nil {
		t.Fatal("ticket tool accepted missing room")
	}
}

func serveExternalFixtureSchema(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/schemas" {
		return false
	}
	properties := map[string]any{}
	for k := range externalTool("").Arguments {
		properties[k] = map[string]any{"type": "string"}
	}
	schema, _ := json.Marshal(map[string]any{"type": "object", "properties": properties, "additionalProperties": false})
	json.NewEncoder(w).Encode(external.SchemaCatalog{Server: "fixture", Tools: []external.ToolSchema{{Name: "fixture_create", InputSchema: schema}}})
	return true
}
