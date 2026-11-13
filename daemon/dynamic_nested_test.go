package daemon

import (
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/effects"
)

func TestDynamicNestedCommandsRejectUnknownPolicy(t *testing.T) {
	for _, value := range []string{"allow", "off", "ACCOUNT", " "} {
		if err := validateRuntimeConfig(RuntimeConfig{NestedCommands: value}); err == nil {
			t.Fatalf("accepted nested command policy %q", value)
		}
	}
}

func TestDynamicNestedCommandsAuditedRestartPreservesLivePin(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	pin, err := d.store.GetMarker(ctx, "live-config")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "RuntimeConfig", RuntimeConfig{NestedCommands: "account", AuthorizingMessage: "missing"}, ""); err == nil {
		t.Fatal("accepted policy change without operator authority")
	}
	authorize(t, d)
	if _, err = call(t, d, "RuntimeConfig", RuntimeConfig{NestedCommands: "allow", AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("accepted unknown policy")
	}
	prior := d.live.effectiveNestedCommands()
	if _, err = call(t, d, "RuntimeConfig", RuntimeConfig{NestedCommands: "account", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if prior != effects.NestedRefuse || d.live.effectiveNestedCommands() != effects.NestedAccount {
		t.Fatal("policy update did not preserve prior dispatch snapshot")
	}
	if d.live.broker.NestedCommands != "" && d.live.broker.NestedCommands != effects.NestedRefuse {
		t.Fatal("runtime update mutated the shared broker used by in-flight effects")
	}
	gotPin, err := d.store.GetMarker(ctx, "live-config")
	if err != nil || string(gotPin) != string(pin) {
		t.Fatal("live configuration pin changed", err)
	}
	raw, err := d.store.GetMarker(ctx, "runtime-config")
	if err != nil {
		t.Fatal(err)
	}
	var stored RuntimeConfig
	if err = json.Unmarshal(raw, &stored); err != nil || stored.NestedCommands != "account" || stored.AuthorizingMessage != "" {
		t.Fatal(stored, err)
	}
	if _, err = call(t, d, "RuntimeConfig", RuntimeConfig{NestedCommands: "allow", AuthorizingMessage: "message"}, ""); err == nil {
		t.Fatal("accepted invalid update")
	}
	afterInvalid, err := d.store.GetMarker(ctx, "runtime-config")
	if err != nil || string(afterInvalid) != string(raw) {
		t.Fatal("invalid update changed durable policy", err)
	}
	if d.dynamicConfiguration()["nested_commands"] != "account" {
		t.Fatal("policy absent from runtime status")
	}
	stop(t, d)
	d = nil
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if d.live.effectiveNestedCommands() != effects.NestedAccount {
		t.Fatal("restart lost policy update")
	}
	if _, err = call(t, d, "RuntimeConfig", RuntimeConfig{NestedCommands: "refuse", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if d.live.effectiveNestedCommands() != effects.NestedRefuse {
		t.Fatal("could not restore refusing policy")
	}
}
