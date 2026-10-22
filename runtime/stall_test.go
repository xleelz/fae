package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStallSignals(t *testing.T) {
	old := activity{cpu: time.Second, events: 2, handoff: 3, known: true}
	for _, a := range []activity{{cpu: 2 * time.Second}, {events: 3}, {handoff: 4}} {
		if !a.progressed(old) {
			t.Fatal("positive signal ignored")
		}
	}
	if old.progressed(old) {
		t.Fatal("silence is not progress")
	}
	for _, tt := range []struct {
		s    string
		want time.Duration
	}{{"01:02.50", 62500 * time.Millisecond}, {"1-02:03:04", 93784 * time.Second}, {"00:00.01", 10 * time.Millisecond}} {
		d, e := cpuDuration(tt.s)
		if e != nil || d != tt.want {
			t.Fatal(tt, d, e)
		}
	}
	if _, e := cpuDuration("bad"); e == nil {
		t.Fatal("invalid CPU accepted")
	}
}
func TestStallActivityUsesStructuredEvents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "events.jsonl")
	h := filepath.Join(dir, "handoff.json")
	os.WriteFile(p, []byte("terminal noise\n{\"type\":\"item.started\"}\n"), 0600)
	os.WriteFile(h, []byte("abc"), 0600)
	a := sampleActivity(context.Background(), os.Getpid(), dir, h)
	if !a.known || a.events != 1 || a.handoff != 3 {
		t.Fatal(a)
	}
	if _, ok := processCPU(context.Background(), -1); ok {
		t.Fatal("missing process known")
	}
}
func TestSilentStalledBridgeRetires(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	exe, _ := os.Executable()
	d := &Direct{Root: root, Command: []string{exe, "-test.run=^TestBridgeHelper$", "--", "{config}"}, Environment: map[string]string{"BRIDGE_HELPER": "1", "BRIDGE_MODE": "wait"}, Timeout: 10 * time.Second, StallTimeout: 300 * time.Millisecond, StallPollInterval: 50 * time.Millisecond}
	out, e := d.Run(context.Background(), Launch{DispatchID: "stall", SessionID: "stall", TaskID: "task", Role: "role", Worktree: root, Mode: "local", Brief: []byte(`{}`)})
	var turn *TurnError
	if !errors.As(e, &turn) || turn.Class != "stalled" || !out.ProcessesRetired || len(d.Sessions()) != 0 {
		t.Fatalf("%+v %v", out, e)
	}
}
func TestSilentBusyBridgeNotStalled(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	exe, _ := os.Executable()
	d := &Direct{Root: root, Command: []string{exe, "-test.run=^TestStallBusyHelper$", "--", "{config}"}, Environment: map[string]string{"STALL_BUSY": "1"}, Timeout: 2 * time.Second, StallTimeout: 600 * time.Millisecond, StallPollInterval: 100 * time.Millisecond}
	_, e := d.Run(context.Background(), Launch{DispatchID: "busy", SessionID: "busy", TaskID: "task", Role: "role", Worktree: root, Mode: "local", Brief: []byte(`{}`)})
	var turn *TurnError
	if !errors.As(e, &turn) || turn.Class != "interrupted" {
		t.Fatal(e)
	}
}
func TestStallBusyHelper(t *testing.T) {
	if os.Getenv("STALL_BUSY") != "1" {
		return
	}
	for {
		_ = time.Now()
	}
}

func TestStructuredAndHandoffActivityPreventStall(t *testing.T) {
	for _, mode := range []string{"events", "handoff"} {
		t.Run(mode, func(t *testing.T) {
			root, _ := filepath.EvalSymlinks(t.TempDir())
			exe, _ := os.Executable()
			d := &Direct{Root: root, Command: []string{exe, "-test.run=^TestStallActivityHelper$", "--", "{config}"}, Environment: map[string]string{"STALL_ACTIVITY": mode}, Timeout: 2 * time.Second, StallTimeout: 600 * time.Millisecond, StallPollInterval: 100 * time.Millisecond}
			_, e := d.Run(context.Background(), Launch{DispatchID: mode, SessionID: mode, TaskID: "task", Role: "role", Worktree: root, Mode: "local", Brief: []byte(`{}`)})
			var turn *TurnError
			if !errors.As(e, &turn) || turn.Class != "interrupted" {
				t.Fatal(e)
			}
		})
	}
}
func TestStallActivityHelper(t *testing.T) {
	mode := os.Getenv("STALL_ACTIVITY")
	if mode == "" {
		return
	}
	var c BridgeConfig
	b, _ := os.ReadFile(os.Args[len(os.Args)-1])
	if json.Unmarshal(b, &c) != nil {
		os.Exit(2)
	}
	p := c.Handoff
	data := "x"
	if mode == "events" {
		p = filepath.Join(c.LogDirectory, "events.jsonl")
		data = "{\"type\":\"item.started\"}\n"
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		os.Exit(3)
	}
	for {
		_, _ = f.WriteString(data)
		time.Sleep(100 * time.Millisecond)
	}
}
func TestUnknownProcessCannotAuthorizeStall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	done := make(chan bool, 1)
	go monitorStall(ctx, func() int { return -1 }, t.TempDir(), "", 50*time.Millisecond, 20*time.Millisecond, cancel, done)
	if <-done {
		t.Fatal("unknown census authorized cancellation")
	}
}
