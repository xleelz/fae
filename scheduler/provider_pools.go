package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// DefaultQuotaGrace is the FAE-RT-018 default grace period added to a parsed
// provider usage-limit reset time T before dispatches or probes resume.
const DefaultQuotaGrace = 60 * time.Second

// ProviderPoolQuota records a FAE-RT-018 quota-exhausted-until-T mark on a
// host-scoped provider concurrency pool.
type ProviderPoolQuota struct {
	PoolID         string    `json:"pool_id"`
	ResetAt        time.Time `json:"reset_at"`
	GraceSeconds   int       `json:"grace_seconds"`
	ExhaustedUntil time.Time `json:"exhausted_until"`
	CutOffTaskID   string    `json:"cut_off_task_id,omitempty"`
	Actor          string    `json:"actor,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Active reports whether the pool is still within its quota-exhausted window
// (before T + grace).
func (q ProviderPoolQuota) Active(now time.Time) bool {
	return !q.ExhaustedUntil.IsZero() && now.Before(q.ExhaustedUntil)
}

// ProviderPoolPause records a durable FAE-RT-018 dispatch pause on a
// host-scoped provider pool.
type ProviderPoolPause struct {
	PoolID    string    `json:"pool_id"`
	Paused    bool      `json:"paused"`
	Actor     string    `json:"actor,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ProviderPoolSnapshot bundles a provider pool's configuration, active/waiting
// leases, quota state, and pause state for inspection and scheduling.
type ProviderPoolSnapshot struct {
	Config ProviderPoolConfig `json:"config"`
	Pool   Pool               `json:"pool"`
	Quota  *ProviderPoolQuota `json:"quota,omitempty"`
	Pause  *ProviderPoolPause `json:"pause,omitempty"`
}

// UnboundedProviderPoolCap is the effective admission limit used when a
// provider pool is configured with Cap == -1 (unbounded).
const UnboundedProviderPoolCap = 1000000

// ValidateProviderPoolConfig checks that a provider pool configuration is well-formed.
func ValidateProviderPoolConfig(cfg ProviderPoolConfig) error {
	if strings.TrimSpace(cfg.ID) == "" {
		return errors.New("provider pool id required")
	}
	if strings.TrimSpace(cfg.Match.Provider) == "" {
		return fmt.Errorf("provider pool %s requires match.provider", cfg.ID)
	}
	if cfg.Cap == 0 || cfg.Cap < -1 {
		return fmt.Errorf("provider pool %s cap must be -1 (unbounded) or >= 1", cfg.ID)
	}
	return nil
}

// MatchProviderPool resolves a (provider, credentialIdentity) pair against a
// set of configured provider pools (FAE-SCH-015). An exact credential match
// wins over a wildcard/default pool for the same provider.
func MatchProviderPool(configs map[string]ProviderPoolConfig, provider, credentialIdentity string) (ProviderPoolConfig, bool) {
	provider = strings.TrimSpace(provider)
	credentialIdentity = strings.TrimSpace(credentialIdentity)
	if provider == "" || len(configs) == 0 {
		return ProviderPoolConfig{}, false
	}
	if credentialIdentity == "" {
		credentialIdentity = "default"
	}
	ids := make([]string, 0, len(configs))
	for id := range configs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cfg := configs[id]
		if cfg.Match.Provider == provider && cfg.Match.CredentialIdentity == credentialIdentity {
			return cfg, true
		}
	}
	for _, id := range ids {
		cfg := configs[id]
		cred := strings.TrimSpace(cfg.Match.CredentialIdentity)
		if cfg.Match.Provider == provider && (cred == "" || cred == "*" || (cred == "default" && credentialIdentity == "default")) {
			return cfg, true
		}
	}
	return ProviderPoolConfig{}, false
}

func effectiveProviderPoolCap(cap int) int {
	if cap == -1 {
		return UnboundedProviderPoolCap
	}
	if cap < 1 {
		return 1
	}
	return cap
}

