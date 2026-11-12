package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
)

// AdmissionRefusal identifies a provable no-launch infrastructure deferral.
type AdmissionRefusal struct{ Reason string }

func (e *AdmissionRefusal) Error() string { return "host admission: " + e.Reason }

type PaneReservation struct {
	ID, Socket, Birth string
	Owner             Owner
	Reserved          time.Time
	SeenAt            time.Time
}
type PaneLedger struct {
	ledger *Ledger
	probe  func(PaneReservation) bool
}

func OpenPaneLedger(path string, limit int) (*PaneLedger, error) {
	l, e := openLedger(path, limit, true)
	if e != nil {
		return nil, e
	}
	return &PaneLedger{ledger: l, probe: func(p PaneReservation) bool { return processowner.ProvedDead(p.Owner.PID, p.Birth) }}, nil
}

// Reserve holds the shared lock across inventory, memory sampling and durable
// reservation. A live pane replaces its pending pane count, while the memory
// estimate stays reserved for the first 180 seconds (and throughout startup).
func (p *PaneLedger) Reserve(ctx context.Context, r PaneReservation, inventory func(context.Context) (map[string]int, error), observe func(context.Context) (hostguard.Sample, error), config hostguard.Config, now time.Time) error {
	if r.ID == "" || r.Birth == "" || r.Owner.PID <= 0 || r.Owner.Started <= 0 || !filepath.IsAbs(r.Socket) || r.Reserved.IsZero() {
		return errors.New("invalid pane reservation")
	}
	var refusal error
	err := p.ledger.transact(ctx, true, func(s *ledgerState) error {
		config.PaneLimit = paneLimit(s)
		if config.PaneLimit < 2 {
			return errors.New("invalid shared pane limit")
		}
		if e := p.reconcile(s, now); e != nil {
			return e
		}
		sample, e := observe(ctx)
		if e != nil {
			return e
		}
		// Hard host limits precede pane and memory reservation decisions.
		hard := hostguard.Admit(sample, config, 0, true)
		if hard.Hard {
			refusal = &AdmissionRefusal{hard.Reason}
			return nil
		}
		counts, e := inventory(ctx)
		if e != nil {
			return e
		}
		live, pending, memory := 0, 0, 0
		for _, n := range counts {
			if n < 0 || n > 1_000_000 {
				return errors.New("invalid live pane count")
			}
			if live > 1_000_000-n {
				return errors.New("Herdr inventory exceeds pane bound")
			}
			live += n
		}
		for i, entry := range s.Panes {
			if entry.ID == r.ID {
				return errors.New("duplicate pane reservation")
			}
			if info, err := os.Lstat(entry.Socket); err == nil && info.Mode()&os.ModeSocket != 0 {
				if _, found := counts[entry.Socket]; !found {
					return errors.New("known Herdr socket missing from live census")
				}
			} else if err != nil && !os.IsNotExist(err) {
				return err
			}
			present := counts[entry.Socket] > 0
			if !present {
				pending++
			}
			if present && entry.SeenAt.IsZero() {
				entry.SeenAt = now
				s.Panes[i].SeenAt = now
			}
			if !present || now.Sub(entry.SeenAt) < 180*time.Second {
				memory++
			}
		}
		if !CanReservePane(config.PaneLimit, live, pending) {
			refusal = &AdmissionRefusal{"PaneLimit"}
			return nil
		}
		sample.LivePanes = live
		decision := hostguard.Admit(sample, config, memory, true)
		if !decision.Allowed {
			refusal = &AdmissionRefusal{decision.Reason}
			return nil
		}
		s.Panes = append(s.Panes, r)
		s.Audit = append(s.Audit, LeaseAudit{At: now, Action: "pane-reserved", ID: r.ID, Owner: r.Owner, Reason: fmt.Sprintf("live=%d pending=%d memory=%d", live, pending, memory)})
		return nil
	})
	if err != nil {
		return err
	}
	return refusal
}
func (p *PaneLedger) reconcile(s *ledgerState, now time.Time) error {
	kept := make([]PaneReservation, 0, len(s.Panes))
	ids := map[string]bool{}
	for _, r := range s.Panes {
		if r.ID == "" || r.Birth == "" || r.Owner.PID <= 0 || r.Owner.Started <= 0 || !filepath.IsAbs(r.Socket) || r.Reserved.IsZero() || ids[r.ID] {
			return errors.New("invalid durable pane reservation")
		}
		ids[r.ID] = true
		if p.probe(r) {
			s.Audit = append(s.Audit, LeaseAudit{At: now, Action: "pane-recovered", ID: r.ID, Owner: r.Owner, Reason: "exact owner death; live runtime panes counted separately"})
			continue
		}
		kept = append(kept, r)
	}
	s.Panes = kept
	return nil
}
func (p *PaneLedger) Release(ctx context.Context, id string, owner Owner, birth string) error {
	return p.ledger.transact(ctx, true, func(s *ledgerState) error {
		if e := p.reconcile(s, time.Now().UTC()); e != nil {
			return e
		}
		for i, r := range s.Panes {
			if r.ID == id {
				if r.Owner != owner || r.Birth != birth {
					return errors.New("pane reservation owner mismatch")
				}
				s.Panes = append(s.Panes[:i], s.Panes[i+1:]...)
				s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "pane-released", ID: id, Owner: owner})
				return nil
			}
		}
		return nil
	})
}
func (p *PaneLedger) Snapshot(ctx context.Context) ([]PaneReservation, error) {
	var out []PaneReservation
	err := p.ledger.transact(ctx, true, func(s *ledgerState) error {
		if e := p.reconcile(s, time.Now().UTC()); e != nil {
			return e
		}
		out = append(out, s.Panes...)
		return nil
	})
	return out, err
}

