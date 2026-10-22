package runtime

import (
	"context"
	"errors"
	"github.com/xleelz/fae/processowner"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func residentFixture(t *testing.T, mode string) (*ResidentMultiplexer, Launch) {
	t.Helper()
	mux, l := muxFixture(t, mode)
	idle, e := NewOwnedIdleSessions(filepath.Join(mux.Direct.Root, "idle"), mux.Direct.Root)
	if e != nil {
		t.Fatal(e)
	}
	m := &ResidentMultiplexer{Direct: mux.Direct, Binary: mux.Binary, Instance: mux.Instance, Idle: idle}
	// Standalone fixture simulates the caller's durable resident-lease checkpoint.
	birth, e := processowner.Birth(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	m.AfterStop = func(_ context.Context, id string) error {
		return processowner.AcknowledgeLinkedRetirement(filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, id, os.Getpid(), birth)
	}
	t.Cleanup(func() {
		if e := m.Close(context.Background()); e != nil {
			t.Error(e)
		}
	})
	return m, l
}
func TestResidentTwoTurnsSamePaneFreshConfig(t *testing.T) {
	m, l := residentFixture(t, "")
	starts, stops := 0, 0
	m.BeforeStart = func(context.Context, string, string) error { starts++; return nil }
	ackResident := m.AfterStop
	m.AfterStop = func(ctx context.Context, id string) error { stops++; return ackResident(ctx, id) }
	first, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	if !first.ProcessesRetired {
		t.Fatal("turn process not retired")
	}
	ackResidentTurn(t, m, l.SessionID)
	entry := m.entries[residentKey(l)]
	pid := entry.records[1].PID
	l.SessionID += "-second"
	l.DispatchID += "-second"
	l.Brief = []byte(`{"value":"new brief"}`)
	second, e := m.Run(context.Background(), l)
	if e != nil {
		t.Fatal(e)
	}
	if !second.ProcessesRetired || first.LogDirectory == second.LogDirectory || m.entries[residentKey(l)].records[1].PID != pid || starts != 1 {
		t.Fatal("resident/config identity mismatch")
	}
	ackResidentTurn(t, m, l.SessionID)
	records, _ := filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.json"))
	if len(records) != 2 {
		t.Fatalf("expected only resident records, got %v", records)
	}
	if _, e = m.Run(context.Background(), l); e == nil {
		t.Fatal("repeated identity accepted")
	}
	if e = m.Close(context.Background()); e != nil {
		t.Fatal(e)
	}
	if stops != 1 {
		t.Fatal(stops)
	}
	records, _ = filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.json*"))
	if len(records) != 0 {
		t.Fatal(records)
	}
}
func TestResidentTaskIsolationAndExpiry(t *testing.T) {
	m, l := residentFixture(t, "")
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	first := m.entries[residentKey(l)]
	l.TaskID += "-other"
	l.DispatchID += "-other"
	l.SessionID += "-other"
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	if len(m.entries) != 2 || m.entries[residentKey(l)].records[1].PID == first.records[1].PID {
		t.Fatal("task isolation lost")
	}
	m.Idle.TTL = time.Nanosecond
	counts := map[string]int{}
	for _, entry := range m.entries {
		counts[entry.socket] = 1
	}
	actions, e := m.Idle.Round(context.Background(), counts, 24, 0, time.Now().Add(time.Second))
	if e != nil || len(actions) != 1 {
		t.Fatalf("%v %v", actions, e)
	}
	for _, entry := range m.entries {
		if entry.closed.Load() {
			if _, e = os.Stat(entry.socket); !os.IsNotExist(e) {
				t.Fatal("expired socket retained")
			}
		}
	}
}
func TestResidentInterruptedTurnRetiresOnlyTurn(t *testing.T) {
	m, l := residentFixture(t, "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() {
		for {
			records, _ := filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.json"))
			if len(records) >= 4 {
				time.Sleep(100 * time.Millisecond)
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}()
	out, e := m.Run(ctx, l)
	if e == nil {
		t.Fatal("timeout succeeded")
	}
	if !out.ProcessesRetired {
		t.Fatalf("turn retirement unproven: %v", e)
	}
	if m.Idle.Count() != 1 {
		t.Fatal("resident lost")
	}
}
func TestCanReuseResident(t *testing.T) {
	for _, identity := range []bool{false, true} {
		for _, live := range []bool{false, true} {
			for _, fresh := range []bool{false, true} {
				if got := CanReuseResident(identity, live, fresh); got != (identity && live && !fresh) {
					t.Fatal(identity, live, fresh)
				}
			}
		}
	}
}
func TestResidentFreshContextUsesOneShot(t *testing.T) {
	m, l := residentFixture(t, "")
	l.FreshContext = true
	oneshot := 0
	m.OneShotBeforeStart = func(context.Context, string, string) error { oneshot++; return nil }
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	if oneshot != 1 || m.Idle.Count() != 0 || len(m.entries) != 0 {
		t.Fatal("fresh context pooled")
	}
}
func TestResidentMissingBridgeRetainsSafeIdleWorker(t *testing.T) {
	m, l := residentFixture(t, "")
	m.Direct.Command = []string{"/nonexistent/fae-bridge", "{config}"}
	result, e := m.Run(context.Background(), l)
	if e == nil || !result.NonExecution || !result.ProcessesRetired {
		t.Fatalf("%+v %v", result, e)
	}
	if m.Idle.Count() != 1 {
		t.Fatal("resident not retained")
	}
}
func TestResidentStartupFailureReleasesReservation(t *testing.T) {
	m, l := residentFixture(t, "")
	m.Binary = "/nonexistent/herdr"
	m.Direct.StartTimeout = 100 * time.Millisecond
	starts, stops := 0, 0
	m.BeforeStart = func(context.Context, string, string) error { starts++; return nil }
	m.AfterStop = func(context.Context, string) error { stops++; return nil }
	if _, e := m.Run(context.Background(), l); e == nil {
		t.Fatal("missing executable succeeded")
	}
	if starts != 1 || stops != 1 || m.Idle.Count() != 0 {
		t.Fatalf("startup lease leaked %d %d", starts, stops)
	}
}
func TestResidentGlobalReuseAcrossTasks(t *testing.T) {
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	id := residentKey(l)
	l.TaskID += "-next"
	l.SessionID += "-next"
	l.DispatchID += "-next"
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	if residentKey(l) != id || len(m.entries) != 1 {
		t.Fatal("workflow-global transport not shared")
	}
}
func TestResidentDistinctTasksRunConcurrently(t *testing.T) {
	m, l := residentFixture(t, "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 2)
	go func() { _, e := m.Run(ctx, l); done <- e }()
	other := l
	other.TaskID += "-two"
	other.SessionID += "-two"
	other.DispatchID += "-two"
	go func() { _, e := m.Run(ctx, other); done <- e }()
	for len(m.Sessions()) < 2 {
		select {
		case <-ctx.Done():
			t.Fatal("distinct resident turns serialized")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	for i := 0; i < 2; i++ {
		if e := <-done; e == nil {
			t.Fatal("cancelled turn succeeded")
		}
	}
}
func TestResidentOverlappingSameKeyRefused(t *testing.T) {
	m, l := residentFixture(t, "wait")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := m.Run(ctx, l); done <- e }()
	for len(m.Sessions()) < 1 {
		select {
		case <-ctx.Done():
			t.Fatal("turn failed to start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	second := l
	second.SessionID += "-overlap"
	second.DispatchID += "-overlap"
	result, e := m.Run(ctx, second)
	if e == nil || !result.NonExecution || !result.ProcessesRetired {
		t.Fatalf("overlap accepted or retirement wrong: %+v %v", result, e)
	}
	var deferred interface{ AdmissionDeferred() bool }
	if !errors.As(e, &deferred) || !deferred.AdmissionDeferred() {
		t.Fatalf("active overlap should defer admission: %v", e)
	}
	cancel()
	<-done
	if len(m.entries) != 1 {
		t.Fatal("overlap replaced resident")
	}
}

func TestResidentDeadIdentityDoesNotLeakNewTurn(t *testing.T) {
	m, l := residentFixture(t, "")
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	entry := m.entries[residentKey(l)]
	if e := processowner.Terminate(context.Background(), entry.records[1], m.Direct.Root); e != nil {
		t.Fatal(e)
	}
	l.SessionID += "-next"
	l.DispatchID += "-next"
	result, e := m.Run(context.Background(), l)
	if e == nil || !result.NonExecution || !result.ProcessesRetired {
		t.Fatalf("%+v %v", result, e)
	}
	if len(m.Sessions()) != 0 {
		t.Fatal("phantom dispatch retained")
	}
}

// The transport deliberately retains terminal receipts until its caller has
// durably journaled retirement. Simulate that acknowledgement in runtime tests.
func ackResidentTurn(t *testing.T, m *ResidentMultiplexer, id string) {
	t.Helper()
	birth, e := processowner.Birth(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	records, e := filepath.Glob(filepath.Join(m.Direct.Root, ".processes", "*.retired"))
	if e != nil || len(records) < 2 {
		t.Fatalf("turn retirement receipts missing: %v %v", records, e)
	}
	if e = processowner.AcknowledgeLinkedRetirement(filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, id, os.Getpid(), birth); e != nil {
		t.Fatal(e)
	}
}
