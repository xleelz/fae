package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/hostguard"
)

// FAE-HOST-005, FAE-CTL-030: each line of a landing's host admission is
// audited as a landing-admission-wait row with its own outcome: a wait slice
// is waiting, the reading that lifts a pressure hold lifted, the hard
// breaker's refusal refused and an expired wait timeout. Before, the lifted
// outcome had no test, and the refusal and the timeout were audited as
// waiting. The landing admission runs through the daemon's own guard, under
// the production pressure policy on a deterministic clock.
func TestLandingAdmissionAuditsEachLineWithItsOutcome(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	g := d.live.guard
	policy := hostguard.DefaultPolicy()
	// Set before the first admission reads them (pressure() reads them once).
	g.clock, g.policy = &pressureClock{now: pressureEpoch}, &policy
	var mu sync.Mutex
	var next func(n int) hostguard.Sample
	reads := 0
	g.observe = func(context.Context) (hostguard.Sample, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return next(reads), nil
	}
	script := func(f func(n int) hostguard.Sample) {
		mu.Lock()
		defer mu.Unlock()
		next, reads = f, 0
	}
	loaded, quiet := hostSample(0.5, 9), hostSample(0.5, 1)

	// Pressure for 66 s (two slices recorded), then clear readings lift the hold.
	script(func(n int) hostguard.Sample {
		if n <= 23 {
			return loaded
		}
		return quiet
	})
	release, e := d.landingAdmission("lifted")(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if e = release(); e != nil {
		t.Fatal(e)
	}
	// The hard breaker refuses at once.
	script(func(int) hostguard.Sample {
		s := quiet
		s.Memory.Value = float64(hostguard.GiB)
		return s
	})
	var refused *AdmissionRefused
	if _, e = d.landingAdmission("refused")(ctx); !errors.As(e, &refused) {
		t.Fatal("hard breaker did not refuse", e)
	}
	// Pressure that never lifts: the bounded wait expires.
	script(func(int) hostguard.Sample { return loaded })
	var timeout *AdmissionTimeout
	if _, e = d.landingAdmission("timeout")(ctx); !errors.As(e, &timeout) || timeout.Stage != "pressure" {
		t.Fatal("landing wait did not time out", e)
	}

	var outcomes []string
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect landing admission audit", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, `SELECT outcome FROM audit WHERE action='landing-admission-wait' ORDER BY id`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var o string
			if e = rows.Scan(&o); e != nil {
				return e
			}
			outcomes = append(outcomes, o)
		}
		return rows.Err()
	}); e != nil {
		t.Fatal(e)
	}
	events, e := d.store.Events(ctx, 0, 10000)
	if e != nil {
		t.Fatal(e)
	}
	var lines []string
	for _, ev := range events {
		if ev.Kind != "landing-admission-wait" {
			continue
		}
		var body struct{ Detail string }
		if e = json.Unmarshal(ev.Payload, &body); e != nil {
			t.Fatal(e)
		}
		lines = append(lines, body.Detail)
	}
	if len(lines) != len(outcomes) || len(lines) == 0 {
		t.Fatalf("%d audited outcomes for %d admission lines", len(outcomes), len(lines))
	}
	want := map[string]string{"gate waiting ": "waiting", "gate lifted ": "lifted", "gate refused ": "refused", "gate timeout ": "timeout"}
	seen := map[string]bool{}
	for i, line := range lines {
		expected := ""
		for prefix, o := range want {
			if strings.HasPrefix(line, prefix) {
				expected = o
			}
		}
		if expected == "" || outcomes[i] != expected {
			t.Errorf("line %q audited as %q, want %q", line, outcomes[i], expected)
		}
		seen[outcomes[i]] = true
	}
	for _, o := range want {
		if !seen[o] {
			t.Errorf("no %s line was audited among %d lines", o, len(lines))
		}
	}
}
