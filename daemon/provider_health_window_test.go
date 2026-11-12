package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

// FAE-API-001 with the REQUIREMENTS.md configured-value conventions: an
// out-of-range ProviderHealth window from any client falls back to the
// 15-minute default, as `provider-health --window` does, and the response
// names the fallback; a window in range is used as given and an absent one
// takes the default without a warning. A window that is not an integer is
// still refused. Before, the RPC refused an out-of-range window, so a client
// other than the CLI got an error where the CLI got the default.
func TestProviderHealthRPCFallsBackOnAnOutOfRangeWindow(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	for _, c := range []struct {
		params map[string]any
		window float64
		warned bool
	}{
		{map[string]any{"window_seconds": 600}, 600, false},
		{map[string]any{"window_seconds": 86400}, 86400, false},
		{map[string]any{}, 900, false},
		{map[string]any{"window_seconds": -5}, 900, true},
		{map[string]any{"window_seconds": 86401}, 900, true},
	} {
		r, err := call(t, d, "ProviderHealth", c.params, "")
		if err != nil {
			t.Errorf("%v: %v", c.params, err)
			continue
		}
		var out map[string]any
		if err = json.Unmarshal(r.Result, &out); err != nil {
			t.Fatal(err)
		}
		warning, _ := out["window_warning"].(string)
		if out["window_seconds"] != c.window || (warning != "") != c.warned || c.warned && !strings.Contains(warning, "out of range (1..86400); using the default 900") {
			t.Errorf("%v: window %v, warning %q; want window %v, warned %t", c.params, out["window_seconds"], warning, c.window, c.warned)
		}
	}
	if _, err = call(t, d, "ProviderHealth", map[string]any{"window_seconds": 0.5}, ""); err == nil {
		t.Fatal("a window that is not an integer was accepted")
	}
}
