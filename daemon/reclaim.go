package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/scheduler"
)

// ReclaimParams requests on-demand reconciliation of the role-slot and
// session ledgers, and with HostWide also the host heavy-op and pane ledgers.
type ReclaimParams struct {
	HostWide           bool   `json:"host_wide,omitempty"`
	DryRun             bool   `json:"dry_run,omitempty"`
	AuthorizingMessage string `json:"authorizing_message"`
}
type ReclaimLedger struct {
	Ledger    string                   `json:"ledger"`
	Scope     string                   `json:"scope"`
	Status    string                   `json:"status"`
	Reason    string                   `json:"reason,omitempty"`
	Reclaimed []scheduler.ReclaimEntry `json:"reclaimed"`
	Kept      []scheduler.ReclaimEntry `json:"kept"`
}

// reclaim clears provably dead-owner entries (pid absent or start time
// differs) and keeps live and ambiguous ones. An unreadable ledger fails the
// command closed. Each reclaimed entry gets its own audit row citing the message.
func (d *Daemon) reclaim(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p ReclaimParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.HostWide {
		direction, err := d.store.MessageDirection(ctx, p.AuthorizingMessage)
		if err != nil {
			return nil, err
		}
		if !operatorListHas(direction.Classes, "host-wide") && !operatorListHas(direction.Verbs, "Reclaim.host-wide") {
			return nil, errors.New("host-wide reclaim requires a message that explicitly grants the host-wide class")
		}
	}
	status := "reconciled"
	if p.DryRun {
		status = "dry-run"
	}
	ledgers := []ReclaimLedger{}
	roles := ReclaimLedger{Ledger: "role-slots", Scope: "instance", Status: status, Reclaimed: []scheduler.ReclaimEntry{}, Kept: []scheduler.ReclaimEntry{}}
	keys, err := d.store.LeasePoolKeys(ctx)
	if err != nil {
		return nil, fmt.Errorf("role-slot ledger unreadable: %w", err)
	}
	for _, key := range keys {
		r, err := d.store.ReclaimLeasePool(ctx, audit, key, scheduler.ProbeOwner, p.DryRun)
		if err != nil {
			return nil, fmt.Errorf("role-slot pool %s unreadable: %w", key, err)
		}
		roles.Reclaimed, roles.Kept = append(roles.Reclaimed, r.Reclaimed...), append(roles.Kept, r.Kept...)
	}
	ledgers = append(ledgers, roles)
	sessions, err := d.reclaimSessions(ctx, p.DryRun, status)
	if err != nil {
		return nil, err
	}
	ledgers = append(ledgers, sessions)
	for _, name := range []string{"heavy-op", "panes"} {
		l := ReclaimLedger{Ledger: name, Scope: "host", Status: status, Reclaimed: []scheduler.ReclaimEntry{}, Kept: []scheduler.ReclaimEntry{}}
		var r scheduler.ReclaimReport
		switch {
		case !p.HostWide:
			l.Status, l.Reason = "skipped", "host-scoped ledger; requires --host-wide"
		case d.live == nil || d.live.guard == nil || (name == "panes" && d.live.guard.panes == nil):
			l.Status, l.Reason = "not-configured", "no live host ledger for this adapter"
		case name == "heavy-op":
			if r, err = d.live.guard.host.ReclaimDead(ctx, scheduler.ProbeOwner, p.DryRun); err != nil {
				return nil, fmt.Errorf("heavy-op ledger unreadable: %w", err)
			}
			if !p.DryRun {
				if snaps, snapErr := d.live.guard.host.ProviderPoolsSnapshot(ctx); snapErr == nil {
					for _, snap := range snaps {
						d.live.guard.mirrorProviderPool(ctx, snap)
					}
				}
			}
		default:
			if r, err = d.live.guard.panes.ReclaimDead(ctx, p.DryRun); err != nil {
				return nil, fmt.Errorf("pane ledger unreadable: %w", err)
			}
		}
		if r.Reclaimed != nil {
			l.Reclaimed, l.Kept = r.Reclaimed, r.Kept
		}
		if !p.DryRun {
			for _, entry := range l.Reclaimed {
				if err = d.reclaimAudit(ctx, audit, name+" "+entry.ID, entry); err != nil {
					return nil, err
				}
			}
		}
		ledgers = append(ledgers, l)
	}
	if !p.DryRun {
		if err = d.reclaimAudit(ctx, audit, "", ledgers); err != nil {
			return nil, err
		}
	}
	return map[string]any{"dry_run": p.DryRun, "host_wide": p.HostWide, "ledgers": ledgers}, nil
}

func (d *Daemon) reclaimAudit(ctx context.Context, audit controlplane.Audit, what string, inputs any) error {
	if what != "" {
		audit.Outcome = "reclaimed " + what
	}
	audit.Inputs, _ = json.Marshal(inputs)
	return d.store.Mutate(ctx, audit, func(context.Context, *sql.Tx) error { return nil })
}

// reclaimSessions reconciles this instance's session lease proofs through the
// startup recovery path: a dead owner's never-launched or proven-retired
// session is reclaimed; a launch without retirement proof stays held.
func (d *Daemon) reclaimSessions(ctx context.Context, dryRun bool, status string) (ReclaimLedger, error) {
	l := ReclaimLedger{Ledger: "sessions", Scope: "instance", Status: status, Reclaimed: []scheduler.ReclaimEntry{}, Kept: []scheduler.ReclaimEntry{}}
	if d.live == nil || d.live.guard == nil || d.live.guard.journal == nil {
		l.Status, l.Reason = "not-configured", "no session lease journal for this adapter"
		return l, nil
	}
	before, err := d.live.guard.journal.Entries()
	if err != nil {
		return l, fmt.Errorf("session ledger unreadable: %w", err)
	}
	remaining := map[string]bool{}
	if dryRun {
		for _, proof := range before {
			dead := processowner.ProvedDead(proof.Owner.PID, proof.Birth)
			remaining[proof.ID] = !scheduler.CanRecoverLease(dead, true, proof.State == "reserved", proof.State == "retired")
		}
	} else {
		if err = d.live.guard.recoverProofs(ctx); err != nil {
			return l, fmt.Errorf("session ledger reconcile failed closed: %w", err)
		}
		after, err := d.live.guard.journal.Entries()
		if err != nil {
			return l, err
		}
		for _, proof := range after {
			remaining[proof.ID] = true
		}
	}
	for _, proof := range before {
		e := scheduler.ReclaimEntry{Pool: proof.RolePool, ID: proof.Session, Kind: "session", Owner: proof.Owner, State: proof.State, Liveness: "not-proved-dead"}
		if processowner.ProvedDead(proof.Owner.PID, proof.Birth) {
			e.Liveness = "dead"
		}
		if remaining[proof.ID] {
			l.Kept = append(l.Kept, e)
		} else {
			l.Reclaimed = append(l.Reclaimed, e)
		}
	}
	return l, nil
}
