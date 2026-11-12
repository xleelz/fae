package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
)

// LaunchWarmup is how long a session's FAE-HOST-006 memory reservation is held
// after admission.
const LaunchWarmup = 180 * time.Second

// LaunchReservation is the FAE-HOST-006 memory reservation of a session that
// has no pane ledger entry (the Direct adapter). It lives in the host-wide
// ledger, so the launches of every cooperating instance count as pending.
type LaunchReservation struct {
	ID, Birth string
	Owner     Owner
	Reserved  time.Time
}

// reconcileLaunches drops reservations past their warm-up and those whose
// owner is proven dead, and returns the pending count.
func reconcileLaunches(s *ledgerState, now time.Time) int {
	kept := s.Launches[:0]
	for _, r := range s.Launches {
		if now.Sub(r.Reserved) >= LaunchWarmup || processowner.ProvedDead(r.Owner.PID, r.Birth) {
			continue
		}
		kept = append(kept, r)
	}
	s.Launches = kept
	return len(kept)
}

// ReserveLaunch admits one launch under the ledger lock: memory available must
// cover the hard floor plus (pending launches + 1) per-session estimates, and
// the hard and soft guards apply to the same reading.
func (l *Ledger) ReserveLaunch(ctx context.Context, r LaunchReservation, observe func(context.Context) (hostguard.Sample, error), cfg hostguard.Config, now time.Time) error {
	if r.ID == "" || r.Birth == "" || r.Owner.PID <= 0 || r.Owner.Started <= 0 || r.Reserved.IsZero() {
		return errors.New("invalid launch reservation")
	}
	var refusal error
	err := l.transact(ctx, true, func(s *ledgerState) error {
		pending := reconcileLaunches(s, now)
		for _, x := range s.Launches {
			if x.ID == r.ID {
				return errors.New("duplicate launch reservation")
			}
		}
		sample, e := observe(ctx)
		if e != nil {
			return e
		}
		sample.LivePanes = 0
		if d := hostguard.Admit(sample, cfg, pending, true); !d.Allowed {
			refusal = &AdmissionRefusal{d.Reason}
			return nil
		}
		s.Launches = append(s.Launches, r)
		s.Audit = append(s.Audit, LeaseAudit{At: now, Action: "launch-reserved", ID: r.ID, Owner: r.Owner, Reason: fmt.Sprintf("pending=%d", pending)})
		return nil
	})
	if err != nil {
		return err
	}
	return refusal
}

// ReleaseLaunch withdraws the reservation of a launch that never started.
func (l *Ledger) ReleaseLaunch(ctx context.Context, id string, owner Owner) error {
	return l.transact(ctx, true, func(s *ledgerState) error {
		for i, r := range s.Launches {
			if r.ID == id {
				if r.Owner != owner {
					return errors.New("launch reservation owner mismatch")
				}
				s.Launches = append(s.Launches[:i], s.Launches[i+1:]...)
				s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "launch-released", ID: id, Owner: owner})
				return nil
			}
		}
		return nil
	})
}

// PendingLaunches counts the reservations still in their warm-up.
func (l *Ledger) PendingLaunches(ctx context.Context, now time.Time) (int, error) {
	var n int
	err := l.transact(ctx, true, func(s *ledgerState) error { n = reconcileLaunches(s, now); return nil })
	return n, err
}
