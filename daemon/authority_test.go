package daemon

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
)

func TestPermissionTableEveryActorAndAction(t *testing.T) {
	actors := []string{"operator", "controller", "prophet", "workflow-role", "qa"}
	for action, p := range actionPermissions {
		for _, actor := range actors {
			t.Run(actor+"/"+action, func(t *testing.T) {
				owner := make([]byte, 32)
				token := make([]byte, 32)
				token[0] = 1
				var err error
				server, _ := api.New(owner, func(ctx context.Context, r api.Request) (any, error) {
					_, _, _, err = authorizePrincipal(ctx, r)
					return nil, err
				})
				if actor == "operator" {
					token = owner
				} else if e := server.AddPrincipal(token, api.Principal{Actor: actor}); e != nil {
					t.Fatal(e)
				}
				raw, _ := json.Marshal(api.Request{Version: 1, Method: action, Params: json.RawMessage(`{"action":"create"}`)})
				req := httptest.NewRequest("POST", "/v1/rpc", strings.NewReader(string(raw)))
				req.Header.Set("Authorization", hex.EncodeToString(token))
				server.ServeHTTP(httptest.NewRecorder(), req)
				want := actor == "operator" || (actor == "controller" && action != "ProphetChat") || p.Read || (actor == "prophet" && p.Prophet)
				if (err == nil) != want {
					t.Fatalf("allowed=%v want=%v error=%v", err == nil, want, err)
				}
			})
		}
	}
	for _, method := range []string{"ApproveCode", "WaiveGate", "WritePlane", "unknown"} {
		if _, _, _, e := authorizePrincipal(context.Background(), api.Request{Method: method}); e == nil {
			t.Fatal(method)
		}
	}
	for _, a := range []bool{false, true} {
		for _, k := range []bool{false, true} {
			for _, p := range []bool{false, true} {
				if MayPerformAction(a, k, p) != (a && k && p) {
					t.Fatal("guard")
				}
			}
		}
	}
}
func TestAuthorityNativeConsumptionAndDelegation(t *testing.T) {
	cfg, ctx := fixture(t)
	configPath := filepath.Join(filepath.Dir(cfg.Root), "principals.json")
	controllerPath := filepath.Join(filepath.Dir(cfg.Root), "controller-token")
	rolePath := filepath.Join(filepath.Dir(cfg.Root), "role-token")
	raw, _ := json.Marshal([]PrincipalCredential{{CredentialFile: controllerPath, Actor: "controller"}, {CredentialFile: rolePath, Actor: "workflow-role"}})
	if e := os.WriteFile(configPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	cfg.PrincipalConfig = configPath
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	record := func(id, text string) {
		t.Helper()
		if _, e := call(t, d, "Message", map[string]string{"id": id, "text": text}, ""); e != nil {
			t.Fatal(e)
		}
	}
	install := InstallParams{ID: defaults.Workflow().ID, Document: defaults.Workflow(), AuthorizingMessage: "plain"}
	record("plain", "Install the workflow")
	if _, e := call(t, d, "WorkflowInstall", install, ""); e != nil {
		t.Fatal(e)
	}
	if _, e := call(t, d, "Drain", map[string]string{"authorizing_message": "plain"}, ""); e == nil || !strings.Contains(e.Error(), "incompatible") {
		t.Fatalf("cross-class %v", e)
	}
	stop(t, d)
	d = nil
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := call(t, d, "Drain", map[string]string{"authorizing_message": "plain"}, ""); e == nil || !strings.Contains(e.Error(), "incompatible") {
		t.Fatalf("restart cross-class %v", e)
	}
	delegated := func(path, method string, params any) error {
		t.Helper()
		token, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(params)
		_, e = api.Call(ctx, d.layout.Socket, token, api.Request{Version: 1, Method: method, Params: raw})
		return e
	}
	if e := delegated(rolePath, "WorkflowInstall", install); e == nil {
		t.Fatal("role mutation")
	}
	if e := delegated(controllerPath, "WorkflowInstall", install); e == nil || !strings.Contains(e.Error(), "explicitly") {
		t.Fatalf("undirected controller %v", e)
	}
	record("directed", `{"text":"Install only","authorize":{"verbs":["WorkflowInstall"]}}`)
	install.AuthorizingMessage = "directed"
	if e := delegated(controllerPath, "WorkflowInstall", install); e != nil {
		t.Fatal(e)
	}
	record("class", `{"text":"Governance changes","authorize":{"classes":["governance"]}}`)
	install.AuthorizingMessage = "class"
	if e := delegated(controllerPath, "WorkflowInstall", install); e != nil {
		t.Fatal(e)
	}
	if e := d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect authority evidence", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		var count int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='controller' AND action='WorkflowInstall' AND message_id IN ('directed','class')`).Scan(&count); e != nil {
			return e
		}
		if count != 2 {
			return fmt.Errorf("delegated citation audit count %d", count)
		}
		var consumed string
		if e := tx.QueryRowContext(ctx, `SELECT consumed_by FROM operator_messages WHERE message_id='plain'`).Scan(&consumed); e != nil {
			return e
		}
		if consumed != `["governance"]` {
			return fmt.Errorf("consumption changed after refusals: %s", consumed)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	record("multi", `{"text":"Install and drain","authorize":{"classes":["governance","runtime"]}}`)
	install.AuthorizingMessage = "multi"
	if _, e := call(t, d, "WorkflowInstall", install, ""); e != nil {
		t.Fatal(e)
	}
	if _, e := call(t, d, "Drain", map[string]string{"authorizing_message": "multi"}, ""); e != nil {
		t.Fatal(e)
	}
}

func TestTaskPrincipalTransportScope(t *testing.T) {
	owner := make([]byte, 32)
	token := make([]byte, 32)
	token[0] = 2
	var authErr error
	s, _ := api.New(owner, func(ctx context.Context, r api.Request) (any, error) {
		_, _, _, authErr = authorizePrincipal(ctx, r)
		return nil, authErr
	})
	if e := s.AddPrincipal(token, api.Principal{Actor: "prophet", Task: "owned"}); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		method, params string
		allowed        bool
	}{
		{"TaskHistory", `{"id":"owned"}`, true}, {"TaskHistory", `{"id":"other","task":"owned"}`, false}, {"TaskHistory", `{"id":"other"}`, false}, {"Board", `{}`, false}, {"RestartWithInformation", `{"id":"owned"}`, true}, {"Cancel", `{"id":"owned"}`, false},
	} {
		raw, _ := json.Marshal(api.Request{Version: 1, Method: tc.method, Params: json.RawMessage(tc.params)})
		r := httptest.NewRequest("POST", "/v1/rpc", strings.NewReader(string(raw)))
		r.Header.Set("Authorization", hex.EncodeToString(token))
		s.ServeHTTP(httptest.NewRecorder(), r)
		if (authErr == nil) != tc.allowed {
			t.Fatalf("%s %s: %v", tc.method, tc.params, authErr)
		}
	}
}
