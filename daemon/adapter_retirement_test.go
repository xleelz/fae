package daemon

import (
	"encoding/json"
	"errors"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/external"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdapterRetirementNativeTombstone(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !serveExternalFixtureSchema(w, r) {
			t.Error("unexpected effect")
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	if _, e = call(t, d, "AdapterRetire", AdapterRetireParams{ID: "missing", Reason: "reason", AuthorizingMessage: "message"}, ""); e == nil {
		t.Fatal("missing accepted")
	}
	doc := AdapterDocument{SchemaVersion: 1, Tools: map[string]external.Tool{"fixture": externalTool(srv.URL)}}
	if _, e = call(t, d, "AdapterPut", AdapterPutParams{ID: "one", Document: doc, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "AdapterRetire", AdapterRetireParams{ID: "one", Reason: "reason"}, ""); e == nil {
		t.Fatal("missing authority")
	}
	for _, reason := range []string{"first", "second"} {
		if _, e = call(t, d, "AdapterRetire", AdapterRetireParams{ID: "one", Reason: reason, AuthorizingMessage: "message"}, ""); e != nil {
			t.Fatal(e)
		}
	}
	raw, e := d.store.AdapterRetirement(ctx, "one")
	if e != nil {
		t.Fatal(e)
	}
	var record map[string]any
	if e = json.Unmarshal(raw, &record); e != nil {
		t.Fatal(e)
	}
	if record["reason"] != "first" || record["actor"] != "operator" || record["retired_at"] == "" {
		t.Fatal(record)
	}
	if !errors.Is(d.store.AdapterActive(ctx, "one"), controlplane.ErrAdapterRetired) {
		t.Fatal("active")
	}
	rev, _, e := d.store.GetAdapter(ctx, "one", 1)
	if e != nil || !rev.Retired {
		t.Fatal(rev, e)
	}
	list, e := d.store.ListAdapters(ctx)
	if e != nil || len(list) != 1 || !list[0].Retired {
		t.Fatal(list, e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	after, e := d.store.AdapterRetirement(ctx, "one")
	if e != nil || string(raw) != string(after) {
		t.Fatal(string(after), e)
	}
	if _, e = call(t, d, "AdapterPut", AdapterPutParams{ID: "one", Document: doc, AuthorizingMessage: "message"}, ""); e == nil {
		t.Fatal("retired install")
	}
}
