package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
	"net/http"
	"net/http/httptest"
	"testing"
)

func normalTargetFixture(workspace, endpoint string) TargetConfig {
	c := targetFixture(workspace)
	c.SchemaVersion = 2
	c.Normal = &NormalTargetPolicy{Destination: "main", PushNamespace: "fae/fixture", TicketRoom: "fixture", Tools: map[string]external.Tool{"fixture": externalTool(endpoint)}}
	return c
}
func TestNormalTargetExactPoliciesAndIsolation(t *testing.T) {
	c := normalTargetFixture("/repo", "http://127.0.0.1:1")
	d := &Daemon{live: &liveExecutor{config: LiveConfig{ExternalTools: c.Normal.Tools}}}
	p := AdmitParams{ID: "task", Mode: "Normal", Destination: "main", PushNamespace: "fae/fixture", TicketRoom: "fixture"}
	doc := externalDocument()
	for _, live := range []bool{false, true} {
		p.CommitExternal = live
		if err := d.validateNormalTarget(context.Background(), c, p, doc, false); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.validateNormalTarget(context.Background(), targetFixture("/repo"), p, doc, false); err == nil {
		t.Fatal("v1 Normal")
	}
	local := AdmitParams{Mode: "Local"}
	if err := d.validateNormalTarget(context.Background(), c, local, doc, false); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*AdmitParams){
		func(p *AdmitParams) { p.Destination = "other" },
		func(p *AdmitParams) { p.PushNamespace = "" },
		func(p *AdmitParams) { p.TicketRoom = "other" },
	} {
		bad := p
		mutate(&bad)
		if d.validateNormalTarget(context.Background(), c, bad, doc, false) == nil {
			t.Fatal("disposition pin ignored")
		}
	}
	original, _ := json.Marshal(d.live.config)
	for _, mutate := range []func(*TargetConfig){
		func(c *TargetConfig) { c.Normal.Tools["fixture"] = externalTool("http://127.0.0.1:2") },
		func(c *TargetConfig) { delete(c.Normal.Tools, "fixture") },
		func(c *TargetConfig) {
			tool := c.Normal.Tools["fixture"]
			tool.Role = "implementer"
			c.Normal.Tools["fixture"] = tool
		},
	} {
		bad := normalTargetFixture("/repo", "http://127.0.0.1:1")
		mutate(&bad)
		if d.validateNormalTarget(context.Background(), bad, p, doc, false) == nil {
			t.Fatal("adapter pin ignored")
		}
	}
	after, _ := json.Marshal(d.live.config)
	if string(original) != string(after) {
		t.Fatal("task validation mutated shared live config")
	}
	stage := doc.Stages["external"]
	stage.Pins = map[string]string{"wrong": "pin"}
	doc.Stages["external"] = stage
	if d.validateNormalTarget(context.Background(), c, p, doc, false) == nil {
		t.Fatal("workflow pin ignored")
	}
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			if NormalTargetAuthorized(a, b) != (a && b) {
				t.Fatal("guard")
			}
		}
	}
}
func TestNormalTargetSchemaDiscoveryAndShape(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if !serveExternalFixtureSchema(w, r) {
			t.Error("effect during target health")
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	c := normalTargetFixture("/repo", srv.URL)
	d := &Daemon{live: &liveExecutor{config: LiveConfig{ExternalTools: c.Normal.Tools}}}
	if err := d.validateNormalTarget(context.Background(), c, AdmitParams{}, workflow.Document{}, true); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || d.live.config.ExternalTools["fixture"].DiscoveredSchema != nil {
		t.Fatal("discovery side effect", calls)
	}
	for _, mutate := range []func(*TargetConfig){
		func(c *TargetConfig) { c.SchemaVersion = 1 },
		func(c *TargetConfig) { c.Normal = nil },
		func(c *TargetConfig) { c.Normal.Destination = "" },
		func(c *TargetConfig) { c.Normal.ReviewTask = "task" },
		func(c *TargetConfig) { c.Normal.Review = &external.ReviewPolicy{} },
		func(c *TargetConfig) { c.Normal.DisableReviewPublishMerge = true },
	} {
		bad := normalTargetFixture("/repo", srv.URL)
		mutate(&bad)
		raw, _ := json.Marshal(bad)
		if _, err := parseTarget(raw); err == nil {
			t.Fatal("invalid schema accepted", string(raw))
		}
	}
	tool := c.Normal.Tools["fixture"]
	tool.Arguments = map[string]string{"imaginary": "value"}
	c.Normal.Tools["fixture"] = tool
	if d.validateNormalTarget(context.Background(), c, AdmitParams{}, workflow.Document{}, true) == nil {
		t.Fatal("unknown runtime schema key accepted")
	}
}

func TestNormalTargetNativePinningRestartAndDrift(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveExternalFixtureSchema(w, r) {
			t.Error("unexpected mutation")
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	cfg.Live.ExternalTools = map[string]external.Tool{"fixture": externalTool(srv.URL)}
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
	d.mu.Lock()
	d.draining = true
	d.mu.Unlock()
	c := normalTargetFixture(cfg.Workspace, srv.URL)
	c.IntegrationRef = cfg.Live.IntegrationRef
	c.ProtectedBranches = []string{c.IntegrationRef}
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "normal", Document: c, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	p := AdmitParams{ID: "normal-task", TargetConfig: "normal", Mode: "Normal", Destination: "main", PushNamespace: "fae/fixture", TicketRoom: "fixture"}
	doc := externalDocument()
	if err = d.pinTarget(ctx, &p, doc); err != nil {
		t.Fatal(err)
	}
	if p.TargetRevision != 1 || p.TargetHash == "" {
		t.Fatal("not pinned")
	}
	c.Normal.PushNamespace = "new-namespace"
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "normal", Document: c, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if err = d.checkPinnedTarget(ctx, p, doc); err != nil {
		t.Fatal("latest revision changed existing policy", err)
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = d.checkPinnedTarget(ctx, p, doc); err != nil {
		t.Fatal("restart lost pins", err)
	}
	p.CommitExternal = true
	if err = d.checkPinnedTarget(ctx, p, doc); err != nil {
		t.Fatal("live pins", err)
	}
	tool := d.live.config.ExternalTools["fixture"]
	tool.Endpoint = "http://127.0.0.1:2"
	d.live.config.ExternalTools["fixture"] = tool
	if d.checkPinnedTarget(ctx, p, doc) == nil {
		t.Fatal("installed endpoint drift accepted")
	}
	if err = d.validateNormalTarget(ctx, targetFixture(cfg.Workspace), AdmitParams{Mode: "Local"}, doc, false); err != nil {
		t.Fatal("Normal drift contaminated Local", err)
	}
}
