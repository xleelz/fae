package scheduler

import (
	"context"
	"sort"
	"time"
)

// ReclaimEntry is one ledger entry and the owner liveness that decided it.
type ReclaimEntry struct {
	Pool     string `json:"pool,omitempty"`
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Owner    Owner  `json:"owner"`
	Weight   int    `json:"weight,omitempty"`
	Liveness string `json:"liveness"`
	State    string `json:"state,omitempty"`
}
type ReclaimReport struct {
	Reclaimed []ReclaimEntry `json:"reclaimed"`
	Kept      []ReclaimEntry `json:"kept"`
}

func (l Liveness) String() string {
	switch l {
	case Alive:
		return "alive"
	case Dead:
		return "dead"
	}
	return "unknown"
}

// ReclaimDeadOwners drops holders and waiters whose owner is proved dead (pid
// absent or start time differs) and keeps alive and ambiguous ones. Unlike
// Advance it grants nothing and never applies wait or expiry bounds.
func (p *Pool) ReclaimDeadOwners(probe func(Owner) Liveness) ReclaimReport {
	seen := map[Owner]Liveness{}
	live := func(o Owner) Liveness {
		v, ok := seen[o]
		if !ok {
			v = probe(o)
			seen[o] = v
		}
		return v
	}
	r := ReclaimReport{Reclaimed: []ReclaimEntry{}, Kept: []ReclaimEntry{}}
	sort := func(kind string, req Request) bool {
		e := ReclaimEntry{ID: req.ID, Kind: kind, Owner: req.Owner, Weight: req.Weight, Liveness: live(req.Owner).String()}
		if live(req.Owner) == Dead {
			r.Reclaimed = append(r.Reclaimed, e)
			return false
		}
		r.Kept = append(r.Kept, e)
		return true
	}
	holders := []Lease{}
	for _, h := range p.Holders {
		if sort("holder", h.Request) {
			holders = append(holders, h)
		}
	}
	waiters := []Request{}
	for _, w := range p.Waiting {
		if sort("waiter", w) {
			waiters = append(waiters, w)
		}
	}
	p.Holders, p.Waiting = holders, waiters
	return r
}

// ReclaimDead reconciles the host heavy-op ledger on demand under its lock,
// with one ledger audit entry per reclaimed request. A dry run writes nothing;
// a missing or unreadable ledger fails closed.
func (l *Ledger) ReclaimDead(ctx context.Context, probe func(Owner) Liveness, dryRun bool) (ReclaimReport, error) {
	var r ReclaimReport
	err := l.transact(ctx, !dryRun, func(s *ledgerState) error {
		r = s.Pool.ReclaimDeadOwners(probe)
		poolIDs := make([]string, 0, len(s.ProviderPools))
		for poolID := range s.ProviderPools {
			poolIDs = append(poolIDs, poolID)
		}
		sort.Strings(poolIDs)
		for _, poolID := range poolIDs {
			pp := s.ProviderPools[poolID]
			pr := pp.ReclaimDeadOwners(probe)
			for _, e := range pr.Reclaimed {
				e.Pool = "provider_pool:" + poolID
				r.Reclaimed = append(r.Reclaimed, e)
			}
			for _, e := range pr.Kept {
				e.Pool = "provider_pool:" + poolID
				r.Kept = append(r.Kept, e)
			}
			s.ProviderPools[poolID] = pp
		}
		now := time.Now().UTC()
		for _, e := range r.Reclaimed {
			s.Audit = append(s.Audit, LeaseAudit{At: now, Action: "reclaimed", ID: e.ID, Owner: e.Owner, Reason: "operator reclaim: owner proved dead by pid and start time"})
		}
		return nil
	})
	return r, err
}

// ReclaimDead reports and (unless dryRun) removes pane reservations whose
// owner is proved dead by exact birth identity, using the access-time reconcile.
func (p *PaneLedger) ReclaimDead(ctx context.Context, dryRun bool) (ReclaimReport, error) {
	r := ReclaimReport{Reclaimed: []ReclaimEntry{}, Kept: []ReclaimEntry{}}
	err := p.ledger.transact(ctx, !dryRun, func(s *ledgerState) error {
		r = ReclaimReport{Reclaimed: []ReclaimEntry{}, Kept: []ReclaimEntry{}}
		before := append([]PaneReservation(nil), s.Panes...)
		seen := map[string]bool{}
		memo := *p
		memo.probe = func(x PaneReservation) bool {
			v, ok := seen[x.ID]
			if !ok {
				v = p.probe(x)
				seen[x.ID] = v
			}
			return v
		}
		if e := memo.reconcile(s, time.Now().UTC()); e != nil {
			return e
		}
		for _, x := range before {
			e := ReclaimEntry{ID: x.ID, Kind: "pane", Owner: x.Owner, Liveness: "not-proved-dead"}
			if seen[x.ID] {
				e.Liveness = "dead"
				r.Reclaimed = append(r.Reclaimed, e)
			} else {
				r.Kept = append(r.Kept, e)
			}
		}
		return nil
	})
	return r, err
}
