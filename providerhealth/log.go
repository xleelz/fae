package providerhealth

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ObservationDetail carries the classified outcome of a provider session log
// together with any parsed usage-limit reset timestamp T and provider session ID
// (FAE-RT-018, FAE-RT-019).
type ObservationDetail struct {
	Outcome   Outcome   `json:"outcome"`
	ResetAt   time.Time `json:"reset_at,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
}

var (
	rfc3339Pattern    = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})\b`)
	resetEpochPattern = regexp.MustCompile(`(?i)resets?(?:_at|at|\s+at)?["'\s:=]+(\d{10,13})\b`)
	retryAfterPattern = regexp.MustCompile(`(?i)retry[-_ ]after(?:_seconds)?["'\s:=]+(\d+)\b`)
)

// ReadLog consumes bounded JSONL. Only top-level error events and their error
// objects are diagnostic; successful payloads/messages are never searched.
// Plain text is deliberately ignored: it can be echoed work or a prompt.
func ReadLog(r io.Reader) Outcome {
	return ReadLogDetailed(r).Outcome
}

// ReadLogDetailed consumes bounded JSONL and extracts the highest-ranked
// diagnostic Outcome, any usage/rate-limit reset time T, and any provider
// session reference (FAE-RT-018, FAE-RT-019).
func ReadLogDetailed(r io.Reader) ObservationDetail {
	scan := bufio.NewScanner(io.LimitReader(r, 1<<20))
	scan.Buffer(make([]byte, 4096), 1<<20)
	detail := ObservationDetail{Outcome: OK}
	for scan.Scan() {
		var v map[string]json.RawMessage
		if json.Unmarshal(scan.Bytes(), &v) != nil {
			continue
		}
		if sid := extractSessionID(v); sid != "" && detail.SessionID == "" {
			detail.SessionID = sid
		}
		var typ string
		_ = json.Unmarshal(v["type"], &typ)
		var level string
		_ = json.Unmarshal(v["level"], &level)
		var raw json.RawMessage
		switch {
		case typ == "error" || typ == "turn.failed" || typ == "response.failed" || level == "error":
			raw = v["error"]
			if len(raw) == 0 {
				raw = scan.Bytes()
			}
		case typ == "result" || typ == "assistant" || typ == "rate_limit_event":
			if raw = claudeDiagnostic(typ, v); raw == nil {
				continue
			}
		default:
			continue
		}
		var diagnostic any
		if json.Unmarshal(raw, &diagnostic) != nil {
			continue
		}
		text := strings.ToLower(string(raw))
		o := ModelAPIError
		switch {
		case strings.Contains(text, `"timeout"`):
			o = Timeout
		case strings.Contains(text, "429"), strings.Contains(text, "throttl"), strings.Contains(text, "rate_limit"), strings.Contains(text, "high traffic"), strings.Contains(text, "usage limit"), strings.Contains(text, "hit your limit"), strings.Contains(text, "quota exceeded"), strings.Contains(text, "quota exhausted"), strings.Contains(text, "out of extra usage"):
			o = RateLimited
		case strings.Contains(text, "503"), strings.Contains(text, "temporarily unavailable"), strings.Contains(text, "529"), strings.Contains(text, "overloaded"), strings.Contains(text, "server_error"):
			o = Unavailable
		case strings.Contains(text, "401"), strings.Contains(text, "403"), strings.Contains(text, "authentication"), strings.Contains(text, "unauthorized"), strings.Contains(text, "invalid_api_key"), strings.Contains(text, "not logged in"), strings.Contains(text, "billing"):
			o = AuthFailure
		}
		if o == RateLimited {
			if resetAt := extractResetTime(v, raw); !resetAt.IsZero() && (detail.ResetAt.IsZero() || resetAt.After(detail.ResetAt)) {
				detail.ResetAt = resetAt
			}
		}
		if rank(o) > rank(detail.Outcome) {
			detail.Outcome = o
		}
	}
	return detail
}

