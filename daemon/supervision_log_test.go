package daemon

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func init() { supervisionStderr = io.Discard } // tests read the file; serve copies to stderr

var supervisionLine = regexp.MustCompile(`^(\S+) instance=(\S+) component=(\S+) (.*)$`)

// parseSupervisionLines checks FAE-OBS-001 on every line: an RFC 3339
// timestamp with zone, the instance and a component name.
func parseSupervisionLines(t *testing.T, raw []byte, instance string) [][]string {
	t.Helper()
	var out [][]string
	for _, line := range strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n") {
		m := supervisionLine.FindStringSubmatch(line)
		if m == nil {
			t.Fatalf("unstructured supervision line %q", line)
		}
		if _, err := time.Parse(time.RFC3339Nano, m[1]); err != nil {
			t.Fatalf("line timestamp %q: %v", m[1], err)
		}
		if m[2] != instance || m[3] == "" {
			t.Fatalf("line identity %q", line)
		}
		out = append(out, m[1:])
	}
	return out
}

func TestSupervisionLogPrefixesEveryLineAndCopiesStderr(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supervision.log")
	var stderr bytes.Buffer
	w, err := openSupervisionLog(path, "inst", &stderr)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, "drive rearm refused (%s): %v\n", "cause", "boom")
	fmt.Fprintf(w, "two\nlines")
	dolt := componentStream(w, "dolt")
	io.WriteString(dolt, "par")
	io.WriteString(dolt, "tial\nnext\n")
	fmt.Fprintf(componentLog(w, "admission"), "host lease waiting reason=HeavyLease\n")
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	if n, err := fmt.Fprintf(w, "after close\n"); err != nil || n == 0 {
		t.Fatalf("write after close %d %v", n, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := parseSupervisionLines(t, raw, "inst")
	want := [][2]string{{"daemon", "supervision log opened"}, {"daemon", "drive rearm refused (cause): boom"}, {"daemon", "two"}, {"daemon", "lines"}, {"dolt", "partial"}, {"dolt", "next"}, {"admission", "host lease waiting reason=HeavyLease"}, {"daemon", "supervision log closed"}}
	if len(lines) != len(want) {
		t.Fatalf("lines %q", raw)
	}
	for i, w := range want {
		if lines[i][2] != w[0] || !strings.HasPrefix(lines[i][3], w[1]) {
			t.Fatalf("line %d = %q want %q", i, lines[i], w)
		}
	}
	if stderr.String() != string(raw) {
		t.Fatalf("stderr copy differs:\n%s\n%s", stderr.String(), raw)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("log mode %v %v", info, err)
	}
	if componentLog(&stderr, "x") != io.Writer(&stderr) {
		t.Fatal("foreign writer must pass through")
	}
}

func TestSupervisionLogRotatesPrivately(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supervision.log")
	w, err := openSupervisionLog(path, "inst", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	l := w.(*supervisionLog)
	l.mu.Lock()
	l.limit, l.keep = 400, 2
	l.mu.Unlock()
	for i := 0; i < 40; i++ {
		fmt.Fprintf(w, "line %02d %s\n", i, strings.Repeat("x", 40))
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, p := range []string{path, path + ".1", path + ".2"} {
		info, err := os.Lstat(p)
		if err != nil || info.Mode().Perm() != 0600 || info.Size() > 400 {
			t.Fatalf("%s: %v %v", p, info, err)
		}
		raw, _ := os.ReadFile(p)
		total += len(parseSupervisionLines(t, raw, "inst"))
	}
	if _, err = os.Lstat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("rotation kept too many files: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "line 39") || total < 6 {
		t.Fatalf("newest lines missing (%d): %s", total, raw)
	}
}

func TestSupervisionLogRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "supervision.log")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if w, err := openSupervisionLog(path, "inst", io.Discard); err == nil || w != nil {
		t.Fatal("symlinked supervision log accepted")
	}
}

type blockedWriter struct{ release chan struct{} }

func (b blockedWriter) Write(p []byte) (int, error) { <-b.release; return len(p), nil }

// A stalled standard error (a paused terminal, an unread pipe) must not stall
// the daemon: the file stays complete and the copy drops lines instead.
func TestSupervisionLogStalledStderrNeverBlocks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "supervision.log")
	stalled := blockedWriter{make(chan struct{})}
	defer close(stalled.release)
	w, err := openSupervisionLog(path, "inst", stalled)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3*supervisionStderrQueue; i++ {
			fmt.Fprintf(w, "line %d\n", i)
		}
		w.Close()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("supervision log blocked on standard error")
	}
	raw, _ := os.ReadFile(path)
	lines := parseSupervisionLines(t, raw, "inst")
	if len(lines) != 3*supervisionStderrQueue+2 || !strings.Contains(lines[len(lines)-1][3], "stderr_dropped=") || strings.Contains(lines[len(lines)-1][3], "stderr_dropped=0") {
		t.Fatalf("%d lines, last %q", len(lines), lines[len(lines)-1])
	}
}
