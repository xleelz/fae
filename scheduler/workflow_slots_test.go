package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/hostguard"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"
)

func slotFixture(t *testing.T) (*Ledger, Owner, WorkflowCapacity) {
	t.Helper()
	l, e := OpenDynamicLedger(filepath.Join(t.TempDir(), "ledger"), 1)
	if e != nil {
		t.Fatal(e)
	}
	owner, e := CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	cfg := hostguard.DefaultConfig()
	sample := hostguard.Sample{Memory: hostguard.Reading{Value: 20 * float64(hostguard.GiB), Known: true}, IO10: hostguard.Reading{Known: true}, IO60: hostguard.Reading{Known: true}, Memory10: hostguard.Reading{Known: true}, Memory60: hostguard.Reading{Known: true}, LoadPerCore: hostguard.Reading{Known: true}}
	return l, owner, WorkflowCapacity{Sample: sample, Config: cfg, ReferenceMemory: 4 * hostguard.GiB, ReferencePanes: 4, TotalMemory: 64 * hostguard.GiB, Now: time.Now()}
}
func slotHolder(id string, o Owner) WorkflowHolder {
	return WorkflowHolder{ID: id, Instance: "instance", Scope: "tree", Workflow: "workflow", Revision: 1, Owner: o, Memory: 4 * hostguard.GiB, Panes: 4, Priority: 10, Ready: time.Now()}
}
func TestWorkflowSlotsAtomicCommitmentsAndStaleUsage(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	for _, id := range []string{"one", "two", "three"} {
		d, e := l.AcquireWorkflowSlot(ctx, slotHolder(id, o), c)
		if e != nil || !d.Granted {
			t.Fatal(id, d, e)
		}
	}
	d, e := l.AcquireWorkflowSlot(ctx, slotHolder("four", o), c)
	if e != nil || d.Granted || d.Reason != "host_capacity" {
		t.Fatal(d, e)
	}
	if e = l.PublishWorkflowUsage(ctx, "one", o, hostguard.GiB, 1, c.Now); e != nil {
		t.Fatal(e)
	}
	d, _, e = l.WorkflowSlotSnapshot(ctx, c)
	if e != nil || d.UncommittedMemory != 3*hostguard.GiB {
		t.Fatal("unused commitment", d, e)
	}
	c.Now = c.Now.Add(61 * time.Second)
	d, _, e = l.WorkflowSlotSnapshot(ctx, c)
	if e != nil || d.UncommittedMemory != 2*hostguard.GiB {
		t.Fatal("stale usage not zero", d, e)
	}
}
func TestWorkflowSlotsCrossInstanceRaceLastFootprint(t *testing.T) {
	l, o, c := slotFixture(t)
	other, e := OpenDynamicLedger(l.path, 1)
	if e != nil {
		t.Fatal(e)
	}
	c.Sample.Memory.Value = 10 * float64(hostguard.GiB)
	for attempt := 0; attempt < 20; attempt++ {
		start := make(chan struct{})
		results := make(chan WorkflowSlotDecision, 2)
		var wg sync.WaitGroup
		for i, ledger := range []*Ledger{l, other} {
			wg.Add(1)
			go func(i int, ledger *Ledger) {
				defer wg.Done()
				<-start
				h := slotHolder(string(rune('a'+i)), o)
				h.Instance = h.ID
				d, e := ledger.AcquireWorkflowSlot(context.Background(), h, c)
				if e != nil {
					t.Error(e)
				}
				results <- d
			}(i, ledger)
		}
		close(start)
		wg.Wait()
		close(results)
		grants := 0
		for d := range results {
			if d.Granted {
				grants++
			}
		}
		if grants != 1 {
			t.Fatalf("race grants%d", grants)
		}
		for _, id := range []string{"a", "b"} {
			if e := l.ReleaseWorkflowSlot(context.Background(), id, o); e != nil {
				t.Fatal(e)
			}
		}
	}
}
func TestWorkflowSlotsLiveRequestsFIFOAndImpossibleFootprint(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	if e := l.RegisterWorkflowRequest(ctx, "a", o, 4); e != nil {
		t.Fatal(e)
	}
	if e := l.RegisterWorkflowRequest(ctx, "b", o, 2); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"one", "two"} {
		d, e := l.AcquireWorkflowSlot(ctx, slotHolder(id, o), c)
		if e != nil || !d.Granted {
			t.Fatal(d, e)
		}
	}
	d, e := l.AcquireWorkflowSlot(ctx, slotHolder("three", o), c)
	if e != nil || d.Reason != "workflow_limit" {
		t.Fatal(d, e)
	}
	if e = l.RegisterWorkflowRequest(ctx, "b", o, -1); e != nil {
		t.Fatal(e)
	}
	d, e = l.AcquireWorkflowSlot(ctx, slotHolder("three", o), c)
	if e != nil || !d.Granted {
		t.Fatal(d, e)
	}
	for _, id := range []string{"one", "two", "three"} {
		_ = l.ReleaseWorkflowSlot(ctx, id, o)
	}
	c.Sample.Memory.Value = 10 * float64(hostguard.GiB)
	large := slotHolder("large", o)
	large.Priority = 100
	large.Memory = 8 * hostguard.GiB
	d, e = l.AcquireWorkflowSlot(ctx, large, c)
	if e != nil || d.Reason != "host_capacity" {
		t.Fatal(d, e)
	}
	small := slotHolder("small", o)
	d, e = l.AcquireWorkflowSlot(ctx, small, c)
	if e != nil || d.Granted {
		t.Fatal("FIFO bypass", d, e)
	}
	_ = l.ReleaseWorkflowSlot(ctx, large.ID, o)
	large.Memory = 80 * hostguard.GiB
	d, e = l.AcquireWorkflowSlot(ctx, large, c)
	if e != nil || !d.Impossible {
		t.Fatal(d, e)
	}
	d, e = l.AcquireWorkflowSlot(ctx, small, c)
	if e != nil || !d.Granted {
		t.Fatal("impossible blocks queue", d, e)
	}
}
func TestWorkflowSlotTelemetryGapPressureLockAndRestore(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	h := slotHolder("holder", o)
	c.Sample.Memory.Known = false
	d, e := l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || d.Reason != "telemetry_gap" {
		t.Fatal(d, e)
	}
	c.Sample.Memory.Known = true
	c.Sample.IO10.Known = false
	d, e = l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || !d.Granted || !d.Gap {
		t.Fatal(d, e)
	}
	_ = l.ReleaseWorkflowSlot(ctx, h.ID, o)
	c.PressureHold = true
	d, e = l.AcquireWorkflowSlot(ctx, h, c)
	if e != nil || d.Granted {
		t.Fatal(d, e)
	}
	if e = l.ReestablishWorkflowSlot(ctx, h); e != nil {
		t.Fatal(e)
	}
	d, _, e = l.WorkflowSlotSnapshot(ctx, c)
	if e != nil || d.Held != 1 {
		t.Fatal(d, e)
	}
	lock, e := os.OpenFile(filepath.Join(l.path, "lock"), os.O_RDWR, 0600)
	if e != nil {
		t.Fatal(e)
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		t.Fatal(e)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	bounded, cancel := context.WithTimeout(ctx, 40*time.Millisecond)
	defer cancel()
	start := time.Now()
	d, e = l.AcquireWorkflowSlot(bounded, slotHolder("wait", o), c)
	if e == nil || d.Reason != "host_ledger" || time.Since(start) > time.Second {
		t.Fatal(d, e, time.Since(start))
	}
}

func TestWorkflowSlotsDeadOwnerReclaimAndAboveLimitReestablishment(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	dead := o
	dead.Started++
	if ProbeOwner(dead) != Dead {
		t.Skip("platform cannot prove PID incarnation mismatch")
	}
	if err := l.transact(ctx, true, func(s *ledgerState) error {
		w := workflowState(s)
		h := slotHolder("dead", dead)
		w.Holders[h.ID] = h
		w.Requests["dead-instance"] = WorkflowRequest{Limit: 1, Owner: dead}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	d, state, err := l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || d.Held != 0 || len(state.Requests) != 0 {
		t.Fatal("dead owner retained capacity", d, state, err)
	}
	if err = l.RegisterWorkflowRequest(ctx, "self", o, 1); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"restored-a", "restored-b", "restored-c"} {
		if err = l.ReestablishWorkflowSlot(ctx, slotHolder(id, o)); err != nil {
			t.Fatal(err)
		}
	}
	d, state, err = l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || len(state.Holders) != 3 || d.Limit != 1 {
		t.Fatal("restoration subject to requested limit", d, err)
	}
	d, err = l.AcquireWorkflowSlot(ctx, slotHolder("new", o), c)
	if err != nil || d.Granted || d.Reason != "workflow_limit" {
		t.Fatal("over-limit restoration admitted new work", d, err)
	}
}

func TestWorkflowSlotProcessHelper(t *testing.T) {
	path := os.Getenv("FAE_WORKFLOW_SLOT_TEST_LEDGER")
	if path == "" {
		return
	}
	l, err := OpenWorkflowLedger(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workflow slot helper: %v\n", err)
		os.Exit(2)
	}
	owner, err := CurrentOwner()
	if err != nil {
		fmt.Fprintf(os.Stderr, "workflow slot helper: %v\n", err)
		os.Exit(2)
	}
	if err = json.NewEncoder(os.Stdout).Encode("ready"); err != nil {
		fmt.Fprintf(os.Stderr, "workflow slot helper ready: %v\n", err)
		os.Exit(2)
	}
	if _, err = io.ReadFull(os.Stdin, make([]byte, 1)); err != nil {
		fmt.Fprintf(os.Stderr, "workflow slot helper: %v\n", err)
		os.Exit(2)
	}
	cfg := hostguard.DefaultConfig()
	c := WorkflowCapacity{Sample: hostguard.Sample{Memory: hostguard.Reading{Value: 10 * float64(hostguard.GiB), Known: true}}, Config: cfg, ReferenceMemory: 4 * hostguard.GiB, TotalMemory: 64 * hostguard.GiB, Now: time.Now()}
	h := slotHolder(fmt.Sprint(owner.PID), owner)
	h.Instance = h.ID
	decision, err := l.AcquireWorkflowSlot(context.Background(), h, c)
	if err != nil {
		fmt.Fprintf(os.Stderr, "workflow slot helper: %v\n", err)
		os.Exit(2)
	}
	if err = json.NewEncoder(os.Stdout).Encode(decision); err != nil {
		fmt.Fprintf(os.Stderr, "workflow slot helper: %v\n", err)
		os.Exit(2)
	}
	_, _ = io.ReadFull(os.Stdin, make([]byte, 1))
	os.Exit(0)
}
func TestWorkflowSlotsCompetingProcessesNeverOvercommit(t *testing.T) {
	for attempt := 0; attempt < 6; attempt++ {
		path := filepath.Join(t.TempDir(), "ledger")
		type contender struct {
			cmd     *exec.Cmd
			input   io.WriteCloser
			output  io.ReadCloser
			stderr  *bytes.Buffer
			done    chan error
			decoder *json.Decoder
		}
		children := []contender{}
		for i := 0; i < 2; i++ {
			helperCtx, cancelHelper := context.WithTimeout(context.Background(), 10*time.Second)
			t.Cleanup(cancelHelper)
			cmd := exec.CommandContext(helperCtx, os.Args[0], "-test.run=^TestWorkflowSlotProcessHelper$")
			cmd.Env = append(os.Environ(), "FAE_WORKFLOW_SLOT_TEST_LEDGER="+path)
			input, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stderr := &bytes.Buffer{}
			cmd.Stderr = stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait(); close(done) }()
			t.Cleanup(func() {
				_ = input.Close()
				select {
				case <-done:
				case <-time.After(100 * time.Millisecond):
					_ = cmd.Process.Kill()
					<-done
				}
			})
			children = append(children, contender{cmd: cmd, input: input, output: output, stderr: stderr, done: done, decoder: json.NewDecoder(output)})
		}
		// Both independent processes finish bootstrap before either acquisition.
		// Their second stdin barrier keeps the winner alive until both decisions.
		for _, child := range children {
			var ready string
			if err := child.decoder.Decode(&ready); err != nil || ready != "ready" {
				_ = child.input.Close()
				_ = child.cmd.Process.Kill()
				<-child.done
				t.Fatalf("helper PID%d ready failed: %q %v; stderr: %s", child.cmd.Process.Pid, ready, err, child.stderr.String())
			}
		}
		for _, child := range children {
			if _, err := child.input.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
		}
		granted := 0
		for _, child := range children {
			var decision WorkflowSlotDecision
			if err := child.decoder.Decode(&decision); err != nil {
				_ = child.input.Close()
				_ = child.cmd.Process.Kill()
				<-child.done
				t.Fatalf("helper PID%d protocol failed: %v; stderr: %s", child.cmd.Process.Pid, err, child.stderr.String())
			}
			if decision.Granted {
				granted++
			}
		}
		for _, child := range children {
			_, _ = child.input.Write([]byte{1})
			_ = child.input.Close()
			if err := <-child.done; err != nil {
				t.Fatalf("helper PID%d exited: %v; stderr: %s", child.cmd.Process.Pid, err, child.stderr.String())
			}
		}
		if granted != 1 {
			t.Fatalf("cross-process overcommit: granted%d", granted)
		}
	}
}

func TestWorkflowSlotReportedCeilingIsEstimateCandidateBinds(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	c.Sample.Memory.Value = 8 * float64(hostguard.GiB)
	small := slotHolder("small-reference-control", o)
	small.Memory = hostguard.GiB
	small.Panes = 1
	d, err := l.AcquireWorkflowSlot(ctx, small, c)
	if err != nil || !d.Granted || d.Ceiling != 0 {
		t.Fatal("reference estimate refused affordable candidate", d, err)
	}
	_ = l.ReleaseWorkflowSlot(ctx, small.ID, o)
	c.Sample.Memory.Value = 12 * float64(hostguard.GiB)
	c.ReferenceMemory = hostguard.GiB
	large := slotHolder("large-candidate-control", o)
	large.Memory = 8 * hostguard.GiB
	d, err = l.AcquireWorkflowSlot(ctx, large, c)
	if err != nil || d.Granted || d.Reason != "host_capacity" || d.Ceiling != 6 {
		t.Fatal("reference ceiling bypassed candidate footprint", d, err)
	}
}

func TestWorkflowSlotTelemetryGapKeepsLedgerDiagnostics(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	if err := l.RegisterWorkflowRequest(ctx, "instance", o, 3); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"one", "two"} {
		d, err := l.AcquireWorkflowSlot(ctx, slotHolder(id, o), c)
		if err != nil || !d.Granted {
			t.Fatal(d, err)
		}
	}
	c.Sample.Memory.Known = false
	d, state, err := l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || d.Reason != "telemetry_gap" || d.Requested != 3 || d.Held != 2 || len(d.Holders) != 2 || d.HeldByInstance["instance"] != 2 || len(state.Requests) != 1 {
		t.Fatal("telemetry gap hid known authority state", d, state, err)
	}
}

