package scheduler

import (
	"context"
	"errors"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
	"time"
)

type HostLimitRecord struct {
	Capacity   int
	Thresholds hostguard.Config
	Owner      Owner
	Birth      string
}

func OpenDynamicLedger(path string, capacity int) (*Ledger, error) {
	return openLedger(path, capacity, true)
}
func reconcileHost(s *ledgerState) (int, hostguard.Config, error) {
	limit := 0
	var cfg hostguard.Config
	for id, r := range s.HostLimits {
		if id == "" || r.Capacity < 1 || r.Capacity > 1000000 || r.Owner.PID <= 1 || r.Owner.Started <= 0 || r.Birth == "" || !hostguard.ValidConfig(r.Thresholds) {
			return 0, cfg, errors.New("invalid durable host limits")
		}
		if processowner.ProvedDead(r.Owner.PID, r.Birth) {
			delete(s.HostLimits, id)
			continue
		}
		if limit == 0 {
			limit = r.Capacity
			cfg = r.Thresholds
		} else {
			if r.Capacity < limit {
				limit = r.Capacity
			}
			cfg = hostguard.Conservative(cfg, r.Thresholds)
		}
	}
	if limit == 0 {
		limit = s.Pool.Capacity
		cfg = hostguard.DefaultConfig()
	}
	s.Pool.AdmissionLimit = limit
	if limit > s.Pool.Capacity {
		s.Pool.Capacity = limit
	}
	return limit, cfg, nil
}
func (l *Ledger) ConfigureHost(ctx context.Context, instance string, capacity int, cfg hostguard.Config) error {
	if instance == "" || capacity < 1 || capacity > 1000000 || !hostguard.ValidConfig(cfg) {
		return errors.New("invalid host limits")
	}
	return l.transact(ctx, true, func(s *ledgerState) error {
		if _, _, e := reconcileHost(s); e != nil {
			return e
		}
		owner, e := CurrentOwner()
		if e != nil {
			return e
		}
		birth, e := processowner.Birth(owner.PID)
		if e != nil {
			return e
		}
		if s.HostLimits == nil {
			s.HostLimits = map[string]HostLimitRecord{}
		}
		r := HostLimitRecord{capacity, cfg, owner, birth}
		if old, ok := s.HostLimits[instance]; ok {
			if old == r {
				return nil
			}
			if (old.Owner != owner || old.Birth != birth) && !processowner.ProvedDead(old.Owner.PID, old.Birth) {
				return errors.New("host limits belong to another live owner")
			}
		}
		s.HostLimits[instance] = r
		if _, _, e = reconcileHost(s); e != nil {
			return e
		}
		s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "host-limits-configured", ID: instance, Owner: owner})
		return nil
	})
}
func (l *Ledger) HostLimits(ctx context.Context) (int, hostguard.Config, error) {
	var n int
	var c hostguard.Config
	e := l.transact(ctx, true, func(s *ledgerState) error { var err error; n, c, err = reconcileHost(s); return err })
	return n, c, e
}
func (l *Ledger) UnconfigureHost(ctx context.Context, instance string, owner Owner, birth string) error {
	return l.transact(ctx, true, func(s *ledgerState) error {
		if _, _, e := reconcileHost(s); e != nil {
			return e
		}
		if r, ok := s.HostLimits[instance]; ok {
			if r.Owner != owner || r.Birth != birth {
				return errors.New("host configuration owner mismatch")
			}
			delete(s.HostLimits, instance)
			s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "host-limits-unconfigured", ID: instance, Owner: owner})
		}
		_, _, e := reconcileHost(s)
		return e
	})
}
