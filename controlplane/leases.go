package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/scheduler"
	"time"
)

type LeasePoolConfig struct {
	Key, Kind               string
	Capacity                int
	TTL, WarnAfter, MaxWait time.Duration
}
type LeaseUpdate struct {
	Granted []scheduler.Lease
	Events  []scheduler.Event
	Pool    scheduler.Pool
}

func (c LeasePoolConfig) pool() (scheduler.Pool, error) {
	p, e := scheduler.NewPool(c.Capacity)
	if e != nil {
		return scheduler.Pool{}, e
	}
	if c.Key == "" || len(c.Key) > 255 || c.Kind == "" || len(c.Kind) > 64 {
		return *p, errors.New("pool key and kind required")
	}
	if c.TTL != 0 {
		p.TTL = c.TTL
	}
	if c.WarnAfter != 0 {
		p.WarnAfter = c.WarnAfter
	}
	if c.MaxWait != 0 {
		p.MaxWait = c.MaxWait
	}
	if p.TTL <= 0 || p.WarnAfter < 0 || p.MaxWait <= 0 {
		return *p, errors.New("invalid pool durations")
	}
	return *p, nil
}

// EnsureLeasePool creates a pool or applies a changed capacity to it. A
// changed capacity governs future grants only (FAE-SCH-006): current holders
// keep their slots and drain, and the validation ceiling never shrinks below
// them. A differing kind or duration still conflicts.
func (s *Store) EnsureLeasePool(ctx context.Context, a Audit, c LeasePoolConfig) error {
	p, e := c.pool()
	if e != nil {
		return e
	}
	b, _ := json.Marshal(p)
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if _, e := tx.ExecContext(ctx, `INSERT IGNORE INTO resource_pools (pool_key,kind,version,payload) VALUES (?,?,1,?)`, c.Key, c.Kind, string(b)); e != nil {
			return e
		}
		var kind, raw string
		var version int64
		if e := tx.QueryRowContext(ctx, `SELECT kind,version,payload FROM resource_pools WHERE pool_key=?`, c.Key).Scan(&kind, &version, &raw); e != nil {
			return e
		}
		actual, e := decodePool(ctx, tx, raw)
		if e != nil {
			return e
		}
		if kind != c.Kind || actual.TTL != p.TTL || actual.WarnAfter != p.WarnAfter || actual.MaxWait != p.MaxWait {
			return errors.New("pool configuration conflict")
		}
		limit := actual.Capacity
		if actual.AdmissionLimit > 0 {
			limit = actual.AdmissionLimit
		}
		if limit == p.Capacity {
			return nil
		}
		if e := actual.Validate(); e != nil {
			return e
		}
		actual.Capacity = max(actual.Capacity, p.Capacity)
		actual.AdmissionLimit = 0
		if p.Capacity < actual.Capacity {
			actual.AdmissionLimit = p.Capacity
		}
		updated, e := s.encodePool(ctx, tx, actual)
		if e != nil {
			return e
		}
		result, e := tx.ExecContext(ctx, `UPDATE resource_pools SET version=version+1,payload=? WHERE pool_key=? AND version=?`, updated, c.Key, version)
		if e != nil {
			return e
		}
		if n, e := result.RowsAffected(); e != nil || n != 1 {
			return errors.Join(e, ErrConflict)
		}
		return s.appendEvents(ctx, tx, "", []Event{leaseEvent("lease_pool_capacity", c.Key, map[string]int{"from": limit, "to": p.Capacity})}, "")
	})
}
func (s *Store) LeaseSnapshot(ctx context.Context, key string) (scheduler.Pool, error) {
	var raw string
	var p scheduler.Pool
	e := s.db.QueryRowContext(ctx, `SELECT payload FROM resource_pools WHERE pool_key=?`, key).Scan(&raw)
	if e == nil {
		p, e = decodePool(ctx, s.db, raw)
	}
	if e == nil {
		e = p.Validate()
	}
	return p, e
}

// mutatePool is the lease ledger's version CAS. The leases table projects its
// holders in the same transaction: a released holder is closed with its
// reason from reasons (by request ID, filled by fn) or fallback.
func (s *Store) mutatePool(ctx context.Context, a Audit, key, fallback string, reasons map[string]string, fn func(*scheduler.Pool) ([]Event, error)) error {
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		var raw, kind string
		var version int64
		if e := tx.QueryRowContext(ctx, `SELECT version,payload,kind FROM resource_pools WHERE pool_key=?`, key).Scan(&version, &raw, &kind); e != nil {
			return e
		}
		p, e := decodePool(ctx, tx, raw)
		if e != nil {
			return fmt.Errorf("unreadable lease ledger: %w", e)
		}
		if e := p.Validate(); e != nil {
			return e
		}
		before := append([]scheduler.Lease(nil), p.Holders...)
		events, e := fn(&p)
		if e != nil {
			return e
		}
		text, e := s.encodePool(ctx, tx, p)
		if e != nil {
			return e
		}
		result, e := tx.ExecContext(ctx, `UPDATE resource_pools SET version=version+1,payload=? WHERE pool_key=? AND version=?`, text, key, version)
		if e != nil {
			return e
		}
		n, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return ErrConflict
		}
		if e = projectLeasesTx(ctx, tx, key, kind, before, p.Holders, reasons, fallback, time.Now().UTC()); e != nil {
			return e
		}
		if len(events) > 0 {
			return s.appendEvents(ctx, tx, "", events, "")
		}
		return nil
	})
}

