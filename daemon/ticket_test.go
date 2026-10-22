package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
)

func ticketConfig(endpoint string) LiveConfig {
	bindings := map[string]string{"bucket": "room", "target": "destination", "space": "push_namespace", "item": "task"}
	create := TicketAction{Tool: "create", Action: "room-open-action", Bindings: bindings}
	update := TicketAction{Tool: "update", Action: "room-progress-action", Bindings: map[string]string{"bucket": "room", "target": "destination", "space": "push_namespace", "item": "task", "reference": "ticket"}}
	return LiveConfig{ExternalTools: map[string]external.Tool{
		"create": {Endpoint: endpoint, Server: "fixture", Name: "vendor_new", Role: "planner", Arguments: map[string]string{"target": "main", "space": "fae/task"}},
		"update": {Endpoint: endpoint, Server: "fixture", Name: "vendor_change", Role: "planner", Arguments: map[string]string{"target": "main", "space": "fae/task"}},
	}, TicketWorkflow: &TicketWorkflowConfig{RefreshSeconds: 1, Workflows: []string{"ticket-workflow"}, Actions: map[string]TicketAction{"Pending": create, "open": create, "active": update, "Done": update}}}
}
func ticketJob() engine.Job {
	return engine.Job{TaskID: "ticket-task", Tree: "tree", State: domain.Done, Disposition: domain.Disposition{Mode: domain.Normal, Live: true, TicketRoom: "room-37", Destination: "main", PushNamespace: "fae/task"}, Document: workflow.Document{ID: "ticket-workflow", Stages: map[string]workflow.Stage{"open": {Primitive: workflow.ExternalEffect, Subject: "create", Role: "planner"}, "active": {Primitive: workflow.ExternalEffect, Subject: "update", Role: "planner"}}}, History: []engine.Event{{From: domain.Pending, To: "open"}, {From: "open", To: "active"}, {From: "active", To: domain.Done}}}
}
func TestTicketTransitionsDurableHistoryAndMerge(t *testing.T) {
	j := ticketJob()
	cfg := *ticketConfig("").TicketWorkflow
	rows := ticketTransitions(j, cfg)
	if len(rows) != 3 || rows[0].Step != "Pending" || rows[1].Step != "active" || rows[2].Step != "Done" {
		t.Fatal(rows)
	}
	j.ReviewMergeRequired = true
	if got := ticketTransitions(j, cfg); len(got) != 2 {
		t.Fatal(got)
	}
	j.ReviewMerged = true
	if got := ticketTransitions(j, cfg); len(got) != 3 {
		t.Fatal(got)
	}
	j.Disposition.Live = false
	if len(ticketTransitions(j, cfg)) != 0 {
		t.Fatal("dry run emitted")
	}
	j.Disposition.Mode = domain.Local
	j.Disposition.Live = true
	if len(ticketTransitions(j, cfg)) != 0 {
		t.Fatal("Local emitted")
	}
}
func TestTicketConfigPins(t *testing.T) {
	c := ticketConfig("http://127.0.0.1:1")
	if e := validateTicketConfig(c); e != nil {
		t.Fatal(e)
	}
	for _, source := range []string{"room", "destination", "push_namespace"} {
		bad := ticketConfig("http://127.0.0.1:1")
		a := bad.TicketWorkflow.Actions["Pending"]
		for key, v := range a.Bindings {
			if v == source {
				delete(a.Bindings, key)
			}
		}
		if validateTicketConfig(bad) == nil {
			t.Fatal("missing binding", source)
		}
	}
}