func newProviderPool(cap int) Pool {
	eff := effectiveProviderPoolCap(cap)
	p, _ := NewPool(eff)
	p.AdmissionLimit = eff
	p.ReadySetOrder = true
	return *p
}

func applyProviderPoolCap(p *Pool, cap int) {
	eff := effectiveProviderPoolCap(cap)
	p.ReadySetOrder = true
	if p.TTL <= 0 {
		p.TTL = 6 * time.Hour
	}
	if p.MaxWait <= 0 {
		p.MaxWait = 1800 * time.Second
	}
	if p.WarnAfter < 0 {
		p.WarnAfter = 300 * time.Second
	}
	used := 0
	for _, h := range p.Holders {
		used += h.Weight
	}
	if p.Capacity < eff {
		p.Capacity = eff
	}
	if p.Capacity < used {
		p.Capacity = used
	}
	p.AdmissionLimit = eff
}

func ensureProviderPoolMaps(s *ledgerState) {
	if s.ProviderPools == nil {
		s.ProviderPools = map[string]Pool{}
	}
	if s.ProviderPoolConfigs == nil {
		s.ProviderPoolConfigs = map[string]ProviderPoolConfig{}
	}
	if s.ProviderPoolQuotas == nil {
		s.ProviderPoolQuotas = map[string]ProviderPoolQuota{}
	}
	if s.ProviderPoolPauses == nil {
		s.ProviderPoolPauses = map[string]ProviderPoolPause{}
	}
}

// ConfigureProviderPools installs or updates provider pool configurations in
// the host ledger without evicting active holders (FAE-SCH-015).
func (l *Ledger) ConfigureProviderPools(ctx context.Context, configs []ProviderPoolConfig) error {
	for _, cfg := range configs {
		if err := ValidateProviderPoolConfig(cfg); err != nil {
			return err
		}
	}
	return l.transact(ctx, true, func(s *ledgerState) error {
		ensureProviderPoolMaps(s)
		for _, cfg := range configs {
			cfg.ID = strings.TrimSpace(cfg.ID)
			cfg.Match.Provider = strings.TrimSpace(cfg.Match.Provider)
			cfg.Match.CredentialIdentity = strings.TrimSpace(cfg.Match.CredentialIdentity)
			s.ProviderPoolConfigs[cfg.ID] = cfg
			pp, exists := s.ProviderPools[cfg.ID]
			if !exists {
				pp = newProviderPool(cfg.Cap)
			} else {
				applyProviderPoolCap(&pp, cfg.Cap)
			}
			s.ProviderPools[cfg.ID] = pp
		}
		return nil
	})
}

// ResizeProviderPool updates one provider pool's cap (and optional match rule)
// in the host ledger without evicting active holders (FAE-SCH-015).
func (l *Ledger) ResizeProviderPool(ctx context.Context, cfg ProviderPoolConfig, reason string) (ProviderPoolSnapshot, error) {
	cfg.ID = strings.TrimSpace(cfg.ID)
	cfg.Match.Provider = strings.TrimSpace(cfg.Match.Provider)
	cfg.Match.CredentialIdentity = strings.TrimSpace(cfg.Match.CredentialIdentity)
	if cfg.ID == "" || cfg.Cap == 0 || cfg.Cap < -1 {
		return ProviderPoolSnapshot{}, errors.New("provider pool id and cap (-1 or >= 1) required")
	}
	var out ProviderPoolSnapshot
	err := l.transact(ctx, true, func(s *ledgerState) error {
		ensureProviderPoolMaps(s)
		existingCfg, hadCfg := s.ProviderPoolConfigs[cfg.ID]
		if cfg.Match.Provider == "" {
			if hadCfg && existingCfg.Match.Provider != "" {
				cfg.Match = existingCfg.Match
			} else {
				cfg.Match.Provider = cfg.ID
			}
		}
		s.ProviderPoolConfigs[cfg.ID] = cfg
		pp, exists := s.ProviderPools[cfg.ID]
		if !exists {
			pp = newProviderPool(cfg.Cap)
		} else {
			applyProviderPoolCap(&pp, cfg.Cap)
		}
		now := time.Now().UTC()
		_, _, _ = pp.Advance(now, func(o Owner) bool { return ProbeOwner(o) != Dead })
		s.ProviderPools[cfg.ID] = pp
		owner, _ := CurrentOwner()
		if reason == "" {
			reason = fmt.Sprintf("resize provider pool %s cap=%d", cfg.ID, cfg.Cap)
		}
		s.Audit = append(s.Audit, LeaseAudit{At: now, Action: "provider-pool-resize", ID: cfg.ID, Owner: owner, Reason: reason})
		out = snapshotForPool(s, cfg.ID)
		return nil
	})
	return out, err
}

