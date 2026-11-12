package providerhealth

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPenaltiesAndDecay(t *testing.T) {
	now := time.Unix(10000, 0)
	for _, tc := range []struct {
		o     Outcome
		score float64
	}{{OK, 100}, {TurnBudgetExhausted, 100}, {RateLimited, 40}, {Unavailable, 0}, {AuthFailure, 0}, {Timeout, 60}, {ModelAPIError, 60}, {MalformedOutput, 60}} {
		t.Run(string(tc.o), func(t *testing.T) {
			h := Health{}.Observe(tc.o, now)
			if h.Score != tc.score {
				t.Fatal(h)
			}
			want := 100 - (100-tc.score)/2
			if math.Abs(h.At(now.Add(time.Hour))-want) > 1e-9 {
				t.Fatal(h)
			}
			if h.Eligible(now) != (tc.score >= 50) {
				t.Fatal(h)
			}
		})
	}
	h := Health{}.Observe(RateLimited, now).Observe(OK, now.Add(time.Second))
	if h.Score > 41 || h.Failures != 1 {
		t.Fatal("hold bypass", h)
	}
	h = h.Observe(OK, now.Add(time.Hour))
	if h.Score != 100 {
		t.Fatal("hold expiry", h)
	}
	h = Health{}.Observe(Unavailable, now).Observe(Timeout, now)
	if h.Score != 0 {
		t.Fatal("negative score", h)
	}
	if h.At(now.Add(-time.Hour)) != 0 {
		t.Fatal("clock rollback")
	}
	if !Eligible(50000) || Eligible(49999) || Eligible(100001) || Eligible(-1) {
		t.Fatal("threshold")
	}
}
func TestStructuredLogClassifier(t *testing.T) {
	cases := []struct {
		raw  string
		want Outcome
	}{
		{`{"type":"error","message":"HTTP 429"}`, RateLimited},
		{`{"type":"turn.failed","error":{"message":"model temporarily unavailable","status":503}}`, Unavailable},
		{`{"level":"error","error":{"code":"invalid_api_key"}}`, AuthFailure},
		{`{"type":"error","message":"experiencing high traffic"}`, RateLimited},
		{`{"type":"error","message":"unexpected provider failure"}`, ModelAPIError},
		{`{"type":"error","error":{"code":"timeout"}}`, Timeout},
		{`{"type":"item.completed","item":{"text":"HTTP 503 authentication rate-limit"}}`, OK},
		{`{"output":{"review":"429 unavailable"}}`, OK},
		{`HTTP 503 temporarily unavailable`, OK},
		{`{broken`, OK},
		// Claude Code stream-json records.
		{`{"type":"assistant","error":"authentication_failed","message":{"content":[{"type":"text","text":"Not logged in"}]}}`, AuthFailure},
		{`{"type":"assistant","error":"rate_limit"}`, RateLimited},
		{`{"type":"assistant","error":"server_error"}`, Unavailable},
		{`{"type":"assistant","error":"billing_error"}`, AuthFailure},
		{`{"type":"assistant","error":"invalid_request"}`, ModelAPIError},
		{`{"type":"assistant","error":null,"message":{"content":[{"type":"text","text":"HTTP 429 rate_limit 503"}]}}`, OK},
		{`{"type":"result","subtype":"success","is_error":true,"result":"Not logged in · Please run /login","terminal_reason":"api_error"}`, AuthFailure},
		{`{"type":"result","subtype":"success","is_error":true,"result":"API Error: 529 overloaded_error","api_error_status":529}`, Unavailable},
		{`{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached"}`, RateLimited},
		{`{"type":"result","subtype":"error_during_execution","is_error":true,"result":""}`, ModelAPIError},
		{`{"type":"result","subtype":"success","is_error":false,"result":"429 authentication unavailable","structured_output":{"note":"503"}}`, OK},
		{`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","overageStatus":"rejected"}}`, OK},
		{`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour"}}`, RateLimited},
	}
	for _, tc := range cases {
		if got := ReadLog(strings.NewReader(tc.raw)); got != tc.want {
			t.Fatalf("%s: %s != %s", tc.raw, got, tc.want)
		}
	}
}
func TestSessionDirectoryScopeAndClassification(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "provider-123")
	os.Mkdir(child, 0700)
	os.WriteFile(filepath.Join(root, "answer.json"), []byte(`{"type":"error","message":"503"}`), 0600)
	if SessionLogs(root) != OK {
		t.Fatal("read work output")
	}
	os.WriteFile(filepath.Join(child, "events.jsonl"), []byte(`{"type":"error","message":"429"}`), 0600)
	if Classify(root, false, false) != RateLimited || Classify(root, true, false) != RateLimited {
		t.Fatal("classification")
	}
	if Classify("", false, true) != Timeout || Classify("", false, false) != MalformedOutput {
		t.Fatal("fallback")
	}
	os.Remove(filepath.Join(child, "events.jsonl"))
	os.Symlink(filepath.Join(root, "answer.json"), filepath.Join(child, "events.jsonl"))
	if SessionLogs(root) != OK {
		t.Fatal("followed symlink")
	}
}

func TestRateLimitedExponentialBackoffEligibility(t *testing.T) {
	base := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	h := Health{Provider: "anthropic", Model: "claude", Score: 100}
	now := base
	for i, d := range []time.Duration{
		1 * time.Minute,
		2 * time.Minute,
		4 * time.Minute,
		8 * time.Minute,
		16 * time.Minute,
		32 * time.Minute,
		60 * time.Minute,
	} {
		h = h.Observe(RateLimited, now)
		if h.Eligible(now) {
			t.Fatalf("step %d: expected ineligible immediately at %v", i, now)
		}
		if h.Eligible(now.Add(d - time.Second)) {
			t.Fatalf("step %d: expected ineligible 1s before probe at %v", i, now.Add(d-time.Second))
		}
		probeAt := now.Add(d)
		if h.Eligible(probeAt) != (h.At(probeAt) >= 50) {
			t.Fatalf("step %d: due probe must respect score >=50, got score %v eligible %v", i, h.At(probeAt), h.Eligible(probeAt))
		}
		now = probeAt
	}
}

func TestClaudeAssistantRateLimitExtractsResetTime(t *testing.T) {
	wantReset := time.Date(2026, 11, 5, 16, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		raw  string
	}{
		{
			name: "message_text_rfc3339",
			raw:  `{"type":"assistant","error":"rate_limit","message":{"content":[{"type":"text","text":"You've hit your usage limit. Resets at 2026-11-05T16:00:00Z"}]}}`,
		},
		{
			name: "top_level_resets_at",
			raw:  `{"type":"assistant","error":"rate_limit","resets_at":"2026-11-05T16:00:00Z"}`,
		},
		{
			name: "nested_rate_limit_info",
			raw:  `{"type":"assistant","error":"rate_limit","rate_limit_info":{"resets_at":"2026-11-05T16:00:00Z"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			detail := ReadLogDetailed(strings.NewReader(tc.raw))
			if detail.Outcome != RateLimited {
				t.Fatalf("Outcome = %s, want %s", detail.Outcome, RateLimited)
			}
			if !detail.ResetAt.Equal(wantReset) {
				t.Fatalf("ResetAt = %v, want %v", detail.ResetAt, wantReset)
			}
		})
	}
}