// Native Dolt restart test covers ordered rows, uncertain response recovery,
// room-discovered nonstandard argument names, and one POST across restart.
func TestTicketOutboxNativeRestart(t *testing.T)          { testTicketOutboxNative(t, false) }
func TestDynamicAdapterTicketOutboxRecovery(t *testing.T) { testTicketOutboxNative(t, true) }
func testTicketOutboxNative(t *testing.T, dynamic bool) {
	cfg, ctx := fixture(t)
	var mu sync.Mutex
	receipts := map[string]external.Receipt{}
	posts := 0
	hide := true
	malformed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/schemas":
			schema := json.RawMessage(`{"type":"object","properties":{"target":{"type":"string"},"space":{"type":"string"},"bucket":{"type":"string"},"item":{"type":"string"},"reference":{"type":"string"},"phase_value":{"type":"string"},"serial":{"type":"integer"}},"required":["target","space","bucket","item"],"additionalProperties":false}`)
			json.NewEncoder(w).Encode(external.SchemaCatalog{Server: "fixture", Tools: []external.ToolSchema{{Name: "vendor_new", InputSchema: schema}, {Name: "vendor_change", InputSchema: schema}}})
		case strings.HasPrefix(r.URL.Path, "/rooms/"):
			json.NewEncoder(w).Encode(external.RoomResources{Server: "fixture", Room: "room-37", Actions: []external.RoomAction{{Name: "room-open-action", Tool: "vendor_new", Arguments: map[string]any{"phase_value": "room-custom-open", "serial": json.Number("9007199254740993")}}, {Name: "room-progress-action", Tool: "vendor_change", Arguments: map[string]any{"phase_value": "room-custom-progress", "serial": json.Number("9007199254740993")}}}})
		case strings.HasPrefix(r.URL.Path, "/effects/"):
			key := strings.TrimPrefix(r.URL.Path, "/effects/")
			if r.Method == "POST" {
				var intent external.Intent
				if err := json.NewDecoder(r.Body).Decode(&intent); err != nil {
					t.Error(err)
				}
				posts++
				receipts[key] = external.Receipt{Key: key, Task: intent.Task, Tool: intent.Tool, Digest: intent.Digest, ExternalID: "T-123", State: "performed", Tree: intent.Tree}
				if hide {
					w.WriteHeader(502)
					fmt.Fprint(w, `{"external_id":"T-123"}`)
					return
				}
				json.NewEncoder(w).Encode(receipts[key])
				return
			}
			receipt, ok := receipts[key]
			if !ok || hide {
				http.NotFound(w, r)
				return
			}
			if malformed {
				w.WriteHeader(502)
			}
			json.NewEncoder(w).Encode(receipt)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	owner, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	local := ticketConfig(server.URL)
	if err := ValidateExternalSchemas(ctx, local); err != nil {
		t.Fatal("dynamic template startup", err)
	}
	d := &Daemon{config: cfg, store: owner.store, live: &liveExecutor{config: local}, ticketDiscovery: &external.Discovery{TTL: time.Second}}
	j := ticketJob()
	admission := AdmitParams{}
	if dynamic {
		authorize(t, owner)
		installed, e := call(t, owner, "AdapterPut", AdapterPutParams{ID: "tickets", Document: AdapterDocument{SchemaVersion: 1, Tools: local.ExternalTools, TicketWorkflow: local.TicketWorkflow}, AuthorizingMessage: "message"}, "")
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(installed.Result)
		var ref AdapterBinding
		if e = json.Unmarshal(raw, &ref); e != nil {
			t.Fatal(e)
		}
		d.live.config = LiveConfig{IntegrationRef: "refs/heads/main"}
		target := targetFixture(cfg.Workspace)
		target.SchemaVersion = 3
		target.Normal = &NormalTargetPolicy{Adapter: &ref, Destination: j.Disposition.Destination, PushNamespace: j.Disposition.PushNamespace, TicketRoom: j.Disposition.TicketRoom, Tools: local.ExternalTools, TicketWorkflow: local.TicketWorkflow}
		raw, _ = json.Marshal(TargetPutParams{ID: "tickets", Document: target})
		if _, e = d.putTarget(ctx, controlplane.Audit{Actor: "test", Action: "install target", Outcome: "ok"}, raw); e != nil {
			t.Fatal(e)
		}
		for name, stage := range j.Document.Stages {
			stage.Pins = local.ExternalTools[stage.Subject].Arguments
			j.Document.Stages[name] = stage
		}
		admission = AdmitParams{ID: j.TaskID, Mode: "Normal", CommitExternal: true, TargetConfig: "tickets", Destination: j.Disposition.Destination, PushNamespace: j.Disposition.PushNamespace, TicketRoom: j.Disposition.TicketRoom}
		if e = d.pinTarget(ctx, &admission, j.Document); e != nil {
			t.Fatal(e)
		}
	}

	fixturePayload, _ := json.Marshal(envelope{Job: j, Admission: admission})
	d.jobs = map[string]controlplane.JobSnapshot{j.TaskID: {ID: j.TaskID, Payload: fixturePayload}}
	if err = d.driveTaskTickets(ctx, j); err == nil {
		t.Fatal("ambiguous response accepted")
	}
	rows, err := d.store.TicketEmissions(ctx, j.TaskID, j.Disposition.TicketRoom)
	if err != nil || len(rows) != 3 || !rows[0].Started || rows[0].Emitted || rows[0].Attempts != 1 || rows[0].LastError == "" {
		t.Fatal(rows, err)
	}
	if err = d.driveTaskTickets(ctx, j); err == nil || !strings.Contains(err.Error(), "replay refused") {
		t.Fatal(err)
	}
	mu.Lock()
	n := posts
	mu.Unlock()
	if n != 1 {
		t.Fatal("mutation replayed", n)
	}
	stop(t, owner)
	owner, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, owner)
	d.store = owner.store
	if status := owner.ticketStatus.Load().(ticketStatus); status.Uncertain != 1 {
		t.Fatal("restart without ticket config hid uncertain row", status)
	}
	mu.Lock()
	hide = false
	malformed = true
	mu.Unlock()
	if err = d.driveTaskTickets(ctx, j); err == nil {
		t.Fatal("malformed query accepted")
	}
	rows, _ = d.store.TicketEmissions(ctx, j.TaskID, j.Disposition.TicketRoom)
	var pinned ticketIntent
	json.Unmarshal(rows[0].Intent, &pinned)
	mu.Lock()
	observed := receipts[pinned.Intent.Key]
	mu.Unlock()
	raw, _ := json.Marshal(observed)
	hash, _ := domain.ContentHash(raw)
	if _, err = d.store.GetMarker(ctx, "ticket-observed/"+rows[0].Key+"/"+hash); err != nil {
		t.Fatal("reported query ID lost", err)
	}
	mu.Lock()
	malformed = false
	mu.Unlock()
	d.ctx = ctx
	fixturePayload, _ = json.Marshal(envelope{Job: j, Admission: admission})
	d.jobs = map[string]controlplane.JobSnapshot{j.TaskID: {ID: j.TaskID, Payload: fixturePayload}}
	d.live.daemon = d
	for n := 0; n < 3; n++ {
		d.startTicketOutbox()
		d.workers.Wait()
		if value := d.ticketStatus.Load(); value == nil || value.(ticketStatus).Error != "" {
			t.Fatal(value)
		}
	}
	selected, err := d.liveForTask(ctx, j.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	selected.daemon = d
	output, managed, err := selected.executeManagedTicket(ctx, j, engine.Dispatch{Stage: "open", Config: j.Document.Stages["open"]})
	if err != nil || !managed || output == nil {
		t.Fatal(output, managed, err)
	}
	rows, _ = d.store.TicketEmissions(ctx, j.TaskID, j.Disposition.TicketRoom)
	for _, r := range rows {
		if !r.Emitted || r.TicketID != "T-123" || r.LastError != "" {
			t.Fatal(r)
		}
	}
	mu.Lock()
	n = posts
	mu.Unlock()
	if n != 3 {
		t.Fatal("expected one mutation per lifecycle step", n)
	}
	stats, err := d.store.TicketStats(ctx)
	if err != nil || stats.Depth != 0 || stats.Failures != 0 {
		t.Fatal(stats, err)
	}
	events, err := d.store.JobEvents(ctx, j.TaskID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]bool{}
	for _, event := range events {
		kinds[event.Kind] = true
		if event.Kind == "ticket-outbox-emitted" {
			var row controlplane.TicketEmission
			if json.Unmarshal(event.Payload, &row) != nil {
				t.Fatal("invalid audit event")
			}
			var intent ticketIntent
			if json.Unmarshal(row.Intent, &intent) != nil || intent.Intent.Digest == "" || intent.Intent.Session == "" || intent.Tool.Server != "fixture" {
				t.Fatal("missing canonical effect provenance", row)
			}
		}
	}
	for _, kind := range []string{"ticket-outbox-queued", "ticket-outbox-prepared", "ticket-outbox-uncertain", "ticket-outbox-failed", "ticket-outbox-emitted"} {
		if !kinds[kind] {
			t.Fatal("missing atomic event", kind)
		}
	}
	// An unchanged history never creates more rows/effects after restart.
	if err = d.driveTaskTickets(ctx, j); err != nil {
		t.Fatal(err)
	}
	afterEvents, err := d.store.JobEvents(ctx, j.TaskID, 0, 1000)
	if err != nil || len(afterEvents) != len(events) {
		t.Fatal("idle outbox wrote new audit events", len(events), len(afterEvents), err)
	}
	changed := local
	changed.ExternalTools = map[string]external.Tool{}
	for k, v := range local.ExternalTools {
		changed.ExternalTools[k] = v
	}
	tool := changed.ExternalTools["create"]
	tool.Endpoint = "http://127.0.0.1:1"
	changed.ExternalTools["create"] = tool
	d.live.config = changed
	if err = d.driveTaskTickets(ctx, j); dynamic {
		if err != nil {
			t.Fatal("instance drift affected pinned adapter", err)
		}
	} else if err == nil {
		t.Fatal("changed installed endpoint accepted")
	}
}
func TestTicketGuardTruthTable(t *testing.T) {
	for bits := 0; bits < 16; bits++ {
		started, emitted, ordered, live := bits&1 != 0, bits&2 != 0, bits&4 != 0, bits&8 != 0
		if external.CanEmitTicket(started, emitted, ordered, live) != (!started && !emitted && ordered && live) {
			t.Fatal(bits)
		}
	}
}