func snapshotForPool(s *ledgerState, poolID string) ProviderPoolSnapshot {
	snap := ProviderPoolSnapshot{
		Config: s.ProviderPoolConfigs[poolID],
		Pool:   s.ProviderPools[poolID],
	}
	if q, ok := s.ProviderPoolQuotas[poolID]; ok && !q.ExhaustedUntil.IsZero() {
		qCopy := q
		snap.Quota = &qCopy
	}
	if p, ok := s.ProviderPoolPauses[poolID]; ok && p.Paused {
		pCopy := p
		snap.Pause = &pCopy
	}
	return snap
}

// ProviderPoolsSnapshot reads all configured provider pools and their current
// holders, waiters, quota marks, and pause flags from the host ledger.
func (l *Ledger) ProviderPoolsSnapshot(ctx context.Context) (map[string]ProviderPoolSnapshot, error) {
	out := map[string]ProviderPoolSnapshot{}
	err := l.transact(ctx, false, func(s *ledgerState) error {
		for poolID := range s.ProviderPoolConfigs {
			out[poolID] = snapshotForPool(s, poolID)
		}
		for poolID := range s.ProviderPools {
			if _, ok := out[poolID]; !ok {
				out[poolID] = snapshotForPool(s, poolID)
			}
		}
		return nil
	})
	return out, err
}

// MatchProviderPool resolves a (provider, credentialIdentity) pair against the
// host ledger's configured provider pools.
func (l *Ledger) MatchProviderPool(ctx context.Context, provider, credentialIdentity string) (ProviderPoolSnapshot, bool, error) {
	var snap ProviderPoolSnapshot
	var matched bool
	err := l.transact(ctx, false, func(s *ledgerState) error {
		cfg, ok := MatchProviderPool(s.ProviderPoolConfigs, provider, credentialIdentity)
		if !ok {
			return nil
		}
		matched = true
		snap = snapshotForPool(s, cfg.ID)
		return nil
	})
	return snap, matched, err
}

// EnqueueProviderPool queues a request in the named provider pool.
func (l *Ledger) EnqueueProviderPool(ctx context.Context, poolID string, r Request) error {
	return l.transact(ctx, true, func(s *ledgerState) error {
		ensureProviderPoolMaps(s)
		pp, ok := s.ProviderPools[poolID]
		if !ok {
			cfg, hasCfg := s.ProviderPoolConfigs[poolID]
			if !hasCfg {
				return fmt.Errorf("unknown provider pool %s", poolID)
			}
			pp = newProviderPool(cfg.Cap)
		}
		pp.ReadySetOrder = true
		for _, w := range pp.Waiting {
			if w.ID == r.ID {
				return nil
			}
		}
		for _, h := range pp.Holders {
			if h.ID == r.ID {
				return nil
			}
		}
		if err := pp.Enqueue(r); err != nil {
			return err
		}
		s.ProviderPools[poolID] = pp
		s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "provider-pool-queued", ID: r.ID, Owner: r.Owner, Reason: poolID})
		return nil
	})
}

