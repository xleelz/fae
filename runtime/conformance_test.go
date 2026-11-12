package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

var (
	_ SessionAdapter = (*Scripted)(nil)
	_ SessionAdapter = (*Direct)(nil)
	_ SessionAdapter = (*Multiplexer)(nil)
	_ SessionAdapter = (*ResidentMultiplexer)(nil)
)

// scriptedFixture maps the bridge helper's modes onto scripted turns.
func scriptedFixture(t *testing.T, mode string) (SessionAdapter, Launch) {
	t.Helper()
	step := map[string]Script{
		"":          {Output: json.RawMessage(`{"value":9007199254740993,"note":"$(touch nope); literal"}`)},
		"missing":   {Class: "missing-handoff"},
		"malformed": {Output: json.RawMessage(`{bad`)},
		"wait":      {Delay: time.Hour},
	}[mode]
	_, l := directFixture(t, mode)
	return &Scripted{Root: t.TempDir(), Owner: "scripted-instance", TailBytes: 128, Scripts: map[string][]Script{l.Role: {step}}}, l
}

// FAE-RT-001 acceptance: the scripted, direct and multiplexer adapters pass one
// conformance suite: handoff collection with a bounded tail and log directory,
// typed failures, identity and single-use sessions, live listing with owner and
// readiness, and retirement of a running session's whole process tree.
func TestSessionAdapterConformance(t *testing.T) {
	adapters := map[string]func(*testing.T, string) (SessionAdapter, Launch){
		"scripted": scriptedFixture,
		"direct": func(t *testing.T, mode string) (SessionAdapter, Launch) {
			d, l := directFixture(t, mode)
			return d, l
		},
		"multiplexer": func(t *testing.T, mode string) (SessionAdapter, Launch) {
			m, l := muxFixture(t, mode)
			return m, l
		},
	}
	for name, fixture := range adapters {
		t.Run(name, func(t *testing.T) {
			t.Run("handoff", func(t *testing.T) {
				a, l := fixture(t, "")
				out, err := a.Run(context.Background(), l)
				if err != nil {
					t.Fatal(err)
				}
				if !json.Valid(out.Output) || !strings.Contains(string(out.Output), "9007199254740993") || !strings.Contains(string(out.Output), "$(touch nope); literal") {
					t.Fatalf("handoff %s", out.Output)
				}
				if st, err := os.Stat(out.LogDirectory); err != nil || !st.IsDir() || len(out.Tail) == 0 || len(out.Tail) > 128 || !out.ProcessesRetired || out.NonExecution {
					t.Fatalf("collected %+v %v", out, err)
				}
				if _, err = a.Run(context.Background(), l); err == nil {
					t.Fatal("session identity reused")
				}
				if len(a.Live()) != 0 {
					t.Fatal("finished session still live", a.Live())
				}
			})
			for mode, class := range map[string]string{"missing": "missing-handoff", "malformed": "malformed-output"} {
				t.Run(mode, func(t *testing.T) {
					a, l := fixture(t, mode)
					_, err := a.Run(context.Background(), l)
					var turn *TurnError
					if !errors.As(err, &turn) || turn.Class != class || turn.Role != l.Role || turn.DispatchID != l.DispatchID {
						t.Fatalf("want %s got %v", class, err)
					}
				})
			}
			t.Run("identity", func(t *testing.T) {
				a, l := fixture(t, "")
				l.TaskID = ""
				_, err := a.Run(context.Background(), l)
				var turn *TurnError
				if !errors.As(err, &turn) || turn.Class != "launch-refused" {
					t.Fatal(err)
				}
			})
			t.Run("live-ready-retire", func(t *testing.T) {
				a, l := fixture(t, "wait")
				type ran struct {
					out Collected
					err error
				}
				done := make(chan ran, 1)
				go func() { out, err := a.Run(context.Background(), l); done <- ran{out, err} }()
				deadline := time.Now().Add(30 * time.Second)
				for {
					live := a.Live()
					if len(live) == 1 && live[0].Ready {
						s := live[0]
						if s.ID != l.SessionID || s.Task != l.TaskID || s.Role != l.Role || s.Owner == "" || s.Started.IsZero() {
							t.Fatalf("live session %+v", s)
						}
						break
					}
					if time.Now().After(deadline) {
						t.Fatalf("session never ready: %+v", live)
					}
					time.Sleep(20 * time.Millisecond)
				}
				if err := a.Retire(context.Background(), "no-such-session"); !errors.Is(err, ErrUnknownSession) {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if err := a.Retire(ctx, l.SessionID); err != nil {
					t.Fatal(err)
				}
				r := <-done
				if r.err == nil || !r.out.ProcessesRetired {
					t.Fatalf("retired session %+v %v", r.out, r.err)
				}
				if len(a.Live()) != 0 {
					t.Fatal("retired session still live")
				}
			})
		})
	}
}
