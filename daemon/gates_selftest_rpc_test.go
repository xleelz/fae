package daemon

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/api"
)

// FAE-CTL-050, FAE-API-010: gates self-test has a daemon read RPC.
func TestGatesSelfTestRPCReportsEveryGate(t *testing.T) {
	d := &Daemon{}
	out, err := d.handleDirect(context.Background(), api.Request{Version: api.Version, Method: "GatesSelfTest", Params: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(out)
	var body struct {
		Passed bool `json:"passed"`
		Gates  []struct {
			Gate    string `json:"gate"`
			CanFail bool   `json:"can_fail"`
		} `json:"gates"`
	}
	if err = json.Unmarshal(raw, &body); err != nil || !body.Passed || len(body.Gates) != len(GateSelfTestCases()) {
		t.Fatal(string(raw), err)
	}
	if _, err = d.handleDirect(context.Background(), api.Request{Version: api.Version, Method: "GatesSelfTest", Params: json.RawMessage(`{"x":1}`)}); err == nil {
		t.Fatal("unknown parameter accepted")
	}
}