func extractSessionID(v map[string]json.RawMessage) string {
	for _, key := range []string{"session_id", "sessionId", "conversation_id", "thread_id"} {
		raw, ok := v[key]
		if !ok || len(raw) == 0 {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) == nil && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func extractResetTime(v map[string]json.RawMessage, diagnostic json.RawMessage) time.Time {
	for _, blob := range []json.RawMessage{v["rate_limit_info"], v["rateLimitInfo"], v["error"], diagnostic} {
		if t := parseResetFromJSON(blob); !t.IsZero() {
			return t
		}
	}
	return parseResetFromText(string(diagnostic))
}

func parseResetFromJSON(blob json.RawMessage) time.Time {
	if len(blob) == 0 {
		return time.Time{}
	}
	dec := json.NewDecoder(bytes.NewReader(blob))
	dec.UseNumber()
	var m map[string]any
	if dec.Decode(&m) != nil {
		return time.Time{}
	}
	for _, key := range []string{"resets_at", "resetsAt", "reset_at", "resetAt"} {
		if val, ok := m[key]; ok {
			if t := parseTimestampValue(val); !t.IsZero() {
				return t
			}
		}
	}
	for _, key := range []string{"retry_after_seconds", "retryAfterSeconds", "retry_after", "retryAfter"} {
		if val, ok := m[key]; ok {
			if d := parseDurationSeconds(val); d > 0 {
				return time.Now().UTC().Add(d)
			}
		}
	}
	return time.Time{}
}

func parseTimestampValue(val any) time.Time {
	switch x := val.(type) {
	case string:
		x = strings.TrimSpace(x)
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, x); err == nil {
				return t.UTC()
			}
		}
		if n, err := strconv.ParseInt(x, 10, 64); err == nil {
			return epochToTime(n)
		}
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return epochToTime(n)
		}
		if f, err := x.Float64(); err == nil && f > 1e9 {
			return time.Unix(int64(f), 0).UTC()
		}
	case float64:
		return epochToTime(int64(x))
	}
	return time.Time{}
}

func epochToTime(n int64) time.Time {
	if n > 1e12 {
		return time.UnixMilli(n).UTC()
	}
	if n > 1e9 {
		return time.Unix(n, 0).UTC()
	}
	return time.Time{}
}

func parseDurationSeconds(val any) time.Duration {
	switch x := val.(type) {
	case json.Number:
		if f, err := x.Float64(); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	case float64:
		if x > 0 {
			return time.Duration(x * float64(time.Second))
		}
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil && f > 0 {
			return time.Duration(f * float64(time.Second))
		}
	}
	return 0
}

func parseResetFromText(text string) time.Time {
	if m := rfc3339Pattern.FindString(text); m != "" {
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if t, err := time.Parse(layout, m); err == nil {
				return t.UTC()
			}
		}
	}
	if m := resetEpochPattern.FindStringSubmatch(text); len(m) == 2 {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
			if t := epochToTime(n); !t.IsZero() {
				return t
			}
		}
	}
	if m := retryAfterPattern.FindStringSubmatch(text); len(m) == 2 {
		if n, err := strconv.ParseInt(m[1], 10, 64); err == nil && n > 0 {
			return time.Now().UTC().Add(time.Duration(n) * time.Second)
		}
	}
	return time.Time{}
}

// claudeDiagnostic extracts Claude Code stream-json failures: a terminal result
// flagged is_error, an API-error record's top-level error code, or a rejected
// rate-limit event. Message content and successful results are never read.
func claudeDiagnostic(typ string, v map[string]json.RawMessage) json.RawMessage {
	switch typ {
	case "result":
		var failed bool
		if json.Unmarshal(v["is_error"], &failed) != nil || !failed {
			return nil
		}
		raw, _ := json.Marshal(map[string]json.RawMessage{"subtype": v["subtype"], "result": v["result"], "api_error_status": v["api_error_status"], "terminal_reason": v["terminal_reason"]})
		return raw
	case "assistant":
		var code string
		if json.Unmarshal(v["error"], &code) != nil || code == "" {
			return nil
		}
		m := map[string]any{"code": code}
		if len(v["message"]) > 0 {
			m["message"] = v["message"]
		}
		if len(v["rate_limit_info"]) > 0 {
			m["rate_limit_info"] = v["rate_limit_info"]
		}
		if len(v["resets_at"]) > 0 {
			m["resets_at"] = v["resets_at"]
		}
		raw, _ := json.Marshal(m)
		return raw
	case "rate_limit_event":
		var info struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(v["rate_limit_info"], &info) != nil || info.Status != "rejected" {
			return nil
		}
		raw, _ := json.Marshal(map[string]any{"code": "rate_limit", "status": "rejected", "rate_limit_info": v["rate_limit_info"]})
		return raw
	}
	return nil
}
func rank(o Outcome) int {
	switch o {
	case AuthFailure:
		return 4
	case Unavailable:
		return 3
	case RateLimited:
		return 2
	case ModelAPIError, Timeout:
		return 1
	}
	return 0
}

