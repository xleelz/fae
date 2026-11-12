package daemon

import (
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
)

// FAE-CP-012: the row-column limit is configured (fae serve --inline-limit)
// and governs the plane's store; a value outside 4..64 KiB is refused before
// anything starts, and zero keeps the 64 KiB default.
func TestInlineLimitConfiguresThePlane(t *testing.T) {
	cfg, ctx := fixture(t)
	for _, bad := range []int{-1, controlplane.MinInlineLimit - 1, controlplane.DefaultInlineLimit + 1} {
		cfg.InlineLimit = bad
		if d, e := Start(ctx, cfg); e == nil || !strings.Contains(e.Error(), "inline limit") {
			if d != nil {
				stop(t, d)
			}
			t.Fatalf("inline limit %d accepted: %v", bad, e)
		}
	}
	for _, tc := range []struct{ configured, effective int }{{8192, 8192}, {0, controlplane.DefaultInlineLimit}} {
		cfg.InlineLimit = tc.configured
		d, e := Start(ctx, cfg)
		if e != nil {
			t.Fatal(e)
		}
		got := d.store.InlineLimit()
		stop(t, d)
		if got != tc.effective {
			t.Fatalf("inline limit %d configured the plane with %d, want %d", tc.configured, got, tc.effective)
		}
	}
}
