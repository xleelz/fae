package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/providerhealth"
)

func TestProviderHealthAndTransactionalLogRPC(t *testing.T) {
	c, ctx := fixture(t)
	d, err := Start(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	o := controlplane.ProviderObservation{ID: "observed-session", TaskID: "observed-task", Provider: "codex", Model: "test-model", Outcome: providerhealth.RateLimited, At: time.Now().UTC().Truncate(time.Microsecond)}
	for range 2 {
		if err := d.store.ObserveProvider(ctx, o); err != nil {
			t.Fatal(err)
		}
	}
	reply, err := call(t, d, "ProviderHealth", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var health struct {
		Bindings []struct {
			Eligible bool    `json:"eligible"`
			Score    float64 `json:"effective_score"`
		} `json:"bindings"`
	}
	if err := json.Unmarshal(reply.Result, &health); err != nil {
		t.Fatal(err)
	}
	if len(health.Bindings) != 1 || health.Bindings[0].Eligible || health.Bindings[0].Score < 40 || health.Bindings[0].Score > 41 {
		t.Fatalf("health: %s", reply.Result)
	}
	reply, err = call(t, d, "ProviderObservations", map[string]any{"limit": 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	var lines []controlplane.ProviderLogLine
	if err := json.Unmarshal(reply.Result, &lines); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Session != o.ID || !strings.Contains(lines[0].Line, "rate-limited") {
		t.Fatalf("log: %s", reply.Result)
	}
	reply, err = call(t, d, "ProviderObservations", map[string]any{"after": lines[0].Sequence, "limit": 1}, "")
	if err != nil || string(reply.Result) != "[]" {
		t.Fatalf("idempotence/pagination: %s %v", reply.Result, err)
	}
	if _, err := call(t, d, "ProviderObservations", map[string]any{"limit": 501}, ""); err == nil {
		t.Fatal("unbounded log accepted")
	}
	if _, err := call(t, d, "ProviderHealth", map[string]any{"unknown": true}, ""); err == nil {
		t.Fatal("unknown fields accepted")
	}
}
