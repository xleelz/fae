package daemon

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
)

// Exercise the real daemon authentication, authority consumption, submission
// audit, and private launcher transport without stopping the fixture daemon.
func TestControllerOperationNativeAuthorityAndSubmission(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stop(t, d) })
	socket := filepath.Join(filepath.Dir(d.layout.Root), "operation.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("1", 64)
	if err = os.WriteFile(controller.LauncherKeyPath(socket), []byte(key), 0600); err != nil {
		t.Fatal(err)
	}
	var submitted atomic.Int32
	received := make(chan controller.OperationRequest, 1)
	served := make(chan struct{})
	go func() {
		defer close(served)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
			var request struct {
				Token     string                      `json:"token"`
				Operation controller.OperationRequest `json:"operation"`
			}
			if err = json.NewDecoder(conn).Decode(&request); err == nil && request.Token == key {
				submitted.Add(1)
				received <- request.Operation
				_ = json.NewEncoder(conn).Encode(map[string]any{"operation": controller.OperationResult{ID: "submitted", Done: false}})
			}
			conn.Close()
		}
	}()
	t.Cleanup(func() { listener.Close(); <-served })
	p := ControllerOperationParams{AuthorizingMessage: "missing", Operation: controller.OperationRequest{Action: "stop", Options: controller.Options{Message: "missing", Note: "planned", Launcher: socket}}}
	if _, err = call(t, d, "ControllerOperation", p, ""); err == nil {
		t.Fatal("unknown authorizing message accepted")
	}
	authorize(t, d)
	p.AuthorizingMessage = "message"
	p.Operation.Options.Message = "different"
	if _, err = call(t, d, "ControllerOperation", p, ""); err == nil || !strings.Contains(err.Error(), "citation mismatch") {
		t.Fatalf("mismatched citation: %v", err)
	}
	p.Operation.Options.Message = "message"
	roleToken := []byte(strings.Repeat("r", 32))
	if err = d.rpc.AddPrincipal(roleToken, api.Principal{Actor: "workflow-role"}); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(p)
	if _, err = api.Call(ctx, d.layout.Socket, roleToken, api.Request{Version: api.Version, Method: "ControllerOperation", Params: raw}); err == nil {
		t.Fatal("workflow role submitted lifecycle operation")
	}
	if submitted.Load() != 0 {
		t.Fatal("refused request reached launcher")
	}
	response, err := call(t, d, "ControllerOperation", p, "")
	if err != nil {
		t.Fatal(err)
	}
	var result controller.OperationResult
	if err = json.Unmarshal(response.Result, &result); err != nil || result.ID != "submitted" || result.Done {
		t.Fatalf("submission receipt %+v %v", result, err)
	}
	select {
	case request := <-received:
		if request.Action != "stop" || request.Authorization == "" {
			t.Fatalf("wrong or unsigned operation: %+v", request)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	rows, err := d.store.TaskDecisionPayloads(ctx, "stop", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Kind != "controller-operation" || rows[0].Role != "operator" || !strings.Contains(string(rows[0].Payload), `"authorizing_message":"message"`) {
		t.Fatalf("missing submission audit: %+v", rows)
	}
	if submitted.Load() != 1 {
		t.Fatalf("submissions=%d", submitted.Load())
	}
}