// AdvanceProviderPool reconciles dead owners and grants ready-set-ordered
// waiting requests on the named provider pool up to its effective cap.
func (l *Ledger) AdvanceProviderPool(ctx context.Context, poolID string, now time.Time, observed map[Owner]Liveness) ([]Lease, []Event, error) {
	var granted []Lease
	var events []Event
	err := l.transact(ctx, true, func(s *ledgerState) error {
		ensureProviderPoolMaps(s)
		pp, ok := s.ProviderPools[poolID]
		if !ok {
			return fmt.Errorf("unknown provider pool %s", poolID)
		}
		pp.ReadySetOrder = true
		var e error
		granted, events, e = pp.AdvanceObserved(now, observed)
		if e != nil {
			return e
		}
		s.ProviderPools[poolID] = pp
		for _, g := range granted {
			s.Audit = append(s.Audit, LeaseAudit{At: now, Action: "provider-pool-granted", ID: g.ID, Owner: g.Owner, Reason: poolID})
		}
		return nil
	})
	return granted, events, err
}

// TryAcquireProviderPool enqueues r into poolID, allows a brief coalescing
// window so concurrent ready-set admissions in the same round are ordered by
// FAE-SCH-003 (CutOffByQuota first, Priority desc, Admitted asc, TaskID asc),
// advances the pool, and either returns the granted lease or removes r from
// Waiting and returns (Lease{}, false, nil) when the pool is at capacity.
func (l *Ledger) TryAcquireProviderPool(ctx context.Context, poolID string, r Request, coalesce time.Duration) (Lease, bool, error) {
	if r.Weight <= 0 {
		r.Weight = 1
	}
	if r.Queued.IsZero() {
		r.Queued = time.Now().UTC()
	}
	if err := l.EnqueueProviderPool(ctx, poolID, r); err != nil {
		return Lease{}, false, err
	}
	if coalesce > 0 {
		timer := time.NewTimer(coalesce)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = l.CancelProviderPool(context.Background(), poolID, r.ID, r.Owner, "context cancelled before provider pool grant")
			return Lease{}, false, ctx.Err()
		case <-timer.C:
		}
	}
	var granted Lease
	var ok bool
	err := l.transact(ctx, true, func(s *ledgerState) error {
		ensureProviderPoolMaps(s)
		pp, exists := s.ProviderPools[poolID]
		if !exists {
			return fmt.Errorf("unknown provider pool %s", poolID)
		}
		pp.ReadySetOrder = true
		observed := map[Owner]Liveness{r.Owner: Alive}
		for _, h := range pp.Holders {
			if _, seen := observed[h.Owner]; !seen {
				observed[h.Owner] = ProbeOwner(h.Owner)
			}
		}
		for _, w := range pp.Waiting {
			if _, seen := observed[w.Owner]; !seen {
				observed[w.Owner] = ProbeOwner(w.Owner)
			}
		}
		now := time.Now().UTC()
		newlyGranted, _, e := pp.AdvanceObserved(now, observed)
		if e != nil {
			return e
		}
		for _, g := range newlyGranted {
			s.Audit = append(s.Audit, LeaseAudit{At: now, Action: "provider-pool-granted", ID: g.ID, Owner: g.Owner, Reason: poolID})
		}
		for _, h := range pp.Holders {
			if h.ID == r.ID {
				granted = h
				ok = true
				s.ProviderPools[poolID] = pp
				return nil
			}
		}
		// Not granted (pool is at capacity or higher-priority waiters took the slots):
		// remove from Waiting so withheld sessions do not hold phantom waiter entries.
		for i, w := range pp.Waiting {
			if w.ID == r.ID {
				pp.Waiting = append(pp.Waiting[:i], pp.Waiting[i+1:]...)
				break
			}
		}
		s.ProviderPools[poolID] = pp
		return nil
	})
	return granted, ok, err
}

// ReleaseProviderPool releases a held lease on the named provider pool.
func (l *Ledger) ReleaseProviderPool(ctx context.Context, poolID, id string, owner Owner, reason string) error {
	return l.transact(ctx, true, func(s *ledgerState) error {
		if reason == "" {
			return errors.New("release reason required")
		}
		ensureProviderPoolMaps(s)
		pp, ok := s.ProviderPools[poolID]
		if !ok {
			return fmt.Errorf("unknown provider pool %s", poolID)
		}
		if !pp.Release(id, owner) {
			return errors.New("provider pool lease not held by owner")
		}
		s.ProviderPools[poolID] = pp
		s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "provider-pool-released", ID: id, Owner: owner, Reason: poolID + ": " + reason})
		return nil
	})
}