// SessionLogs reads only known event files in the fresh, runtime-owned launch
// directory. A bridge may use events.jsonl directly or a provider-* child.
// It never searches user-global log directories, follows symlinks, or reads
// answer.json (work content). The runtime creates this directory at launch;
// this provides timestamp-scoped discovery even without a CLI announcement.
func SessionLogs(dir string) Outcome {
	return SessionLogsDetailed(dir).Outcome
}

// SessionLogsDetailed reads only known event files in the launch directory and
// returns the highest-ranked Outcome, any parsed reset time T, and any session ID.
func SessionLogsDetailed(dir string) ObservationDetail {
	if dir == "" {
		return ObservationDetail{Outcome: OK}
	}
	paths := sessionPaths(dir)
	result := ObservationDetail{Outcome: OK}
	for _, path := range paths {
		st, e := os.Lstat(path)
		if e != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
			continue
		}
		f, e := os.Open(path)
		if e != nil {
			continue
		}
		d := ReadLogDetailed(f)
		f.Close()
		if d.SessionID != "" && result.SessionID == "" {
			result.SessionID = d.SessionID
		}
		if !d.ResetAt.IsZero() && (result.ResetAt.IsZero() || d.ResetAt.After(result.ResetAt)) {
			result.ResetAt = d.ResetAt
		}
		if rank(d.Outcome) > rank(result.Outcome) {
			result.Outcome = d.Outcome
		}
	}
	return result
}

func Classify(dir string, validOutput, timedOut bool) Outcome {
	return ClassifyDetailed(dir, validOutput, timedOut).Outcome
}

// ClassifyDetailed returns the classified ObservationDetail for a session launch
// directory, preserving ResetAt and SessionID when present.
func ClassifyDetailed(dir string, validOutput, timedOut bool) ObservationDetail {
	// Genuine structured error events remain observations even if the provider
	// recovered and produced an output. Work payload text is never searched.
	d := SessionLogsDetailed(dir)
	if d.Outcome != OK {
		return d
	}
	if validOutput {
		d.Outcome = OK
		return d
	}
	if timedOut {
		d.Outcome = Timeout
		return d
	}
	d.Outcome = MalformedOutput
	return d
}

// sessionPaths is shared by health classification and positive activity detection.
func sessionPaths(dir string) []string {
	paths := []string{filepath.Join(dir, "events.jsonl")}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "provider-") {
			paths = append(paths, filepath.Join(dir, e.Name(), "events.jsonl"))
		}
	}
	return paths
}

// ActivityEvents counts complete structured activity events in the same bounded
// session-log reader used for health classification. Plain terminal text is ignored.
func ActivityEvents(dir string) int64 {
	var count int64
	for _, path := range sessionPaths(dir) {
		st, e := os.Lstat(path)
		if e != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
			continue
		}
		f, e := os.Open(path)
		if e != nil {
			continue
		}
		scan := bufio.NewScanner(io.LimitReader(f, 1<<20))
		scan.Buffer(make([]byte, 4096), 1<<20)
		for scan.Scan() {
			var v struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(scan.Bytes(), &v) == nil && v.Type != "" {
				count++
			}
		}
		f.Close()
	}
	return count
}
