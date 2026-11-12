package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/providerhealth"
)

type ProviderObservation struct {
	ID           string                 `json:"id"`
	TaskID       string                 `json:"task_id"`
	Provider     string                 `json:"provider"`
	Model        string                 `json:"model"`
	Outcome      providerhealth.Outcome `json:"outcome"`
	At           time.Time              `json:"at"`
	ResetAt      time.Time              `json:"reset_at,omitempty"`
	GraceSeconds int                    `json:"grace_seconds,omitempty"`
	CutOffTaskID string                 `json:"cut_off_task_id,omitempty"`
}

// ProviderQuotaState is the durable FAE-RT-018 quota-exhausted-until-T state
// recorded for an unpooled (provider, model) binding.
type ProviderQuotaState struct {
	Provider       string    `json:"provider"`
	Model          string    `json:"model"`
	ResetAt        time.Time `json:"reset_at"`
	GraceSeconds   int       `json:"grace_seconds"`
	ExhaustedUntil time.Time `json:"exhausted_until"`
	CutOffTaskID   string    `json:"cut_off_task_id,omitempty"`
	Actor          string    `json:"actor,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func providerQuotaMarkerName(provider, model string) string {
	return "provider-quota/" + provider + "/" + model
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
		h.Outcome = providerhealth.Outcome(outcome.String)
		h.LastOK = lastOK.Time
		h.LastFailure = lastFailure.Time
		h.HoldUntil = hold.Time
		if h.Outcome == providerhealth.RateLimited && h.Failures > 0 {
			h.RateLimitProbes = int(h.Failures)
		}
		// Concurrent observations serialize in commit order; wall-clock rollback
		// cannot move the decay reference backwards.
		at := o.At
		if at.Before(h.LastOK) {
			at = h.LastOK
		}
		if at.Before(h.LastFailure) {
			at = h.LastFailure
		}
		grace := providerhealth.DefaultQuotaGrace
		if o.GraceSeconds > 0 {
			grace = time.Duration(o.GraceSeconds) * time.Second
		}
		h = h.ObserveQuota(o.Outcome, at, o.ResetAt, grace, o.CutOffTaskID)
		_, e = tx.ExecContext(ctx, `REPLACE INTO provider_health (provider,model,outcome,score,failures,last_ok,last_failure,hold_until) VALUES (?,?,?,?,?,?,?,?)`, o.Provider, o.Model, string(h.Outcome), h.Score, h.Failures, nullableTime(h.LastOK), nullableTime(h.LastFailure), nullableTime(h.HoldUntil))
		if e != nil {
			return e
		}
		if o.Outcome == providerhealth.RateLimited && !o.ResetAt.IsZero() {
			q := ProviderQuotaState{
				Provider:       o.Provider,
				Model:          o.Model,
				ResetAt:        h.QuotaResetAt,
				GraceSeconds:   int(grace / time.Second),
				ExhaustedUntil: h.QuotaExhaustedUntil,
				CutOffTaskID:   h.CutOffTaskID,
				Actor:          "daemon",
				Reason:         "provider usage limit",
				UpdatedAt:      at,
			}
			qRaw, _ := json.Marshal(q)
			if e = s.putMarkerTx(ctx, tx, providerQuotaMarkerName(o.Provider, o.Model), qRaw); e != nil {
				return e
			}
		} else if o.Outcome == providerhealth.OK {
			_ = s.deleteMarkerTx(ctx, tx, providerQuotaMarkerName(o.Provider, o.Model))
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
		if e = s.appendEvents(ctx, tx, o.TaskID, []Event{{Kind: "provider-observation", Payload: eventRaw}}, ""); e != nil {
			return e
		}
		return s.versionCommit(ctx, tx, fmt.Sprintf("provider observation %s/%s %s by daemon", o.Provider, o.Model, o.Outcome))
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
	indexByKey := map[[2]string]int{}
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
		if h.Outcome == providerhealth.RateLimited && h.Failures > 0 && !h.LastFailure.IsZero() {
			probes := int(h.Failures)
			h.RateLimitProbes = probes
			h.NextProbeAt = h.LastFailure.Add(providerhealth.ProbeBackoffDelay(probes-1, providerhealth.DefaultProbeCap))
		}
		indexByKey[[2]string{h.Provider, h.Model}] = len(result)
		result = append(result, h)
	}
	if e = rows.Err(); e != nil {
		return nil, e
	}
	qMarkers, qErr := s.MarkersWithPrefix(ctx, "provider-quota/", 1000)
	if qErr == nil {
		qKeys := make([]string, 0, len(qMarkers))
		for k := range qMarkers {
			qKeys = append(qKeys, k)
		}
		sort.Strings(qKeys)
		for _, k := range qKeys {
			var q ProviderQuotaState
			if json.Unmarshal(qMarkers[k], &q) != nil || q.Provider == "" || q.Model == "" || q.ExhaustedUntil.IsZero() {
				continue
			}
			key := [2]string{q.Provider, q.Model}
			if idx, ok := indexByKey[key]; ok {
				h := &result[idx]
				if !h.LastOK.IsZero() && h.LastOK.After(q.UpdatedAt) {
					continue
				}
				h.QuotaResetAt = q.ResetAt
				h.QuotaExhaustedUntil = q.ExhaustedUntil
				h.CutOffTaskID = q.CutOffTaskID
				h.HoldUntil = q.ExhaustedUntil
				h.RateLimitProbes = 0
				h.NextProbeAt = time.Time{}
			} else {
				h := providerhealth.Health{
					Provider:            q.Provider,
					Model:               q.Model,
					Outcome:             providerhealth.RateLimited,
					Score:               40,
					Failures:            1,
					LastFailure:         q.UpdatedAt,
					HoldUntil:           q.ExhaustedUntil,
					QuotaResetAt:        q.ResetAt,
					QuotaExhaustedUntil: q.ExhaustedUntil,
					CutOffTaskID:        q.CutOffTaskID,
				}
				indexByKey[key] = len(result)
				result = append(result, h)
			}
		}
		sort.Slice(result, func(i, j int) bool {
			if result[i].Provider != result[j].Provider {
				return result[i].Provider < result[j].Provider
			}
			return result[i].Model < result[j].Model
		})
	}
	return result, nil
}

// SetProviderQuota marks a (provider, model) binding quota-exhausted until T + grace
// with an audited mutation (FAE-RT-018).
func (s *Store) SetProviderQuota(ctx context.Context, a Audit, q ProviderQuotaState) error {
	q.Provider = strings.TrimSpace(q.Provider)
	q.Model = strings.TrimSpace(q.Model)
	if q.Provider == "" || q.Model == "" || q.ResetAt.IsZero() {
		return errors.New("provider, model and reset time required")
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
	raw, _ := json.Marshal(q)
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if e := s.putMarkerTx(ctx, tx, providerQuotaMarkerName(q.Provider, q.Model), raw); e != nil {
			return e
		}
		return s.insertAuthorityDecisionTx(ctx, tx, "quota-set", q.Provider+"/"+q.Model, a.Actor, q.Reason, raw)
	})
}

// ClearProviderQuota clears a (provider, model) binding's quota-exhausted mark
// and restores its health hold with an audited mutation (FAE-RT-018).
func (s *Store) ClearProviderQuota(ctx context.Context, a Audit, provider, model string) error {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	if provider == "" || model == "" {
		return errors.New("provider and model required")
	}
	now := time.Now().UTC()
	raw, _ := json.Marshal(map[string]any{"provider": provider, "model": model, "cleared_at": now})
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if e := s.deleteMarkerTx(ctx, tx, providerQuotaMarkerName(provider, model)); e != nil {
			return e
		}
		if _, e := tx.ExecContext(ctx, `UPDATE provider_health SET outcome='ok',score=100,last_ok=?,hold_until=NULL WHERE provider=? AND model=?`, now, provider, model); e != nil {
			return e
		}
		return s.insertAuthorityDecisionTx(ctx, tx, "quota-clear", provider+"/"+model, a.Actor, a.Outcome, raw)
	})
}
