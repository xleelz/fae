package controlplane

import (
	"context"
	"errors"
	"time"
)

type PlaneCommit struct {
	Hash string    `json:"hash"`
	Time time.Time `json:"time"`
}

// PlaneHead reports the plane's latest Dolt commit for status.
func (s *Store) PlaneHead(ctx context.Context) (PlaneCommit, error) {
	var c PlaneCommit
	e := s.db.QueryRowContext(ctx, `SELECT commit_hash,date FROM dolt_log LIMIT 1`).Scan(&c.Hash, &c.Time)
	c.Time = c.Time.UTC()
	return c, e
}

type LeaseUsage struct {
	Pools    int `json:"pools"`
	Capacity int `json:"capacity"`
	Holders  int `json:"holders"`
	Waiters  int `json:"waiters"`
}

// LeaseUsageByKind summarizes durable lease pools (role slots and the like) by kind.
func (s *Store) LeaseUsageByKind(ctx context.Context) (map[string]LeaseUsage, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT kind,payload FROM resource_pools ORDER BY pool_key LIMIT 10001`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var stored [][2]string
	for rows.Next() {
		var kind, raw string
		if e = rows.Scan(&kind, &raw); e != nil {
			return nil, e
		}
		if len(stored) == 10000 {
			return nil, errors.New("lease pool listing bound exceeded")
		}
		stored = append(stored, [2]string{kind, raw})
	}
	if e = errors.Join(rows.Err(), rows.Close()); e != nil {
		return nil, e
	}
	out := map[string]LeaseUsage{}
	for _, row := range stored {
		kind := row[0]
		p, e := decodePool(ctx, s.db, row[1])
		if e != nil {
			return nil, e
		}
		u := out[kind]
		u.Pools++
		cap := p.Capacity
		if p.AdmissionLimit > 0 {
			cap = p.AdmissionLimit
		}
		u.Capacity += cap
		u.Holders += len(p.Holders)
		u.Waiters += len(p.Waiting)
		out[kind] = u
	}
	return out, nil
}
