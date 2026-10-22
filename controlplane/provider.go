package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xleelz/fae/providerhealth"
)

type ProviderObservation struct {
	ID       string                 `json:"id"`
	TaskID   string                 `json:"task_id"`
	Provider string                 `json:"provider"`
	Model    string                 `json:"model"`
	Outcome  providerhealth.Outcome `json:"outcome"`
	At       time.Time              `json:"at"`
}

// ObserveProvider commits health, an operator-visible audit line and the event
// together. Session identity is the idempotence key, including across crashes.
// The audit table is the durable operator log (not a best-effort file append).
func (s *Store) ObserveProvider(ctx context.Context, o ProviderObservation) error {
	if o.ID == "" || o.TaskID == "" || o.Provider == "" || o.Model == "" || !o.Outcome.Valid() || o.At.IsZero() {
		return errors.New("complete provider observation required")
	}
	raw, _ := json.Marshal(o)
	digest, canonical, e := ContentHash(raw)
	if e != nil {
		return e
	}
	return s.transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if e := lockEvents(ctx, tx); e != nil {
			return e
		}
		var prior string
		e := tx.QueryRowContext(ctx, `SELECT digest FROM provider_observations WHERE observation_id=?`, o.ID).Scan(&prior)
		if e == nil {
			if prior != digest {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		h := providerhealth.Health{Provider: o.Provider, Model: o.Model}
		var lastOK, lastFailure, hold sql.NullTime
		var outcome sql.NullString
		e = tx.QueryRowContext(ctx, `SELECT outcome,score,failures,last_ok,last_failure,hold_until FROM provider_health WHERE provider=? AND model=? FOR UPDATE`, o.Provider, o.Model).Scan(&outcome, &h.Score, &h.Failures, &lastOK, &lastFailure, &hold)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		h.LastOK = lastOK.Time
		h.LastFailure = lastFailure.Time
		h.HoldUntil = hold.Time
		// Concurrent observations serialize in commit order; wall-clock rollback
		// cannot move the decay reference backwards.
		at := o.At
		if at.Before(h.LastOK) {
			at = h.LastOK
		}
		if at.Before(h.LastFailure) {
			at = h.LastFailure
		}
		h = h.Observe(o.Outcome, at)
		_, e = tx.ExecContext(ctx, `REPLACE INTO provider_health (provider,model,outcome,score,failures,last_ok,last_failure,hold_until) VALUES (?,?,?,?,?,?,?,?)`, o.Provider, o.Model, string(h.Outcome), h.Score, h.Failures, nullableTime(h.LastOK), nullableTime(h.LastFailure), nullableTime(h.HoldUntil))
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO provider_observations (observation_id,digest,payload) VALUES (?,?,?)`, o.ID, digest, string(canonical))
		if e != nil {
			return e
		}
		line := fmt.Sprintf("provider %s model %s outcome %s score %.3f session %s", o.Provider, o.Model, o.Outcome, h.Score, o.ID)
		_, e = tx.ExecContext(ctx, `INSERT INTO audit (actor,action,message_id,inputs,argument_digest,outcome,created_at) VALUES (?,?,?,?,?,?,?)`, "daemon", "provider observation", o.ID, string(canonical), digest, line, at)
		if e != nil {
			return e
		}
		eventRaw, _ := json.Marshal(map[string]any{"observation": o, "score": h.Score, "line": line})
		return s.appendEvents(ctx, tx, o.TaskID, []Event{{Kind: "provider-observation", Payload: eventRaw}}, "")
	})
}
func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}
func (s *Store) ProviderHealth(ctx context.Context) ([]providerhealth.Health, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT provider,model,outcome,score,failures,last_ok,last_failure,hold_until FROM provider_health ORDER BY provider,model`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	result := []providerhealth.Health{}
	for rows.Next() {
		var h providerhealth.Health
		var outcome sql.NullString
		var a, b, c sql.NullTime
		if e = rows.Scan(&h.Provider, &h.Model, &outcome, &h.Score, &h.Failures, &a, &b, &c); e != nil {
			return nil, e
		}
		h.Outcome = providerhealth.Outcome(outcome.String)
		h.LastOK = a.Time
		h.LastFailure = b.Time
		h.HoldUntil = c.Time
		result = append(result, h)
	}
	return result, rows.Err()
}
