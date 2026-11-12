package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowLimitNativeCapacityAuthorityAndNoOtherEdits(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(defaults.HappyScript())
	sample := cfg.WorkflowSlotSample
	cfg.WorkflowSlotSample = func(ctx context.Context) (hostguard.Sample, error) {
		s, e := sample(ctx)
		s.Memory.Value = 19 * float64(hostguard.GiB)
		return s, e
	}
	principalPath := filepath.Join(filepath.Dir(cfg.Root), "capacity-principals.json")
	tokenPath := filepath.Join(filepath.Dir(cfg.Root), "capacity-token")
	raw, _ := json.Marshal([]PrincipalCredential{{CredentialFile: tokenPath, Actor: "controller"}})
	if err := os.WriteFile(principalPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.PrincipalConfig = principalPath
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	for id, text := range map[string]string{"capacity": `{"authorize":{"classes":["capacity"]}}`, "runtime": `{"authorize":{"classes":["runtime"]}}`, "verb": `{"authorize":{"verbs":["WorkflowLimitSet"]}}`} {
		if _, err = call(t, d, "Message", map[string]string{"id": id, "text": text}, ""); err != nil {
			t.Fatal(err)
		}
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}
	delegated := func(method string, p any) (api.Response, error) {
		raw, _ := json.Marshal(p)
		return api.Call(ctx, d.layout.Socket, token, api.Request{Version: 1, Method: method, Params: raw})
	}
	for _, message := range []string{"runtime", "verb"} {
		if _, err = delegated("WorkflowLimitSet", WorkflowLimitParams{Max: 4, AuthorizingMessage: message}); err == nil {
			t.Fatal("non-capacity message authorized request", message)
		}
	}
	if _, err = delegated("WorkflowLimitSet", WorkflowLimitParams{Max: 4, AuthorizingMessage: "capacity"}); err != nil {
		t.Fatal(err)
	}
	response, err := delegated("WorkflowLimit", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]json.RawMessage
	_ = json.Unmarshal(response.Result, &view)
	var request workflowRequestInfo
	_ = json.Unmarshal(view["request"], &request)
	if request.Limit != 4 || request.Principal != "controller" || request.Message != "capacity" || request.At.IsZero() {
		t.Fatal(string(response.Result))
	}
	if _, err = delegated("WorkflowLimitSet", WorkflowLimitParams{Max: 8, AuthorizingMessage: "capacity"}); err != nil {
		t.Fatal(err)
	}
	response, err = delegated("WorkflowLimit", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(response.Result, &view)
	var host struct{ Binding string }
	_ = json.Unmarshal(view["host"], &host)
	if host.Binding != "host_memory" {
		t.Fatal("host memory did not bind requested eight", string(response.Result))
	}
	before, err := d.store.GetMarker(ctx, "workflow-slot-request")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = delegated("WorkflowLimitSet", WorkflowLimitParams{Max: 0, AuthorizingMessage: "capacity"}); err == nil {
		t.Fatal("zero request accepted")
	}
	if _, err = delegated("WorkflowLimitSet", map[string]any{"max": 5, "host_thresholds": map[string]int{"soft": 0}, "authorizing_message": "capacity"}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatal("capacity changed host settings", err)
	}
	if _, err = delegated("Drain", map[string]any{"authorizing_message": "capacity"}); err == nil {
		t.Fatal("capacity grant authorized drain")
	}
	after, err := d.store.GetMarker(ctx, "workflow-slot-request")
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused request changed stored limit", err)
	}
}
