package external

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func schemaTool(endpoint string) Tool {
	return Tool{Endpoint: endpoint, Server: "vendor", Name: "change", Role: "daemon", Arguments: map[string]string{"space": "room", "enabled": "true", "sequence": "9007199254740993"}}
}
func schemaDescriptor() ToolSchema {
	return ToolSchema{Name: "change", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["space","enabled","sequence"],"properties":{"space":{"type":"string","enum":["room"]},"enabled":{"type":"boolean"},"sequence":{"type":"integer","minimum":9007199254740993}}}`)}
}
func TestDiscoveredPinValidation(t *testing.T) {
	cases := []struct {
		name string
		edit func(*ToolSchema, map[string]string)
		want string
	}{
		{"valid", func(*ToolSchema, map[string]string) {}, ""},
		{"missing key", func(s *ToolSchema, p map[string]string) { p["guessed"] = "true" }, "absent"},
		{"wrong type", func(s *ToolSchema, p map[string]string) { p["enabled"] = "notbool" }, "schema type"},
		{"enum", func(s *ToolSchema, p map[string]string) { p["space"] = "other" }, "must be"},
		{"required", func(s *ToolSchema, p map[string]string) { delete(p, "enabled") }, "missing property"},
		{"large integer precision", func(s *ToolSchema, p map[string]string) { p["sequence"] = "9007199254740992" }, "minimum"},
		{"external reference", func(s *ToolSchema, p map[string]string) {
			s.InputSchema = json.RawMessage(`{"type":"object","properties":{"space":{"type":"string","$ref":"https://untrusted.invalid/schema"}}}`)
			delete(p, "enabled")
			delete(p, "sequence")
		}, "forbidden"},
		{"duplicate schema key", func(s *ToolSchema, p map[string]string) {
			s.InputSchema = json.RawMessage(`{"type":"object","type":"object","properties":{"space":{"type":"string"}}}`)
			delete(p, "enabled")
			delete(p, "sequence")
		}, "duplicate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := schemaDescriptor()
			p := schemaTool("").Arguments
			tc.edit(&s, p)
			got, err := ValidatePins(s, p)
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if fmt.Sprint(got["sequence"]) != "9007199254740993" {
					t.Fatal(got)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s got %v", tc.want, err)
			}
		})
	}
}
func TestSchemaDiscoveryExactIdentityAndBounds(t *testing.T) {
	cases := []struct {
		name, body string
		code       int
		want       string
	}{
		{"valid", `{"server":"vendor","tools":[{"name":"mcp__vendor__change","inputSchema":` + string(schemaDescriptor().InputSchema) + `}]}`, 200, ""},
		{"wrongserver", `{"server":"other","tools":[]}`, 200, "server"},
		{"wrongtool", `{"server":"vendor","tools":[{"name":"mcp__other__change"}]}`, 200, "missing"},
		{"duplicates", `{"server":"vendor","tools":[{"name":"change"},{"name":"change"}]}`, 200, "duplicate"},
		{"duplicated identity", `{"server":"vendor","server":"vendor","tools":[]}`, 200, "duplicate"},
		{"trailing", `{} {}`, 200, "trailing"},
		{"oversize", strings.Repeat(" ", (1<<20)+1), 200, "large"},
		{"missing endpoint", `{}`, 404, "404"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/schemas" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
				}
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			_, err := (&Discovery{}).Discover(context.Background(), schemaTool(srv.URL))
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %s got %v", tc.want, err)
			}
		})
	}
}
func TestRoomRefreshAndSchemaDerivedAction(t *testing.T) {
	now := time.Now()
	var reads atomic.Int32
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/schemas":
			_ = json.NewEncoder(w).Encode(SchemaCatalog{Server: "vendor", Tools: []ToolSchema{schemaDescriptor()}})
		case "/rooms/room/resources":
			reads.Add(1)
			if fail.Load() {
				http.Error(w, "offline", 503)
				return
			}
			_ = json.NewEncoder(w).Encode(RoomResources{Server: "vendor", Room: "room", Actions: []RoomAction{{Name: "vendor-action-xyz", Tool: "mcp__vendor__change", Arguments: map[string]any{"space": "room", "enabled": true, "sequence": json.Number("9007199254740993")}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	d := &Discovery{TTL: time.Second, Now: func() time.Time { return now }}
	base := schemaTool(srv.URL)
	got, err := d.ResolveRoomAction(context.Background(), base, "room", "vendor-action-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "change" || got.Arguments["enabled"] != "true" || got.Arguments["sequence"] != "9007199254740993" || got.DiscoveredSchema == nil {
		t.Fatal(got)
	}
	r, err := d.Room(context.Background(), base, "room")
	if err != nil {
		t.Fatal(err)
	}
	r.Actions[0].Arguments["space"] = "corrupt"
	again, err := d.Room(context.Background(), base, "room")
	if err != nil || again.Actions[0].Arguments["space"] != "room" {
		t.Fatal(again, err)
	}
	if reads.Load() != 1 {
		t.Fatal("cache miss", reads.Load())
	}
	if _, err = d.ResolveRoomAction(context.Background(), base, "room", "guessed"); err == nil || !strings.Contains(err.Error(), "room") {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	fail.Store(true)
	if _, err = d.Room(context.Background(), base, "room"); err == nil {
		t.Fatal("stale cache used")
	}
	fail.Store(false)
	if _, err = d.Room(context.Background(), base, "room"); err != nil {
		t.Fatal(err)
	}
	if reads.Load() != 3 {
		t.Fatal(reads.Load())
	}
	d.TTL = 6 * time.Minute
	if _, err = d.Room(context.Background(), base, "room"); err == nil {
		t.Fatal("unbounded ttl")
	}
}
func TestTypedIntentPinsAndSchemaDrift(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/schemas" {
			json.NewEncoder(w).Encode(SchemaCatalog{Server: "vendor", Tools: []ToolSchema{schemaDescriptor()}})
			return
		}
		requests.Add(1)
		var i Intent
		if err := json.NewDecoder(r.Body).Decode(&i); err != nil {
			t.Error(err)
		}
		if _, ok := i.TypedArguments["enabled"].(bool); !ok {
			t.Error("wire boolean absent", i.TypedArguments)
		}
		_ = json.NewEncoder(w).Encode(Receipt{})
	}))
	defer srv.Close()
	tool := schemaTool(srv.URL)
	s := schemaDescriptor()
	tool.DiscoveredSchema = &s
	i, err := NewIntent(tool, "task", "stage", "key", "daemon", "tree", "session", tool.Arguments)
	if err != nil {
		t.Fatal(err)
	}
	if i.SchemaDigest == "" {
		t.Fatal("missing durable schema pin")
	}
	if _, err = Perform(context.Background(), tool, i); err != nil {
		t.Fatal(err)
	}
	s.InputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
	if _, _, err = Query(context.Background(), tool, i); err == nil {
		t.Fatal("changed schema accepted")
	}
	if requests.Load() != 1 {
		t.Fatal("schema drift reached network")
	}
}
func TestSchemaPinGuard(t *testing.T) {
	if !CanUseSchemaPin(true) || CanUseSchemaPin(false) {
		t.Fatal("undeclared pin allowed")
	}
}

func TestRoomPinsAndDynamicBindings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/schemas" {
			_ = json.NewEncoder(w).Encode(SchemaCatalog{Server: "vendor", Tools: []ToolSchema{schemaDescriptor()}})
			return
		}
		_ = json.NewEncoder(w).Encode(RoomResources{Server: "vendor", Room: "room", Actions: []RoomAction{{Name: "change", Tool: "change", Arguments: map[string]any{"space": "other", "enabled": true, "sequence": json.Number("9007199254740993")}}}})
	}))
	defer srv.Close()
	d := &Discovery{}
	base := schemaTool(srv.URL)
	if _, err := d.ResolveRoomAction(context.Background(), base, "room", "change"); err == nil || !strings.Contains(err.Error(), "installed pin") {
		t.Fatal(err)
	}
	got, err := d.ResolveRoomActionWithBindings(context.Background(), base, "room", "change", map[string]string{"space": "room"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Arguments["space"] != "room" {
		t.Fatal(got)
	}
	if _, err = d.ResolveRoomActionWithBindings(context.Background(), base, "room", "change", map[string]string{"guessed": "room", "space": "room"}); err == nil {
		t.Fatal("undeclared binding accepted")
	}
}
func TestSchemaDiscoveryRedirectAndCancellation(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, 302) }))
	defer srv.Close()
	d := &Discovery{}
	if _, err := d.Discover(context.Background(), schemaTool(srv.URL)); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatal(err)
	}
	if targetCalls.Load() != 0 {
		t.Fatal("redirect followed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.Discover(ctx, schemaTool(srv.URL)); err == nil {
		t.Fatal("cancel ignored")
	}
	if _, err := d.Discover(context.Background(), schemaTool("http://example.com")); err == nil {
		t.Fatal("remote plaintext allowed")
	}
}

func TestTemplatePinsRequireConcreteRuntimeValidation(t *testing.T) {
	schema := ToolSchema{Name: "revise", InputSchema: json.RawMessage(`{"type":"object","required":["revision","room"],"properties":{"revision":{"type":"integer","enum":[2]},"room":{"type":"string","enum":["allowed"]}}}`)}
	pins := map[string]string{"revision": "$revision", "room": "allowed"}
	if err := ValidateTemplatePins(schema, pins, map[string]string{"revision": "integer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidatePins(schema, pins); err == nil {
		t.Fatal("unsubstituted token reached concrete call")
	}
	pins["revision"] = "2"
	if _, err := ValidatePins(schema, pins); err != nil {
		t.Fatal(err)
	}
	pins["revision"] = "3"
	if _, err := ValidatePins(schema, pins); err == nil {
		t.Fatal("runtime enum ignored")
	}
	pins["revision"] = `"2"`
	if _, err := ValidatePins(schema, pins); err == nil {
		t.Fatal("runtime type ignored")
	}
	delete(pins, "revision")
	if err := ValidateTemplatePins(schema, pins, map[string]string{"revision": "integer"}); err != nil {
		t.Fatal("configured missing dynamic required field refused", err)
	}
	pins["room"] = "other"
	if err := ValidateTemplatePins(schema, pins, map[string]string{"revision": "integer"}); err == nil {
		t.Fatal("static enum bypassed")
	}
	if err := ValidateTemplatePins(schema, pins, map[string]string{"revision": "string"}); err == nil {
		t.Fatal("source type mismatch")
	}
	if err := ValidateTemplatePins(schema, pins, map[string]string{"invented": "string"}); err == nil {
		t.Fatal("invented key")
	}
}
