package daemon

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// FAE-SCH-013, FAE-OBS-006: an unreadable Herdr census skips the whole idle
// maintenance round (TTL, shedding, reconciliation, release retries). That
// refusal is never silent: it is one row, event and line per episode rather
// than per tick, recorded again after idleRefusalInterval while it persists.
// A refusal by the idle registry is paired with its line the same way, and the
// round that succeeds again closes the episode with a line.
func TestIdleMaintenanceRefusalsRecordedAndPaired(t *testing.T) {
	cfg, ctx, _, socket := idleSleeperConfig(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	// A stopped drive runs no maintenance round of its own, including one it
	// began at start, whose census runs unlocked and would interleave here.
	stopDrive(t, d)
	d.mu.Lock()
	defer d.mu.Unlock()
	// Counted from here: the drive's own round, against the real census,
	// may have recorded a refusal before it stopped.
	d.obs.mu.Lock()
	d.obs.idleRefusal = idleRefusal{}
	d.obs.mu.Unlock()
	logged, _ := os.ReadFile(d.layout.Log)
	lines := func(needle string) int {
		raw, _ := os.ReadFile(d.layout.Log)
		return strings.Count(string(raw[len(logged):]), needle)
	}
	const refusedEvents = `SELECT COUNT(*) FROM events WHERE kind='idle-maintenance-refused'`
	const refusedAudits = `SELECT COUNT(*) FROM audit WHERE action='idle-maintenance-refused'`
	eventsBefore, auditsBefore := countRows(t, d, refusedEvents), countRows(t, d, refusedAudits)
	refusals := func() int {
		return countRows(t, d, refusedEvents) - eventsBefore
	}
	maintain := func(rounds int) {
		t.Helper()
		for i := 0; i < rounds; i++ {
			if err := d.maintainIdleSessions(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	d.live.guard.inventory = func(context.Context) (map[string]int, error) {
		return nil, errors.New("herdr pane list: stale foreign socket")
	}
	maintain(5)
	if n, l := refusals(), lines("idle maintenance refused cause=census"); n != 1 || l != 1 {
		t.Fatalf("census refusal episode: events=%d lines=%d, want 1 and 1", n, l)
	}
	if audits := countRows(t, d, refusedAudits) - auditsBefore; audits != 1 {
		t.Fatal("census refusal audit rows", audits)
	}
	// The census stays unreadable past the interval: recorded again, naming
	// the refused rounds left unrecorded since.
	d.obs.mu.Lock()
	d.obs.idleRefusal.at = d.obs.idleRefusal.at.Add(-idleRefusalInterval)
	d.obs.mu.Unlock()
	maintain(1)
	if n, l := refusals(), lines("idle maintenance refused cause=census"); n != 2 || l != 2 || lines("unrecorded_refusals=4") != 1 {
		t.Fatalf("persisting census refusal: events=%d lines=%d", n, l)
	}
	// The registry refuses a census that omits its registered socket.
	d.live.guard.inventory = func(context.Context) (map[string]int, error) { return map[string]int{}, nil }
	maintain(3)
	if n, l := refusals(), lines("idle maintenance refused cause=round"); n != 3 || l != 1 {
		t.Fatalf("registry refusal: events=%d lines=%d, want 3 and 1", n, l)
	}
	d.live.guard.inventory = func(context.Context) (map[string]int, error) { return map[string]int{*socket: 1}, nil }
	maintain(2)
	if n, l := refusals(), lines("idle maintenance resumed after cause=round"); n != 3 || l != 1 {
		t.Fatalf("recovery: events=%d resumed lines=%d", n, l)
	}
}

// FAE-SCH-013: the in-flight count is read after the census, where the round
// uses it. A turn started while the census runs without the writer lock is
// counted, so panes saturated by owned idle sessions are not taken for
// three idle rounds: nothing is shed and no starvation is recorded.
func TestIdleMaintenanceCountsTurnStartedDuringCensus(t *testing.T) {
	cfg, ctx, _, socket := idleSleeperConfig(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	// A stopped drive runs no maintenance round of its own, including one it
	// began at start, whose census runs unlocked and would interleave here.
	stopDrive(t, d)
	d.mu.Lock()
	defer d.mu.Unlock()
	defer delete(d.prophetActive, "census-turn")
	d.live.guard.inventory = func(context.Context) (map[string]int, error) {
		// A Prophet turn starts while the census runs unlocked.
		d.mu.Lock()
		if d.prophetActive == nil {
			d.prophetActive = map[string]bool{}
		}
		d.prophetActive["census-turn"] = true
		d.mu.Unlock()
		return map[string]int{*socket: 60}, nil
	}
	for round := 0; round < 4; round++ {
		if err = d.maintainIdleSessions(ctx); err != nil {
			t.Fatal(err)
		}
		delete(d.prophetActive, "census-turn") // that turn ended before the next round
	}
	if d.idleSessions.Count() != 1 {
		t.Fatal("an idle session was shed while a turn was in flight")
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM events WHERE kind IN ('idle-session-shed','PaneCapStarvation')`); n != 0 {
		t.Fatal("saturation with a turn in flight recorded as idle", n)
	}
}