// encodePool stores a lease ledger, by reference once its holders and
// waiters outgrow the inline limit, so its row stays bounded (FAE-CP-012).
func (s *Store) encodePool(ctx context.Context, tx *sql.Tx, p scheduler.Pool) (string, error) {
	b, e := json.Marshal(p)
	if e != nil {
		return "", e
	}
	return s.inlineOrRef(ctx, tx, b)
}

// decodePool reads a lease ledger column, following a blob reference.
func decodePool(ctx context.Context, q queryer, text string) (scheduler.Pool, error) {
	var p scheduler.Pool
	raw := []byte(text)
	if ref, ok := blobRef(text); ok {
		var e error
		if raw, e = getBlob(ctx, q, ref); e != nil {
			return p, e
		}
	}
	return p, json.Unmarshal(raw, &p)
}
func leaseEvent(kind, key string, value any) Event {
	b, _ := json.Marshal(map[string]any{"pool": key, "value": value})
	return Event{Kind: kind, Payload: b}
}
func (s *Store) EnqueueLease(ctx context.Context, a Audit, key string, r scheduler.Request) error {
	return s.mutatePool(ctx, a, key, "", nil, func(p *scheduler.Pool) ([]Event, error) {
		// Identical replay is safe after an uncertain SQL commit.
		for _, w := range p.Waiting {
			if w.ID == r.ID {
				if w.Owner == r.Owner && w.Weight == r.Weight && w.Class == r.Class && w.Priority == r.Priority && w.Queued.Equal(r.Queued) {
					return nil, nil
				}
				return nil, ErrIdempotencyConflict
			}
		}
		for _, h := range p.Holders {
			if h.ID == r.ID {
				if h.Request.ID == r.ID && h.Owner == r.Owner && h.Weight == r.Weight && h.Class == r.Class && h.Priority == r.Priority && h.Queued.Equal(r.Queued) {
					return nil, nil
				}
				return nil, ErrIdempotencyConflict
			}
		}
		if e := p.Enqueue(r); e != nil {
			return nil, e
		}
		return []Event{leaseEvent("lease_queued", key, r)}, nil
	})
}
func (s *Store) AdvanceLeases(ctx context.Context, a Audit, key string, now time.Time, observed map[scheduler.Owner]scheduler.Liveness) (LeaseUpdate, error) {
	var out LeaseUpdate
	reasons := map[string]string{}
	e := s.mutatePool(ctx, a, key, "advanced", reasons, func(p *scheduler.Pool) ([]Event, error) {
		granted, events, e := p.AdvanceObserved(now, observed)
		if e != nil {
			return nil, e
		}
		for _, ev := range events {
			reasons[ev.RequestID] = ev.Kind
		}
		out = LeaseUpdate{granted, events, *p}
		var records []Event
		for _, g := range granted {
			records = append(records, leaseEvent("lease_granted", key, g))
		}
		for _, ev := range events {
			records = append(records, leaseEvent("lease_"+ev.Kind, key, ev))
		}
		return records, nil
	})
	if e != nil {
		return LeaseUpdate{}, e
	}
	return out, nil
}
func (s *Store) ReleaseLease(ctx context.Context, a Audit, key, id string, owner scheduler.Owner, reason string) error {
	if reason == "" {
		return errors.New("release reason required")
	}
	return s.mutatePool(ctx, a, key, reason, nil, func(p *scheduler.Pool) ([]Event, error) {
		if !p.Release(id, owner) {
			return nil, errors.New("lease not held by owner")
		}
		return []Event{leaseEvent("lease_released", key, map[string]any{"id": id, "owner": owner, "reason": reason})}, nil
	})
}

func (s *Store) CancelLease(ctx context.Context, a Audit, key, id string, owner scheduler.Owner, reason string) error {
	if reason == "" {
		return errors.New("cancellation reason required")
	}
	return s.mutatePool(ctx, a, key, reason, nil, func(p *scheduler.Pool) ([]Event, error) {
		for i, w := range p.Waiting {
			if w.ID == id && w.Owner == owner {
				p.Waiting = append(p.Waiting[:i], p.Waiting[i+1:]...)
				return []Event{leaseEvent("lease_cancelled", key, map[string]any{"id": id, "owner": owner, "reason": reason})}, nil
			}
		}
		return nil, errors.New("request not waiting for owner")
	})
}

func (s *Store) RecoverLease(ctx context.Context, a Audit, key string, proof scheduler.OperationProof, ownerDead bool) error {
	return s.mutatePool(ctx, a, key, "recovered", nil, func(p *scheduler.Pool) ([]Event, error) {
		changed, e := p.RecoverProof(proof, ownerDead)
		if e != nil {
			return nil, e
		}
		if !changed {
			return nil, nil
		}
		return []Event{leaseEvent("lease_recovered", key, proof)}, nil
	})
}