func TestWorkflowSlotSchemaUpgradePreservesLegacyPoolsAndFencesOldWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger")
	oldCompatible, err := OpenDynamicLedger(path, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err = oldCompatible.transact(context.Background(), true, func(s *ledgerState) error {
		p, e := NewPool(2)
		if e != nil {
			return e
		}
		s.ProviderPools = map[string]Pool{"existing": *p}
		s.Audit = append(s.Audit, LeaseAudit{Action: "legacy-existing"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	beforeRaw, err := os.ReadFile(filepath.Join(path, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var before ledgerState
	if err = json.Unmarshal(beforeRaw, &before); err != nil || before.Version != 1 {
		t.Fatal(err, before.Version)
	}
	upgraded, err := OpenWorkflowLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	afterRaw, err := os.ReadFile(filepath.Join(path, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var after ledgerState
	if err = json.Unmarshal(afterRaw, &after); err != nil {
		t.Fatal(err)
	}
	if after.Version != 2 || after.WorkflowSlots == nil || !reflect.DeepEqual(before.Pool, after.Pool) || !reflect.DeepEqual(before.ProviderPools, after.ProviderPools) || !reflect.DeepEqual(before.Audit, after.Audit[:len(before.Audit)]) || len(after.Audit) != len(before.Audit)+1 {
		t.Fatal("schema upgrade lost existing host ledger data")
	}
	// This is the previous binary's exact version gate, before invoking any
	// mutation callback. Without it its old struct would discard WorkflowSlots.
	var legacy struct {
		Version int
		Pool    Pool
		Audit   []LeaseAudit
	}
	if err = json.Unmarshal(afterRaw, &legacy); err != nil {
		t.Fatal(err)
	}
	legacyRefused := legacy.Version != 1
	if !legacyRefused {
		t.Fatal("old writer accepted slot ledger schema")
	}
	negative, _ := json.Marshal(legacy)
	var lost ledgerState
	_ = json.Unmarshal(negative, &lost)
	if lost.WorkflowSlots != nil {
		t.Fatal("negative old-struct control unexpectedly preserved slots")
	}
	owner, err := CurrentOwner()
	if err != nil {
		t.Fatal(err)
	}
	if err = upgraded.RegisterWorkflowRequest(context.Background(), "aware", owner, -1); err != nil {
		t.Fatal(err)
	}
	if _, err = oldCompatible.Snapshot(context.Background()); err != nil {
		t.Fatal("new heavy-pool reader rejected upgraded common ledger", err)
	}
	raw, err := os.ReadFile(filepath.Join(path, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var final ledgerState
	_ = json.Unmarshal(raw, &final)
	if final.Version != 2 || len(final.WorkflowSlots.Instances) != 1 || final.ProviderPools["existing"].Capacity != 2 {
		t.Fatal("new pool reader erased slot participants")
	}
}
func TestWorkflowSlotLegacyLiveParticipantRequiresUpgrade(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	if err := l.ConfigureHost(ctx, "legacy", 3, c.Config); err != nil {
		t.Fatal(err)
	}
	d, err := l.AcquireWorkflowSlot(ctx, slotHolder("holder", o), c)
	if err != nil || d.Granted || d.Reason != "host_ledger" {
		t.Fatal("unregistered live legacy process silently bypassed slots", d, err)
	}
	if err = l.RegisterWorkflowRequest(ctx, "upgraded", o, -1); err != nil {
		t.Fatal(err)
	}
	d, err = l.AcquireWorkflowSlot(ctx, slotHolder("holder", o), c)
	if err != nil || !d.Granted {
		t.Fatal("registered v2 participant still fenced", d, err)
	}
}

func TestWorkflowSlotExistingHolderRepricesOnlyAdditionalUnusedCommitment(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	if err := l.RegisterWorkflowRequest(ctx, "instance", o, 1); err != nil {
		t.Fatal(err)
	}
	h := slotHolder("tree", o)
	d, err := l.AcquireWorkflowSlot(ctx, h, c)
	if err != nil || !d.Granted {
		t.Fatal(d, err)
	}
	if err = l.PublishWorkflowUsage(ctx, h.ID, o, hostguard.GiB, 1, c.Now); err != nil {
		t.Fatal(err)
	}
	c.Sample.Memory.Value = 10 * float64(hostguard.GiB)
	larger := h
	larger.Revision = 2
	larger.Memory = 8 * hostguard.GiB
	larger.Panes = 8
	d, err = l.AcquireWorkflowSlot(ctx, larger, c)
	if err != nil || d.Granted || d.Reason != "host_capacity" {
		t.Fatal("larger revision bypassed commitment", d, err)
	}
	_, state, err := l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || state.Holders[h.ID].Memory != 4*hostguard.GiB {
		t.Fatal("denied resize changed old holder", state, err)
	}
	d, err = l.AcquireWorkflowSlot(ctx, h, c)
	if err != nil || !d.Granted {
		t.Fatal("denied resize stopped prior children", d, err)
	}
	c.Sample.Memory.Value = 14 * float64(hostguard.GiB)
	d, err = l.AcquireWorkflowSlot(ctx, larger, c)
	if err != nil || !d.Granted || !d.Repriced || d.GrantedHolder.Memory != 8*hostguard.GiB {
		t.Fatal("delta fitting existing holder was refused by requested count", d, err)
	}
	_, state, err = l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || len(state.Holders) != 1 || state.Holders[h.ID].Revision != 2 {
		t.Fatal(state, err)
	}
	d, err = l.AcquireWorkflowSlot(ctx, h, c)
	if err != nil || !d.Granted {
		t.Fatal(d, err)
	}
	_, state, err = l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || state.Holders[h.ID].Memory != 8*hostguard.GiB {
		t.Fatal("smaller/older visit shrank conservative commitment", state, err)
	}
}

func TestWorkflowSlotSharedReservationPricesCandidateAndReferenceUnderLock(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	shared := c.Config
	shared.PerPane = 2 * hostguard.GiB
	if err := l.ConfigureHost(ctx, "shared-price", 3, shared); err != nil {
		t.Fatal(err)
	}
	if err := l.RegisterWorkflowRequest(ctx, "instance", o, -1); err != nil {
		t.Fatal(err)
	}
	h := slotHolder("priced", o)
	h.Sessions = 2
	h.Memory = 2 * c.Config.PerPane
	h.Panes = 0
	c.ReferenceSessions = 2
	c.ReferenceMemory = 2 * c.Config.PerPane
	d, err := l.AcquireWorkflowSlot(ctx, h, c)
	if err != nil || !d.Granted || d.GrantedHolder.Memory != 4*hostguard.GiB {
		t.Fatal("local price bypassed shared effective reservation", d, err)
	}
	snapshot, state, err := l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || snapshot.Ceiling != 3 || state.Holders[h.ID].Memory != 4*hostguard.GiB {
		t.Fatal("reference ceiling used local price", snapshot, state, err)
	}
	shared.PerPane = 3 * hostguard.GiB
	if err = l.ConfigureHost(ctx, "shared-price", 3, shared); err != nil {
		t.Fatal(err)
	}
	d, err = l.AcquireWorkflowSlot(ctx, h, c)
	if err != nil || !d.Granted || d.GrantedHolder.Memory != 4*hostguard.GiB {
		t.Fatal("later reservation repriced an unchanged running holder", d, err)
	}
	snapshot, _, err = l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || snapshot.Ceiling != 2 {
		t.Fatal("new reference did not use raised shared price", snapshot, err)
	}
}
func TestWorkflowSlotProcessHolderDoesNotConsumeOtherAdaptersPanes(t *testing.T) {
	l, o, c := slotFixture(t)
	ctx := context.Background()
	process := slotHolder("process", o)
	process.Sessions = 2
	process.Panes = 0
	d, err := l.AcquireWorkflowSlot(ctx, process, c)
	if err != nil || !d.Granted || d.GrantedHolder.Panes != 0 {
		t.Fatal(d, err)
	}
	c.PaneAdapter = true
	c.Config.PaneLimit = 1
	c.ReferenceSessions = 1
	c.ReferencePanes = 1
	pane := slotHolder("pane", o)
	pane.Sessions = 1
	pane.Panes = 1
	d, err = l.AcquireWorkflowSlot(ctx, pane, c)
	if err != nil || !d.Granted {
		t.Fatal("process holder consumed pane commitments", d, err)
	}
	snapshot, _, err := l.WorkflowSlotSnapshot(ctx, c)
	if err != nil || snapshot.UncommittedPanes != 0 {
		t.Fatal(snapshot, err)
	}
}
