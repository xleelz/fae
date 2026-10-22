package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xleelz/fae/external"
)

func TestStartupExternalSchemaPins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/schemas" {
			t.Fatal(r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(external.SchemaCatalog{Server: "installed", Tools: []external.ToolSchema{{Name: "update", InputSchema: json.RawMessage(`{"type":"object","required":["actual_room"],"properties":{"actual_room":{"type":"string"}}}`)}}})
	}))
	defer srv.Close()
	tool := external.Tool{Endpoint: srv.URL, Server: "installed", Name: "update", Role: "daemon", Arguments: map[string]string{"actual_room": "room"}}
	cfg := LiveConfig{ExternalTools: map[string]external.Tool{"alias": tool}}
	if err := ValidateExternalSchemas(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.ExternalTools["alias"].DiscoveredSchema == nil {
		t.Fatal("schema not installed")
	}
	tool.Arguments = map[string]string{"imagined_room": "room"}
	cfg.ExternalTools["alias"] = tool
	if err := ValidateExternalSchemas(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "alias") || !strings.Contains(err.Error(), "imagined_room") {
		t.Fatal(err)
	}
}

func TestStartupSchemaDaemonTokenAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(external.SchemaCatalog{Server: "installed", Tools: []external.ToolSchema{{Name: "vendor_revise", InputSchema: json.RawMessage(`{"type":"object","required":["revision","room"],"properties":{"revision":{"type":"integer","enum":[2]},"room":{"type":"string"}}}`)}}})
	}))
	defer srv.Close()
	cfg := LiveConfig{ExternalTools: map[string]external.Tool{"alias": {Endpoint: srv.URL, Server: "installed", Name: "vendor_revise", Role: "daemon", Arguments: map[string]string{"revision": "$revision", "room": "actual"}}}, ReviewPolicies: map[string]external.ReviewPolicy{"task": {Operations: map[string]string{"alias": "review_revision"}}}}
	if err := ValidateExternalSchemas(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	tool := cfg.ExternalTools["alias"]
	tool.Arguments["revision"] = "$untrusted"
	cfg.ExternalTools["alias"] = tool
	if err := ValidateExternalSchemas(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "unknown daemon token") {
		t.Fatal(err)
	}
	tool.Arguments["revision"] = "$revision"
	cfg.ExternalTools["alias"] = tool
	cfg.ReviewPolicies = nil
	if err := ValidateExternalSchemas(context.Background(), cfg); err == nil {
		t.Fatal("generic tool token bypass")
	}
}
