package providerhealth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestClassifyDetailedExtractsResetTimeAndSessionID(t *testing.T) {
	dir := t.TempDir()
	resetTime := time.Date(2026, 11, 5, 14, 30, 0, 0, time.UTC)
	streamLog := `{"type":"system","session_id":"sess-abc-123"}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resets_at":"` + resetTime.Format(time.RFC3339) + `"}}
`
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte(streamLog), 0600); err != nil {
		t.Fatal(err)
	}
	detail := ClassifyDetailed(dir, false, false)
	if detail.Outcome != RateLimited {
		t.Fatalf("expected RateLimited, got %s", detail.Outcome)
	}
	if !detail.ResetAt.Equal(resetTime) {
		t.Fatalf("expected ResetAt=%v, got %v", resetTime, detail.ResetAt)
	}
	if detail.SessionID != "sess-abc-123" {
		t.Fatalf("expected SessionID=sess-abc-123, got %q", detail.SessionID)
	}

	now := time.Date(2026, 11, 5, 12, 30, 0, 0, time.UTC)
	h := Health{Provider: "anthropic", Model: "claude", Score: 100}
	h = h.ObserveQuota(RateLimited, now, detail.ResetAt, 60*time.Second, "task-cut-off")
	if !h.QuotaActive(now) {
		t.Fatal("expected QuotaActive at now")
	}
	if h.Eligible(now) {
		t.Fatal("expected ineligible while quota-exhausted")
	}
	if h.Eligible(resetTime.Add(30 * time.Second)) {
		t.Fatal("expected ineligible during 60s grace period after T")
	}
	if !h.Eligible(resetTime.Add(60 * time.Second)) {
		t.Fatal("expected eligible at T + grace (60s)")
	}
}

func TestRateLimitWithoutResetTimeExponentialBackoffProbes(t *testing.T) {
	base := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	h := Health{Provider: "anthropic", Model: "claude", Score: 100}

	wantDelays := []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		32 * time.Minute,
		60 * time.Minute,
		60 * time.Minute,
	}
	now := base
	for i, wantDelay := range wantDelays {
		h = h.Observe(RateLimited, now)
		if h.QuotaActive(now) {
			t.Fatalf("step %d: rate limit without reset time must not set QuotaActive", i)
		}
		wantProbeAt := now.Add(wantDelay)
		if !h.NextProbeAt.Equal(wantProbeAt) {
			t.Fatalf("step %d: NextProbeAt=%v, want %v (delay %v)", i, h.NextProbeAt, wantProbeAt, wantDelay)
		}
		if h.ProbeDue(now.Add(wantDelay - time.Second)) {
			t.Fatalf("step %d: probe should not be due before backoff %v", i, wantDelay)
		}
		if !h.ProbeDue(wantProbeAt) {
			t.Fatalf("step %d: probe should be due at %v", i, wantProbeAt)
		}
		now = wantProbeAt
	}
}

// FAE-RT-009/-010: probe scheduling never authorizes an unhealthy binding.
func TestDueProbeStillRequiresHealthyScore(t *testing.T) {
	now := time.Unix(10000, 0)
	h := Health{}.Observe(RateLimited, now)
	if !h.ProbeDue(now.Add(time.Minute)) || h.At(now.Add(time.Minute)) >= 50 || h.Eligible(now.Add(time.Minute)) {
		t.Fatalf("below-threshold probe dispatched: %+v", h)
	}
	if !h.Eligible(now.Add(time.Hour)) {
		t.Fatal("healed binding must become eligible")
	}
	// A due probe after repeated throttles remains blocked until healing.
	h = h.Observe(RateLimited, now.Add(time.Minute))
	if h.Eligible(h.NextProbeAt) {
		t.Fatal("repeat throttle bypassed eligibility floor")
	}
}

func TestQuotaExpiryDoesNotBypassHealthFloorOrThrottleHold(t *testing.T) {
	now := time.Unix(10000, 0)
	h := Health{}.ObserveQuota(RateLimited, now, now.Add(time.Second), time.Second, "")
	due := now.Add(2 * time.Second)
	if h.QuotaActive(due) || h.Eligible(due) || h.At(due) >= 50 {
		t.Fatalf("expired quota bypassed score: %+v", h)
	}
	h = h.Observe(OK, due)
	if h.Score >= 50 || !h.HoldUntil.Equal(now.Add(time.Hour)) {
		t.Fatalf("success bypassed throttle hold: %+v", h)
	}
}

func TestTaskBudgetOutcomePreservesProviderHealth(t *testing.T) {
	now := time.Unix(10000, 0)
	cases := []Health{
		Health{}.ObserveQuota(RateLimited, now, now.Add(10*time.Minute), time.Minute, "cut-off"),
		Health{}.Observe(RateLimited, now),
		Health{}.Observe(Timeout, now),
	}
	for _, h := range cases {
		after := h.Observe(TurnBudgetExhausted, now.Add(time.Minute))
		if after != h {
			t.Fatalf("task budget changed provider health: before %+v after %+v", h, after)
		}
		for _, elapsed := range []time.Duration{time.Minute, 30 * time.Minute, 2 * time.Hour} {
			at := now.Add(elapsed)
			if after.At(at) != h.At(at) || after.Eligible(at) != h.Eligible(at) {
				t.Fatalf("task budget changed future eligibility at %s", elapsed)
			}
		}
	}
}
