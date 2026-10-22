// Package providerhealth owns route scoring and the single bounded session-log reader.
package providerhealth

import (
	"math"
	"time"
)

type Outcome string

const (
	OK              Outcome = "ok"
	RateLimited     Outcome = "rate-limited"
	Unavailable     Outcome = "unavailable"
	AuthFailure     Outcome = "auth-failure"
	ModelAPIError   Outcome = "model-api-error"
	Timeout         Outcome = "timeout"
	MalformedOutput Outcome = "malformed-output"
)

func (o Outcome) Valid() bool {
	switch o {
	case OK, RateLimited, Unavailable, AuthFailure, ModelAPIError, Timeout, MalformedOutput:
		return true
	}
	return false
}

type Health struct {
	Provider    string    `json:"provider"`
	Model       string    `json:"model"`
	Outcome     Outcome   `json:"outcome"`
	Score       float64   `json:"score"`
	Failures    int64     `json:"failures"`
	LastOK      time.Time `json:"last_ok"`
	LastFailure time.Time `json:"last_failure"`
	HoldUntil   time.Time `json:"hold_until"`
}

func (h Health) At(now time.Time) float64 {
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
	score := h.At(now)
	if o == OK {
		if !now.Before(h.HoldUntil) {
			score = 100
		}
		h.LastOK = now
	} else {
		penalty := 40.0
		switch o {
		case RateLimited:
			penalty = 60
			h.HoldUntil = now.Add(time.Hour)
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
func (h Health) Eligible(now time.Time) bool { return Eligible(int(h.At(now) * 1000)) }
