package external

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestEffectRefusesRuntimeSchemaDrift(t *testing.T) {
	var drift atomic.Bool
	var effects atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/schemas" {
			props := map[string]any{"pin": map[string]string{"type": "string"}}
			if drift.Load() {
				props["added"] = map[string]string{"type": "string"}
			}
			raw, _ := json.Marshal(map[string]any{"type": "object", "properties": props})
			json.NewEncoder(w).Encode(SchemaCatalog{Server: "fixture", Tools: []ToolSchema{{Name: "send", InputSchema: raw}}})
			return
		}
		effects.Add(1)
		w.WriteHeader(404)
	}))
	defer srv.Close()
	tool := Tool{Endpoint: srv.URL, Server: "fixture", Name: "send", Role: "planner", Arguments: map[string]string{"pin": "value"}}
	schema, err := new(Discovery).Discover(context.Background(), tool)
	if err != nil {
		t.Fatal(err)
	}
	tool.DiscoveredSchema = &schema
	intent, err := NewIntent(tool, "task", "stage", "key", "planner", "tree", "session", tool.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	drift.Store(true)
	if _, _, err = Query(context.Background(), tool, intent); err == nil {
		t.Fatal("query accepted schema drift")
	}
	if _, err = Perform(context.Background(), tool, intent); err == nil {
		t.Fatal("mutation accepted schema drift")
	}
	if effects.Load() != 0 {
		t.Fatal("effect endpoint contacted")
	}
}