// Bind links an already-reserved launch to its private API socket before herdr-core
// starts. No interval can expose a live pane without its durable reservation.
func (p *PaneLedger) Bind(ctx context.Context, id string, owner Owner, birth, socket string) error {
	if !filepath.IsAbs(socket) {
		return errors.New("absolute pane socket required")
	}
	return p.ledger.transact(ctx, true, func(s *ledgerState) error {
		if e := p.reconcile(s, time.Now().UTC()); e != nil {
			return e
		}
		for i, r := range s.Panes {
			if r.ID == id {
				if r.Owner != owner || r.Birth != birth {
					return errors.New("pane reservation owner mismatch")
				}
				s.Panes[i].Socket = socket
				return nil
			}
		}
		return errors.New("pane reservation missing before launch")
	})
}
func (e *AdmissionRefusal) AdmissionDeferred() bool { return true }

// Configure atomically changes only future admission. Existing pane reservations
// survive reductions, and cooperating instances reconcile to the smallest limit.
func (p *PaneLedger) Configure(ctx context.Context, instance string, limit int) error {
	if instance == "" || limit < 2 || limit > 1000000 {
		return errors.New("invalid instance pane limit")
	}
	return p.ledger.transact(ctx, true, func(s *ledgerState) error {
		if paneLimit(s) < 2 {
			return errors.New("invalid shared pane limit")
		}
		owner, err := CurrentOwner()
		if err != nil {
			return err
		}
		birth, err := processowner.Birth(owner.PID)
		if err != nil {
			return err
		}
		if s.PaneLimits == nil {
			s.PaneLimits = map[string]PaneLimitRecord{}
		}
		if old := s.PaneLimits[instance]; old.Limit == limit && old.Owner == owner && old.Birth == birth {
			return nil
		}
		if old, ok := s.PaneLimits[instance]; ok && (old.Owner != owner || old.Birth != birth) && !processowner.ProvedDead(old.Owner.PID, old.Birth) {
			return errors.New("pane limit belongs to another live owner")
		}
		s.PaneLimits[instance] = PaneLimitRecord{Limit: limit, Owner: owner, Birth: birth}
		s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "pane-limit-configured", ID: instance, Reason: fmt.Sprintf("requested=%d effective=%d", limit, paneLimit(s))})
		return nil
	})
}

type PaneLimitRecord struct {
	Limit int
	Owner Owner
	Birth string
}

func paneLimit(s *ledgerState) int {
	for id, record := range s.PaneLimits {
		if id == "" || record.Owner.PID <= 1 || record.Owner.Started <= 0 || record.Birth == "" || record.Limit < 2 || record.Limit > 1000000 {
			return 0
		}
		if processowner.ProvedDead(record.Owner.PID, record.Birth) {
			delete(s.PaneLimits, id)
		}
	}
	limit := s.Pool.Capacity
	if len(s.PaneLimits) > 0 {
		limit = 1000000
	}
	for _, record := range s.PaneLimits {
		n := record.Limit
		if n < 2 || n > 1000000 {
			return 0
		}
		limit = TightenPaneLimit(limit, n)
	}
	return limit
}
func (p *PaneLedger) Limit(ctx context.Context) (int, error) {
	var limit int
	err := p.ledger.transact(ctx, false, func(s *ledgerState) error {
		limit = paneLimit(s)
		if limit < 2 {
			return errors.New("invalid shared pane limit")
		}
		return nil
	})
	return limit, err
}

// Unconfigure releases only the caller's exact configuration registration. Pane
// reservations remain independently durable until their retirement proof exists.
func (p *PaneLedger) Unconfigure(ctx context.Context, instance string, owner Owner, birth string) error {
	return p.ledger.transact(ctx, true, func(s *ledgerState) error {
		if paneLimit(s) < 2 {
			return errors.New("invalid shared pane limit")
		}
		old, ok := s.PaneLimits[instance]
		if !ok {
			return nil
		}
		if owner != old.Owner || birth != old.Birth {
			return errors.New("pane configuration owner mismatch")
		}
		delete(s.PaneLimits, instance)
		s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "pane-limit-unconfigured", ID: instance, Owner: owner})
		return nil
	})
}
