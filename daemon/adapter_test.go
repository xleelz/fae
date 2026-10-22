package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/external"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestAdapterGuard(t *testing.T) {
	for _, a := range []bool{false, true} {
		for _, b := range []bool{false, true} {
			if AdapterBindingValid(a, b) != (a && b) {
				t.Fatal(a, b)
			}
		}
	}
}
func TestAdapterNativeInstallRestartAndFailures(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	var broken atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if broken.Load() {
			w.WriteHeader(503)
			return
		}
		if !serveExternalFixtureSchema(w, r) {
			t.Error("effect during install")
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
	doc := AdapterDocument{SchemaVersion: 1, Tools: map[string]external.Tool{"fixture": externalTool(srv.URL)}}
	if _, err = call(t, d, "AdapterPut", AdapterPutParams{ID: "installed", Document: doc}, ""); err == nil {
		t.Fatal("missing authority accepted")
	}
	put := func(id string, doc AdapterDocument) (AdapterBinding, error) {
		res, e := call(t, d, "AdapterPut", AdapterPutParams{ID: id, Document: doc, AuthorizingMessage: "message"}, "")
		if e != nil {
			return AdapterBinding{}, e
		}
		raw, _ := json.Marshal(res.Result)
		var b AdapterBinding
		e = json.Unmarshal(raw, &b)
		return b, e
	}
	first, err := put("installed", doc)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 || len(first.Hash) != 64 {
		t.Fatal(first)
	}
	broken.Store(true)
	if _, err = put("installed", doc); err == nil {
		t.Fatal("failed discovery installed")
	}
	rows, err := d.store.ListAdapters(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	broken.Store(false)
	bad := AdapterDocument{SchemaVersion: 1, Tools: map[string]external.Tool{"fixture": externalTool(srv.URL)}}
	tool := bad.Tools["fixture"]
	tool.Arguments = map[string]string{"imaginary": "key"}
	bad.Tools["fixture"] = tool
	if _, err = put("invalid", bad); err == nil {
		t.Fatal("unknown schema pin installed")
	}
	next, err := put("installed", doc)
	if err != nil || next.Revision != 2 {
		t.Fatal(next, err)
	}
	if len(d.live.config.ExternalTools) != 0 {
		t.Fatal("installation mutated shared config")
	}
	if _, _, err = d.resolveAdapter(ctx, AdapterBinding{ID: first.ID, Revision: first.Revision, Hash: "wrong"}); err == nil {
		t.Fatal("hash mismatch accepted")
	}
	bound, resolved, err := d.resolveAdapter(ctx, first)
	if err != nil || resolved.Tools["fixture"].DiscoveredSchema == nil || bound != first {
		t.Fatal(bound, err)
	}
	tool = resolved.Tools["fixture"]
	tool.Arguments["destination"] = "tamper"
	resolved.Tools["fixture"] = tool
	_, fresh, err := d.resolveAdapter(ctx, first)
	if err != nil || fresh.Tools["fixture"].Arguments["destination"] != "main" {
		t.Fatal("resolver shared mutable maps", err)
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	bound, _, err = d.resolveAdapter(ctx, first)
	if err != nil || bound != first {
		t.Fatal("restart lost immutable adapter", bound, err)
	}
	b, _, err := d.resolveAdapter(context.Background(), AdapterBinding{ID: "installed"})
	if err != nil || b.Revision != 2 {
		t.Fatal(b, err)
	}
	if _, err = call(t, d, "AdapterGet", map[string]any{"id": "installed", "revision": 1}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "AdapterList", map[string]any{}, ""); err != nil {
		t.Fatal(err)
	}
}
