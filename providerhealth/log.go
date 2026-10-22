package providerhealth

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ReadLog consumes bounded JSONL. Only top-level error events and their error
// objects are diagnostic; successful payloads/messages are never searched.
// Plain text is deliberately ignored: it can be echoed work or a prompt.
func ReadLog(r io.Reader) Outcome {
	scan := bufio.NewScanner(io.LimitReader(r, 1<<20))
	scan.Buffer(make([]byte, 4096), 1<<20)
	outcome := OK
	for scan.Scan() {
		var v map[string]json.RawMessage
		if json.Unmarshal(scan.Bytes(), &v) != nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(v["type"], &typ)
		var level string
		_ = json.Unmarshal(v["level"], &level)
		if typ != "error" && typ != "turn.failed" && typ != "response.failed" && level != "error" {
			continue
		}
		raw := v["error"]
		if len(raw) == 0 {
			raw = scan.Bytes()
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
		case strings.Contains(text, "429"), strings.Contains(text, "throttl"), strings.Contains(text, "rate_limit"), strings.Contains(text, "high traffic"):
			o = RateLimited
		case strings.Contains(text, "503"), strings.Contains(text, "temporarily unavailable"):
			o = Unavailable
		case strings.Contains(text, "401"), strings.Contains(text, "403"), strings.Contains(text, "authentication"), strings.Contains(text, "unauthorized"), strings.Contains(text, "invalid_api_key"):
			o = AuthFailure
		}
		if rank(o) > rank(outcome) {
			outcome = o
		}
	}
	return outcome
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
	if dir == "" {
		return OK
	}
	paths := sessionPaths(dir)
	result := OK
	for _, path := range paths {
		st, e := os.Lstat(path)
		if e != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
			continue
		}
		f, e := os.Open(path)
		if e != nil {
			continue
		}
		o := ReadLog(f)
		f.Close()
		if rank(o) > rank(result) {
			result = o
		}
	}
	return result
}
func Classify(dir string, validOutput, timedOut bool) Outcome {
	// Genuine structured error events remain observations even if the provider
	// recovered and produced an output. Work payload text is never searched.
	if o := SessionLogs(dir); o != OK {
		return o
	}
	if validOutput {
		return OK
	}
	if timedOut {
		return Timeout
	}
	return MalformedOutput
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
