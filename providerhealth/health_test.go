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
	}{{OK, 100}, {RateLimited, 40}, {Unavailable, 0}, {AuthFailure, 0}, {Timeout, 60}, {ModelAPIError, 60}, {MalformedOutput, 60}} {
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