func TestTicketAdmissionDispositionAndRole(t *testing.T) {
	c := ticketConfig("http://127.0.0.1:1")
	j := ticketJob()
	p := AdmitParams{CommitExternal: true, TicketRoom: "room-37", Destination: "main", PushNamespace: "fae/task"}
	if err := validateTicketAdmission(c, j.Document, p); err != nil {
		t.Fatal(err)
	}
	p.TicketRoom = ""
	if validateTicketAdmission(c, j.Document, p) == nil {
		t.Fatal("missing room accepted")
	}
	p.CommitExternal = false
	if err := validateTicketAdmission(c, j.Document, p); err != nil {
		t.Fatal("dry run refused", err)
	}
	delete(j.Document.Stages, "active")
	if validateTicketAdmission(c, j.Document, p) == nil {
		t.Fatal("missing role grant accepted")
	}
}

func TestTicketRoundFairness(t *testing.T) {
	jobs := []engine.Job{{TaskID: "z"}, {TaskID: "a"}, {TaskID: "m"}}
	seen := map[string]bool{}
	for round := uint64(0); round < 3; round++ {
		ordered := ticketRoundOrder(jobs, round)
		seen[ordered[0].TaskID] = true
		if len(ordered) != 3 {
			t.Fatal(ordered)
		}
	}
	if len(seen) != 3 {
		t.Fatal("slow head starved task", seen)
	}
	if len(ticketRoundOrder(nil, 99)) != 0 {
		t.Fatal("empty round")
	}
}
