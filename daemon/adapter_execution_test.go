package daemon

import (
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestDynamicAdapterExternalExecution(t *testing.T) {
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
			adapterTools := map[string]external.Tool{"fixture": externalTool(server.URL)}
			cfg.Live.ExternalTools = nil
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

			target := normalTargetFixture(cfg.Workspace, server.URL)
			target.IntegrationRef = cfg.Live.IntegrationRef
			target.ProtectedBranches = []string{target.IntegrationRef}
			authorize(t, d)
			installed, e := call(t, d, "AdapterPut", AdapterPutParams{ID: "dynamic", Document: AdapterDocument{SchemaVersion: 1, Tools: adapterTools}, AuthorizingMessage: "message"}, "")
			if e != nil {
				t.Fatal(e)
			}
			rawInstalled, _ := json.Marshal(installed.Result)
			var ref AdapterBinding
			if e = json.Unmarshal(rawInstalled, &ref); e != nil {
				t.Fatal(e)
			}
			target.SchemaVersion = 3
			target.Normal.Adapter = &ref
			if _, e = call(t, d, "TargetConfigPut", TargetPutParams{ID: "normal", Document: target, AuthorizingMessage: "message"}, ""); e != nil {
				t.Fatal(e)
			}
			admission := AdmitParams{ID: j.TaskID, RunID: "external-run", Mode: "Normal", TargetConfig: "normal", Destination: "main", PushNamespace: "fae/fixture", TicketRoom: "fixture", CommitExternal: mode != "dry-run"}
			if e = d.pinTarget(ctx, &admission, j.Document); e != nil {
				t.Fatal(e)
			}
			selected, e := d.liveForAdmission(ctx, admission)
			if e != nil {
				t.Fatal(e)
			}
			_, intent, e = externalIntent(selected.config, j.Disposition, *j.InFlight)
			if e != nil {
				t.Fatal(e)
			}
			if len(d.live.config.ExternalTools) != 0 {
				t.Fatal("installation mutated instance adapters")
			}
			// Installing a different latest adapter cannot change this target's revision.
			changed := externalTool(server.URL)
			changed.Role = "other"
			if _, e = call(t, d, "AdapterPut", AdapterPutParams{ID: "dynamic", Document: AdapterDocument{SchemaVersion: 1, Tools: map[string]external.Tool{"fixture": changed}}, AuthorizingMessage: "message"}, ""); e != nil {
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
