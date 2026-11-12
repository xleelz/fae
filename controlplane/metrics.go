package controlplane

import (
	"context"
	"errors"
	"time"

	"github.com/xleelz/fae/scheduler"
)

// EventsSince returns, oldest first, the events committed at or after since
// among the newest limit+1 by sequence (a primary-key scan; the archive is
// never compacted), and whether older events in range were left out.
func (s *Store) EventsSince(ctx context.Context, since time.Time, limit int) ([]Event, bool, error) {
	if limit < 1 || limit > 100000 {
		return nil, false, errors.New("invalid event range")
	}
	rows, e := s.db.QueryContext(ctx, `SELECT sequence,kind,job_id,payload,created_at FROM events ORDER BY sequence DESC LIMIT ?`, limit+1)
	if e != nil {
		return nil, false, e
	}
	defer rows.Close()
	events := []Event{}
	for rows.Next() {
		var ev Event
		if e = rows.Scan(&ev.Sequence, &ev.Kind, &ev.JobID, &ev.Payload, &ev.Timestamp); e != nil {
			return nil, false, e
		}
		ev.Timestamp = ev.Timestamp.UTC()
		if ev.Timestamp.Before(since) {
			break
		}
		events = append(events, ev)
	}
	if e = rows.Err(); e != nil {
		return nil, false, e
	}
	truncated := len(events) > limit
	if truncated {
		events = events[:limit]
	}
	for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
		events[i], events[j] = events[j], events[i]
	}
	if err:=rows.Close();err!=nil{return nil,false,err}
	events,err:=s.restoreEventMetadata(ctx,events)
	return events,truncated,err
}

// LeasePool is one durable resource pool (role slots and similar).
type LeasePool struct {
	Key  string         `json:"key"`
	Kind string         `json:"kind"`
	Pool scheduler.Pool `json:"pool"`
}

func (s *Store) LeasePools(ctx context.Context) ([]LeasePool, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT pool_key,kind,payload FROM resource_pools ORDER BY pool_key LIMIT 10000`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out, stored := []LeasePool{}, []string{}
	for rows.Next() {
		var p LeasePool
		var raw string
		if e = rows.Scan(&p.Key, &p.Kind, &raw); e != nil {
			return nil, e
		}
		out, stored = append(out, p), append(stored, raw)
	}
	if e = errors.Join(rows.Err(), rows.Close()); e != nil {
		return nil, e
	}
	for i := range out {
		if out[i].Pool, e = decodePool(ctx, s.db, stored[i]); e != nil {
			return nil, e
		}
	}
	return out, nil
}

// ProviderObservationRecorded reports whether a session's observation is
// already on the plane, so a replay neither writes nor logs a second time.
func (s *Store) ProviderObservationRecorded(ctx context.Context, id string) (bool, error) {
	var n int
	e := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM provider_observations WHERE observation_id=?`, id).Scan(&n)
	return n > 0, e
}
