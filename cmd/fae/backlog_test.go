package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBacklogCLIValidation(t *testing.T) {
	for _, args := range [][]string{{"backlog"}, {"backlog", "edge"}, {"backlog", "edge", "bad"}, {"backlog", "admit"}, {"backlog", "admit", "--authorizing-message", "m"}} {
		if err := execute(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatal(args)
		}
	}
	for _, content := range []string{`{"unknown":1}`, `{} {}`} {
		path := filepath.Join(t.TempDir(), "payload.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := execute(context.Background(), []string{"backlog", "admit", "--file", path, "--authorizing-message", "m"}, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid accepted", content)
		}
	}
}

// FAE-CLI-014: the payload file is read only up to the daemon's 64 KiB bound.
func TestBacklogAdmitPayloadIsBounded(t *testing.T) {
	home, requests := recordingDaemon(t, "bl", map[string]any{"admitted": 1})
	path := filepath.Join(t.TempDir(), "big.json")
	if err := os.WriteFile(path, append([]byte(`{"items":[],"edges":[],"pad":"`), bytes.Repeat([]byte("x"), 64<<10)...), 0600); err != nil {
		t.Fatal(err)
	}
	err := execute(context.Background(), []string{"backlog", "admit", "--file", path, "--home", home, "--instance", "bl", "--authorizing-message", "m"}, &bytes.Buffer{})
	if exitCode(err) != 1 || err == nil || !bytes.Contains([]byte(err.Error()), []byte("exceeds 65536 bytes")) {
		t.Fatal(err)
	}
	select {
	case r := <-requests:
		t.Fatal("oversized payload reached the daemon", r.Method)
	default:
	}
}

// FAE-CLI-015: --accept-in-flight is an edge add input only. Help names it,
// and every other backlog action refuses it, and any flag not its own, as a
// usage error that reaches no daemon.
func TestBacklogAcceptInFlightOnlyOnEdgeAdd(t *testing.T) {
	home, requests := recordingDaemon(t, "bl", map[string]any{"ok": true})
	run := func(args ...string) (int, string) {
		var out bytes.Buffer
		if err := execute(context.Background(), append([]string{"--home", home, "--instance", "bl"}, args...), &out); err != nil {
			return exitCode(err), out.String()
		}
		return 0, out.String()
	}
	for _, args := range [][]string{{"backlog", "--help"}, {"backlog", "edge", "--help"}, {"backlog", "edge", "add", "--help"}, {"help"}} {
		if code, out := run(args...); code != 0 || !strings.Contains(out, "accept-in-flight") {
			t.Errorf("%v: help exit %d does not name --accept-in-flight: %s", args, code, out)
		}
	}
	if code, out := run("backlog", "edge", "remove", "--help"); code != 0 || strings.Contains(out, "accept-in-flight") {
		t.Errorf("edge remove help exit %d names --accept-in-flight: %s", code, out)
	}
	if code, _ := run("backlog", "edge", "add", "--from", "a", "--to", "b", "--accept-in-flight", "--authorizing-message", "m"); code != 0 {
		t.Fatalf("edge add --accept-in-flight exit %d", code)
	}
	if r := <-requests; r.Method != "BacklogEdge" || params(t, r)["accept_in_flight"] != true || params(t, r)["remove"] != nil {
		t.Fatalf("edge add reached %s with %v", r.Method, params(t, r))
	}
	for _, args := range [][]string{
		{"backlog", "edge", "remove", "--from", "a", "--to", "b", "--accept-in-flight"},
		{"backlog", "admit", "--file", "x.json", "--accept-in-flight"},
		{"backlog", "show", "--accept-in-flight"},
		{"backlog", "show", "--file", "x.json"},
		{"backlog", "admit", "--file", "x.json", "--from", "a"},
		{"backlog", "edge", "add", "--from", "a", "--to", "b", "--state", "Done"},
	} {
		if code, _ := run(append(args, "--authorizing-message", "m")...); code != 2 {
			t.Errorf("%v: exit %d, want usage 2", args, code)
		}
	}
	select {
	case r := <-requests:
		t.Fatalf("a usage error reached the daemon: %s %v", r.Method, params(t, r))
	default:
	}
}

// FAE-CLI-001, FAE-CLI-014, FAE-CLI-015: a backlog action missing its
// operand flags (edge --from and --to, admit --file) or given an unknown
// --kind is a usage error (2), before the citation check and without
// dialling the daemon, whether one is running or not.
func TestBacklogMissingOperandsAreUsageErrors(t *testing.T) {
	home, requests := recordingDaemon(t, "bl", map[string]any{"ok": true})
	empty := shortHome(t)
	for _, args := range [][]string{
		{"backlog", "edge", "add", "--to", "b", "--authorizing-message", "m"},
		{"backlog", "edge", "add", "--from", "a", "--authorizing-message", "m"},
		{"backlog", "edge", "remove", "--from", "a", "--authorizing-message", "m"},
		{"backlog", "edge", "remove", "--authorizing-message", "m"},
		{"backlog", "edge", "add", "--from", "a"},
		{"backlog", "edge", "add", "--from", "a", "--to", "b", "--kind", "bogus", "--authorizing-message", "m"},
		{"backlog", "admit", "--authorizing-message", "m"},
		{"backlog", "admit"},
	} {
		for _, at := range []string{home, empty} {
			err := execute(context.Background(), append([]string{"--home", at, "--instance", "bl"}, args...), &bytes.Buffer{})
			if exitCode(err) != 2 {
				t.Errorf("%v (home %s): exit %d %v, want usage 2", args, at, exitCode(err), err)
			}
		}
	}
	select {
	case r := <-requests:
		t.Fatalf("a usage error reached the daemon: %s", r.Method)
	default:
	}
	// With its operands, an uncited edge is still a refusal (1).
	if err := execute(context.Background(), []string{"--home", home, "--instance", "bl", "backlog", "edge", "add", "--from", "a", "--to", "b"}, &bytes.Buffer{}); exitCode(err) != 1 {
		t.Fatalf("uncited edge: exit %d %v", exitCode(err), err)
	}
}
