package controlplane

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xleelz/fae/scheduler"
)

var errAlreadyGone = errors.New("lease entry already released")

// LeasePoolKeys lists every durable role-slot pool on the plane.
func (s *Store) LeasePoolKeys(ctx context.Context) ([]string, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT pool_key FROM resource_pools WHERE kind <> 'provider_pool' ORDER BY pool_key LIMIT 10001`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	keys := []string{}
	for rows.Next() {
		var k string
		if e = rows.Scan(&k); e != nil {
			return nil, e
		}
		keys = append(keys, k)
	}
	if len(keys) > 10000 {
		return nil, errors.New("lease pool listing exceeds 10000; refusing incomplete reconcile")
	}
	return keys, rows.Err()
}

// ReclaimLeasePool reconciles one role-slot pool: entries whose owner is proved
// dead are removed, each in its own audited mutation; alive and ambiguous owners
// are kept. A dry run reads the pool only.
func (s *Store) ReclaimLeasePool(ctx context.Context, a Audit, key string, probe func(scheduler.Owner) scheduler.Liveness, dryRun bool) (scheduler.ReclaimReport, error) {
	p, e := s.LeaseSnapshot(ctx, key)
	if e != nil {
		return scheduler.ReclaimReport{}, e
	}
	report := p.ReclaimDeadOwners(probe)
	for _, list := range [][]scheduler.ReclaimEntry{report.Reclaimed, report.Kept} {
		for i := range list {
			list[i].Pool = key
		}
	}
	if dryRun {
		return report, nil
	}
	for _, entry := range report.Reclaimed {
		row := a
		row.Outcome = "reclaimed role-slots " + key + "/" + entry.ID
		row.Inputs, _ = json.Marshal(entry)
		e = s.mutatePool(ctx, row, key, "reclaimed: dead owner", nil, func(p *scheduler.Pool) ([]Event, error) {
			for i, h := range p.Holders {
				if h.ID == entry.ID && h.Owner == entry.Owner {
					p.Holders = append(p.Holders[:i], p.Holders[i+1:]...)
					return []Event{leaseEvent("lease_reclaimed", key, entry)}, nil
				}
			}
			for i, w := range p.Waiting {
				if w.ID == entry.ID && w.Owner == entry.Owner {
					p.Waiting = append(p.Waiting[:i], p.Waiting[i+1:]...)
					return []Event{leaseEvent("lease_reclaimed", key, entry)}, nil
				}
			}
			return nil, errAlreadyGone
		})
		if errors.Is(e, errAlreadyGone) {
			continue
		}
		if e != nil {
			return report, e
		}
	}
	return report, nil
}
