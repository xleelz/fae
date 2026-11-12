package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestBridgeRefusalHelper is a bridge that ends its turn the way the mode
// says: a typed refusal (record plus exit status) or a malformed variant.
func TestBridgeRefusalHelper(t *testing.T) {
	if os.Getenv("BRIDGE_REFUSAL_HELPER") != "1" {
		return
	}
	var c BridgeConfig
	raw, e := os.ReadFile(os.Args[len(os.Args)-1])
	if e != nil || json.Unmarshal(raw, &c) != nil {
		os.Exit(11)
	}
	record := func(class string) {
		if RecordBridgeRefusal(c, class, `edit "answer.go" replacements[0]: old text not found`) != nil {
			os.Exit(12)
		}
	}
	file := filepath.Join(c.LogDirectory, BridgeRefusalFile)
	switch os.Getenv("BRIDGE_REFUSAL_MODE") {
	case "output":
		record(RefusedOutput)
	case "local":
		record(RefusedBeforeProvider)
	case "binding":
		record(RefusedBinding)
	case "not-started":
		record(ProviderNotStarted)
	case "provider-failed":
		record("provider-failed")
	case "exit-one":
		record(RefusedOutput)
		os.Exit(1)
	case "no-record":
	case "foreign":
		c.DispatchID = "another-dispatch"
		record(RefusedOutput)
	case "with-handoff":
		record(RefusedOutput)
		b, _ := json.Marshal(Handoff{DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Output: json.RawMessage(`{}`)})
		_ = os.WriteFile(c.Handoff, b, 0600)
	case "unknown-class":
		b, _ := json.Marshal(bridgeRefusalRecord{Protocol: "fae-agent-v1", DispatchID: c.DispatchID, SessionID: c.SessionID, TaskID: c.TaskID, Class: "refused-elsewhere", Detail: "x"})
		_ = os.WriteFile(file, b, 0600)
	case "symlink":
		record(RefusedOutput)
		_ = os.Rename(file, file+".target")
		_ = os.Symlink(file+".target", file)
	default:
		os.Exit(13)
	}
	os.Exit(BridgeRefusalExit)
}

// refusalRun returns one adapter's Run over TestBridgeRefusalHelper in mode.
func refusalRun(t *testing.T, adapter, mode string) (func(context.Context, Launch) (Collected, error), Launch) {
	t.Helper()
	exe, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	point := func(d *Direct) {
		d.Command = []string{exe, "-test.run=^TestBridgeRefusalHelper$", "--", "{config}"}
		d.Environment = map[string]string{"BRIDGE_REFUSAL_HELPER": "1", "BRIDGE_REFUSAL_MODE": mode, "GORACE": "atexit_sleep_ms=0"}
	}
	switch adapter {
	case "direct":
		d, l := directFixture(t, "")
		point(d)
		return d.Run, l
	case "multiplexer":
		m, l := muxFixture(t, "")
		point(m.Direct)
		return m.Run, l
	}
	m, l := residentFixture(t, "")
	point(m.Direct)
	l.SessionPolicy = "task-bound"
	return func(ctx context.Context, l Launch) (Collected, error) {
		out, e := m.Run(ctx, l)
		if out.ProcessesRetired {
			ackTurn(t, m, l.SessionID)
		}
		return out, e
	}, l
}

// FAE-WF-034, FAE-RT-004: a bridge's typed refusal (its record plus the
// refusal exit status) reaches the executor as a TurnError of the recorded
// class under the direct adapter, the Herdr pane and a resident worker. A
// record without the status, the status without a valid record, another
// turn's record, a record beside a handoff, an unknown class or a symlinked
// record stays process-failed.
func TestBridgeRefusalTypedAcrossAdapters(t *testing.T) {
	for _, adapter := range []string{"direct", "multiplexer", "resident"} {
		t.Run(adapter, func(t *testing.T) {
			for mode, class := range map[string]string{"output": RefusedOutput, "local": RefusedBeforeProvider, "binding": RefusedBinding, "not-started": ProviderNotStarted, "provider-failed": "provider-failed", "exit-one": "process-failed", "no-record": "process-failed", "foreign": "process-failed", "with-handoff": "process-failed", "unknown-class": "process-failed", "symlink": "process-failed"} {
				if adapter != "direct" && mode != "output" && mode != "local" && mode != "binding" && mode != "no-record" {
					continue // the record checks are Direct's, whatever the transport
				}
				t.Run(mode, func(t *testing.T) {
					run, l := refusalRun(t, adapter, mode)
					out, e := run(context.Background(), l)
					var turn *TurnError
					if !errors.As(e, &turn) || turn.Class != class || turn.DispatchID != l.DispatchID {
						t.Fatalf("want %s, got %v", class, e)
					}
					if !out.ProcessesRetired || out.NonExecution {
						t.Fatalf("collected %+v", out)
					}
					var refusal *BridgeRefusal
					typed := errors.As(e, &refusal)
					if typed != (class != "process-failed") {
						t.Fatalf("typed refusal %v for %s", typed, class)
					}
					if typed && (refusal.Class != class || refusal.Detail != `edit "answer.go" replacements[0]: old text not found`) {
						t.Fatalf("refusal %+v", refusal)
					}
					// Only a refusal made before the provider was launched
					// is before it: a provider that never acknowledged the
					// prompt or failed without an answer was launched.
					if typed && refusal.BeforeProvider() != (class == RefusedBeforeProvider || class == RefusedBinding) {
						t.Fatalf("refusal %+v before the provider: %v", refusal, refusal.BeforeProvider())
					}
				})
			}
		})
	}
}

// The record is the turn's own: RecordBridgeRefusal needs the identity and an
// absolute log directory, takes a known class, bounds the detail and never
// replaces an existing record.
func TestRecordBridgeRefusalBounds(t *testing.T) {
	dir := t.TempDir()
	c := BridgeConfig{Launch: Launch{DispatchID: "d", SessionID: "s", TaskID: "t"}, LogDirectory: dir}
	if RecordBridgeRefusal(c, "refused-elsewhere", "x") == nil {
		t.Fatal("unknown class recorded")
	}
	if RecordBridgeRefusal(BridgeConfig{Launch: c.Launch, LogDirectory: "relative"}, RefusedOutput, "x") == nil {
		t.Fatal("relative log directory accepted")
	}
	long := make([]byte, 3*maxBridgeRefusalDetail)
	for i := range long {
		long[i] = 'x'
	}
	if e := RecordBridgeRefusal(c, RefusedOutput, string(long)); e != nil {
		t.Fatal(e)
	}
	if RecordBridgeRefusal(c, RefusedBeforeProvider, "second") == nil {
		t.Fatal("existing record replaced")
	}
	raw, e := os.ReadFile(filepath.Join(dir, BridgeRefusalFile))
	var r bridgeRefusalRecord
	if e != nil || json.Unmarshal(raw, &r) != nil || r.Class != RefusedOutput || len(r.Detail) != maxBridgeRefusalDetail {
		t.Fatal(string(raw), e)
	}
}