// CancelProviderPool removes a waiting request from the named provider pool.
func (l *Ledger) CancelProviderPool(ctx context.Context, poolID, id string, owner Owner, reason string) error {
	return l.transact(ctx, true, func(s *ledgerState) error {
		if reason == "" {
			return errors.New("cancellation reason required")
		}
		ensureProviderPoolMaps(s)
		pp, ok := s.ProviderPools[poolID]
		if !ok {
			return nil
		}
		for i, w := range pp.Waiting {
			if w.ID == id && w.Owner == owner {
				pp.Waiting = append(pp.Waiting[:i], pp.Waiting[i+1:]...)
				s.ProviderPools[poolID] = pp
				s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "provider-pool-cancelled", ID: id, Owner: owner, Reason: poolID + ": " + reason})
				return nil
			}
		}
		return nil
	})
}

// SetProviderPoolQuota marks a provider pool quota-exhausted until T + grace (FAE-RT-018).
func (l *Ledger) SetProviderPoolQuota(ctx context.Context, q ProviderPoolQuota) error {
	q.PoolID = strings.TrimSpace(q.PoolID)
	if q.PoolID == "" || q.ResetAt.IsZero() {
		return errors.New("provider pool id and reset time required")
	}
	if q.GraceSeconds < 0 {
		q.GraceSeconds = 0
	}
	if q.ExhaustedUntil.IsZero() {
		q.ExhaustedUntil = q.ResetAt.Add(time.Duration(q.GraceSeconds) * time.Second)
	}
	if q.UpdatedAt.IsZero() {
		q.UpdatedAt = time.Now().UTC()
	}
	return l.transact(ctx, true, func(s *ledgerState) error {
		ensureProviderPoolMaps(s)
		if existing, ok := s.ProviderPoolQuotas[q.PoolID]; ok && q.CutOffTaskID == "" && existing.CutOffTaskID != "" {
			q.CutOffTaskID = existing.CutOffTaskID
		}
		s.ProviderPoolQuotas[q.PoolID] = q
		owner, _ := CurrentOwner()
		s.Audit = append(s.Audit, LeaseAudit{At: q.UpdatedAt, Action: "provider-pool-quota-set", ID: q.PoolID, Owner: owner, Reason: q.Reason})
		return nil
	})
}

// ClearProviderPoolQuota clears a provider pool's quota-exhausted mark (FAE-RT-018).
func (l *Ledger) ClearProviderPoolQuota(ctx context.Context, poolID, actor, reason string) error {
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		return errors.New("provider pool id required")
	}
	return l.transact(ctx, true, func(s *ledgerState) error {
		ensureProviderPoolMaps(s)
		delete(s.ProviderPoolQuotas, poolID)
		owner, _ := CurrentOwner()
		s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "provider-pool-quota-clear", ID: poolID, Owner: owner, Reason: reason})
		return nil
	})
}

// SetProviderPoolPause sets or clears the durable pause flag on a provider pool (FAE-RT-018).
func (l *Ledger) SetProviderPoolPause(ctx context.Context, p ProviderPoolPause) error {
	p.PoolID = strings.TrimSpace(p.PoolID)
	if p.PoolID == "" {
		return errors.New("provider pool id required")
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = time.Now().UTC()
	}
	return l.transact(ctx, true, func(s *ledgerState) error {
		ensureProviderPoolMaps(s)
		if p.Paused {
			s.ProviderPoolPauses[p.PoolID] = p
		} else {
			delete(s.ProviderPoolPauses, p.PoolID)
		}
		owner, _ := CurrentOwner()
		action := "provider-pool-paused"
		if !p.Paused {
			action = "provider-pool-unpaused"
		}
		s.Audit = append(s.Audit, LeaseAudit{At: p.UpdatedAt, Action: action, ID: p.PoolID, Owner: owner, Reason: p.Reason})
		return nil
	})
}
