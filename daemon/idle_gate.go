package daemon

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/processowner"
)

// FAE-ARC-002: with no admitted work the daemon blocks on events. Timed drive
// and observability rounds still fire (host sampling and leak census are /proc
// reads), but an idle round skips every plane read; admissions, RPCs,
// completions and plane writes wake the drive at once.
const (
	driveRoundPeriod = 5 * time.Second
	// observeIdleRepair bounds how stale the alerts-file mirror may become
	// while idle when an alert was written without an event notification.
	observeIdleRepair = 10 * time.Minute
	// observeIdleCensus bounds how stale the own-tree leak census (memory and
	// defunct children of an unchanged tree) may become while idle.
	observeIdleCensus = time.Minute
)

// idleRound reports, under d.mu, that a timed round has nothing to do before
// the next one: no non-terminal job other than an operator-held Escalated
// one, nothing active, pending, quarantined or checkpointing, no live session
// to shed, no ticket emission outstanding and no cron or loop occurrence (or
// bound) due within a round.
func (d *Daemon) idleRound(now time.Time) bool {
	if len(d.active) > 0 || d.pendingCompletion != nil || d.pendingCheckpoint != nil || d.pendingLoop != nil || d.loopCheckpoint != nil || len(d.loopActive) > 0 || len(d.prophetActive) > 0 || len(d.quarantined) > 0 || d.backlogUncertain {
		return false
	}
	if d.idleSessions != nil && d.idleSessions.Count() > 0 {
		return false
	}
	if t, ok := d.ticketStatus.Load().(ticketStatus); ok && (t.Depth > 0 || t.Failures > 0 || t.Uncertain > 0) {
		return false
	}
	for _, s := range d.jobs {
		if state := domain.State(s.State); !state.Terminal() && state != domain.Escalated {
			return false
		}
	}
	horizon := now.Add(driveRoundPeriod)
	for _, c := range d.crons {
		if c.State != "active" {
			continue
		}
		if next, ok := c.At(c.Next); !ok || !next.After(horizon) || !c.Spec.End.IsZero() && !c.Spec.End.After(horizon) {
			return false
		}
	}
	for _, l := range d.loops {
		if l.State == "active" && (l.InFlight || !l.Next.After(horizon) || !l.Spec.ExpiresAt.After(horizon)) {
			return false
		}
	}
	return true
}

// signalUnlessIdle is the drive's timed round: it signals a dispatch round
// only when one could do something, and stamps the verdict for observability
// rounds, which must not take the writer lock.
func (d *Daemon) signalUnlessIdle() {
	d.mu.Lock()
	now := time.Now()
	idle := d.idleRound(now)
	d.mu.Unlock()
	if idle {
		d.obs.driveIdleAt.Store(now.UnixNano())
		return
	}
	d.obs.driveIdleAt.Store(0)
	d.signal()
}

// driveIdle reports a recent idle verdict of the drive's timed round; a stale
// or absent one (a stopped or busy drive) is not idle.
func (d *Daemon) driveIdle(now time.Time) bool {
	at := d.obs.driveIdleAt.Load()
	return at != 0 && now.Sub(time.Unix(0, at)) <= 2*driveRoundPeriod
}

// observeIdle reports which steps of this observability round may be skipped:
// plane steps while the daemon is idle and nothing was written since the last
// plane round. The leak census samples every drive round (FAE-HOST-022), and
// an idle daemon runs none: while idle with an unchanged descendant set it is
// skipped (the walk listing that set is the change detector) but for an
// own-tree census every observeIdleCensus. A changed set or a busy drive gets
// the full /proc census, which also reports foreign processes; plane steps and
// that full census run at least every observeIdleRepair.
func (d *Daemon) observeIdle() (plane bool, leaks leakScope) {
	d.eventMu.Lock()
	written := d.eventWake
	d.eventMu.Unlock()
	now := d.obs.clock()
	idle := d.driveIdle(now)
	tree := ""
	if pids, err := processowner.DescendantPIDs(os.Getpid()); err == nil {
		sort.Ints(pids)
		tree = fmt.Sprint(pids)
	}
	d.obs.mu.Lock()
	defer d.obs.mu.Unlock()
	plane = idle && written == d.obs.planeWake && now.Sub(d.obs.planeAt) < observeIdleRepair
	if !plane {
		d.obs.planeWake, d.obs.planeAt = written, now
	}
	switch {
	case !idle || tree == "" || tree != d.obs.censusTree || now.Sub(d.obs.censusAt) >= observeIdleRepair:
		leaks = leakHostWide
		d.obs.censusTree, d.obs.censusAt, d.obs.ownAt = tree, now, now
	case now.Sub(d.obs.ownAt) >= observeIdleCensus:
		leaks = leakOwnTree
		d.obs.ownAt = now
	}
	return plane, leaks
}

// leakScope is an observability round's leak census: none, the daemon's own
// tree, or every process on the host.
type leakScope int

const (
	leakSkip leakScope = iota
	leakOwnTree
	leakHostWide
)
