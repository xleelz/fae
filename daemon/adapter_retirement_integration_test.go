package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/external"
)

func TestAdapterRetirementNativeDiscoveryRaceAndBoundRestart(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var block atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if block.Load() {
			entered <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		if !serveExternalFixtureSchema(w, r) {
			t.Error("unexpected effect during binding")
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
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
	doc := AdapterDocument{SchemaVersion: 1, Tools: map[string]external.Tool{"fixture": externalTool(srv.URL)}}
	res, err := call(t, d, "AdapterPut", AdapterPutParams{ID: "retire-race", Document: doc, AuthorizingMessage: "message"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var binding AdapterBinding
	if err = json.Unmarshal(res.Result, &binding); err != nil {
		t.Fatal(err)
	}
	target := targetFixture(cfg.Workspace)
	target.SchemaVersion = 3
	target.IntegrationRef = cfg.Live.IntegrationRef
	target.ProtectedBranches = []string{cfg.Live.IntegrationRef}
	target.Normal = &NormalTargetPolicy{Adapter: &binding, Destination: "main", PushNamespace: "fae/fixture", Tools: doc.Tools}
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "bound-target", Document: target, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	admission := AdmitParams{ID: "bound-before-retirement", Mode: "Normal", TargetConfig: "bound-target", TargetRevision: 1, Destination: "main", PushNamespace: "fae/fixture", Content: "bound task", AuthorizingMessage: "message"}
	if _, err = call(t, d, "Admit", admission, admission.ID); err != nil {
		t.Fatal(err)
	}
	block.Store(true)
	installDone := make(chan error, 1)
	go func() {
		_, e := call(t, d, "AdapterPut", AdapterPutParams{ID: "retire-race", Document: doc, AuthorizingMessage: "message"}, "")
		installDone <- e
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("discovery never entered")
	}
	_, err = call(t, d, "AdapterRetire", map[string]string{"id": "retire-race", "reason": "superseded vendor", "authorizing_message": "message"}, "")
	block.Store(false)
	close(release)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-installDone; err == nil {
		t.Fatal("discovery installed revision after retirement")
	}
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "new-target", Document: target, AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("retired exact adapter admitted new target")
	}
	admission.ID = "new-after-retirement"
	if _, err = call(t, d, "Admit", admission, admission.ID); err == nil {
		t.Fatal("existing target bypassed adapter retirement")
	}
	rows, err := d.store.ListAdapters(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatal("retirement race changed revision count", len(rows), err)
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal("bound task failed restart", err)
	}
	selected, err := d.liveForTask(ctx, "bound-before-retirement")
	if err != nil || selected.config.ExternalTools["fixture"].Endpoint != srv.URL {
		t.Fatal("bound task lost pinned adapter", err)
	}
	if _, _, err = d.resolveAdapter(ctx, binding); err != nil {
		t.Fatal("retired immutable revision unavailable", err)
	}
}
