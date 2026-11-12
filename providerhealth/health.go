// Package providerhealth owns route scoring and the single bounded session-log reader.
package providerhealth

import (
	"math"
	"time"
)

type Outcome string

const (
	OK                  Outcome = "ok"
	RateLimited         Outcome = "rate-limited"
	Unavailable         Outcome = "unavailable"
	AuthFailure         Outcome = "auth-failure"
	ModelAPIError       Outcome = "model-api-error"
	Timeout             Outcome = "timeout"
	MalformedOutput     Outcome = "malformed-output"
	TurnBudgetExhausted Outcome = "turn-budget-exhausted"
)

func (o Outcome) Valid() bool {
	switch o {
	case OK, RateLimited, Unavailable, AuthFailure, ModelAPIError, Timeout, MalformedOutput, TurnBudgetExhausted:
		return true
	}
	return false
}

const (
	// DefaultQuotaGrace is the FAE-RT-018 default grace period added after a
	// provider usage-limit reset time T before resuming dispatch.
	DefaultQuotaGrace = 60 * time.Second
	// InitialProbeBackoff is the initial FAE-RT-018 exponential backoff probe
	// interval when a rate-limit event has no parsed reset time.
	InitialProbeBackoff = time.Minute
	// DefaultProbeCap is the FAE-RT-018 default maximum probe interval for
	// rate-limit events without a parsed reset time.
	DefaultProbeCap = time.Hour
)

type Health struct {
	Provider            string    `json:"provider"`
	Model               string    `json:"model"`
	Outcome             Outcome   `json:"outcome"`
	Score               float64   `json:"score"`
	Failures            int64     `json:"failures"`
	LastOK              time.Time `json:"last_ok"`
	LastFailure         time.Time `json:"last_failure"`
	HoldUntil           time.Time `json:"hold_until"`
	QuotaResetAt        time.Time `json:"quota_reset_at,omitempty"`
	QuotaExhaustedUntil time.Time `json:"quota_exhausted_until,omitempty"`
	CutOffTaskID        string    `json:"cut_off_task_id,omitempty"`
	RateLimitProbes     int       `json:"rate_limit_probes,omitempty"`
	NextProbeAt         time.Time `json:"next_probe_at,omitempty"`
}

// ProbeBackoffDelay computes the FAE-RT-018 exponential backoff delay (1m, 2m,
// 4m, 8m, ... up to cap, default 1h) for probe index probes (0-based).
func ProbeBackoffDelay(probes int, cap time.Duration) time.Duration {
	if cap <= 0 {
		cap = DefaultProbeCap
	}
	if probes < 0 {
		probes = 0
	}
	if probes > 16 {
		probes = 16
	}
	d := InitialProbeBackoff << probes
	if d > cap || d <= 0 {
		return cap
	}
	return d
}

// QuotaActive reports whether the binding is currently marked quota-exhausted
// until T + grace (FAE-RT-018).
func (h Health) QuotaActive(now time.Time) bool {
	return !h.QuotaExhaustedUntil.IsZero() && now.Before(h.QuotaExhaustedUntil)
}

// ProbeDue reports whether a rate-limited binding without a reset time has
// reached its next exponential-backoff probe instant (FAE-RT-018).
func (h Health) ProbeDue(now time.Time) bool {
	if h.QuotaActive(now) {
		return false
	}
	return !h.NextProbeAt.IsZero() && !now.Before(h.NextProbeAt)
}

func (h Health) At(now time.Time) float64 {
	if h.QuotaActive(now) {
		return 0
	}
	last := h.LastOK
	if h.LastFailure.After(last) {
		last = h.LastFailure
	}
	if last.IsZero() {
		return 100
	}
	elapsed := math.Max(0, now.Sub(last).Seconds())
	return 100 - (100-h.Score)*math.Exp2(-elapsed/3600)
}
func (h Health) Observe(o Outcome, now time.Time) Health {
	return h.ObserveQuota(o, now, time.Time{}, DefaultQuotaGrace, "")
}

// ObserveQuota records a provider outcome at now, optionally carrying a parsed
// usage-limit reset time T, grace period, and cut-off task ID (FAE-RT-018).
func (h Health) ObserveQuota(o Outcome, now, resetAt time.Time, grace time.Duration, cutOffTaskID string) Health {
	if o == TurnBudgetExhausted {
		// This is a task budget outcome, not a provider observation. Preserve
		// the latent score, its decay reference and any active throttle/quota:
		// At(now) deliberately returns zero while quota is active.
		if h.LastOK.IsZero() && h.LastFailure.IsZero() {
			h.Score = 100
		}
		return h
	}
	score := h.At(now)
	if o == OK {
		if !now.Before(h.HoldUntil) {
			score = 100
		}
		h.LastOK = now
		h.QuotaResetAt = time.Time{}
		h.QuotaExhaustedUntil = time.Time{}
		h.CutOffTaskID = ""
		h.RateLimitProbes = 0
		h.NextProbeAt = time.Time{}
	} else {
		penalty := 40.0
		switch o {
		case RateLimited:
			penalty = 60
			if !resetAt.IsZero() {
				if grace < 0 {
					grace = 0
				}
				h.QuotaResetAt = resetAt.UTC()
				h.QuotaExhaustedUntil = resetAt.UTC().Add(grace)
				h.HoldUntil = now.Add(time.Hour)
				if h.QuotaExhaustedUntil.After(h.HoldUntil) {
					h.HoldUntil = h.QuotaExhaustedUntil
				}
				h.RateLimitProbes = 0
				h.NextProbeAt = time.Time{}
				if cutOffTaskID != "" && h.CutOffTaskID == "" {
					h.CutOffTaskID = cutOffTaskID
				}
			} else {
				h.HoldUntil = now.Add(time.Hour)
				h.QuotaResetAt = time.Time{}
				h.QuotaExhaustedUntil = time.Time{}
				delay := ProbeBackoffDelay(h.RateLimitProbes, DefaultProbeCap)
				h.NextProbeAt = now.Add(delay)
				h.RateLimitProbes++
			}
		case Unavailable, AuthFailure:
			penalty = 100
		}
		score = math.Max(0, score-penalty)
		h.LastFailure = now
		h.Failures++
	}
	h.Score = score
	h.Outcome = o
	return h
}

// Eligible applies the health floor even when a quota expires or a probe is due.
// A probe timestamp is a scheduling constraint, never a health waiver.
func (h Health) Eligible(now time.Time) bool {
	if h.QuotaActive(now) {
		return false
	}
	if h.Outcome == RateLimited && h.QuotaExhaustedUntil.IsZero() && !h.NextProbeAt.IsZero() {
		return !now.Before(h.NextProbeAt) && Eligible(int(h.At(now)*1000))
	}
	return Eligible(int(h.At(now) * 1000))
}
