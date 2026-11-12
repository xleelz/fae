package processowner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestGuardExhaustive(t *testing.T) {
	for n := 0; n < 16; n++ {
		if CanTerminate(n&1 != 0, n&2 != 0, n&4 != 0, n&8 != 0) != (n == 15) {
			t.Fatal(n)
		}
	}
}
func TestOwnedHelper(t *testing.T) {
	dir := os.Getenv("OWNER_TEST")
	if dir == "" {
		return
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & wait")
	r, e := Start(cmd, dir, dir)
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(r)
	if e = os.WriteFile(filepath.Join(dir, "ready"), b, 0600); e != nil {
		t.Fatal(e)
	}
	select {}
}
func waitFile(t *testing.T, path string) []byte {
	t.Helper()
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		b, e := os.ReadFile(path)
		if e == nil {
			return b
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout", path)
	return nil
}
func TestSIGKILLParentWatchdogAndForeignIsolation(t *testing.T) {
	dir := testDir(t)
	foreign := exec.Command("/bin/sleep", "300")
	if e := foreign.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { foreign.Process.Kill(); foreign.Wait() }()
	exe, _ := os.Executable()
	parent := exec.Command(exe, "-test.run=^TestOwnedHelper$")
	parent.Env = append(os.Environ(), "OWNER_TEST="+dir)
	if e := parent.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { parent.Process.Kill(); parent.Wait() }()
	var r Record
	if e := json.Unmarshal(waitFile(t, filepath.Join(dir, "ready")), &r); e != nil {
		t.Fatal(e)
	}
	defer syscall.Kill(-r.PGID, syscall.SIGKILL)
	parent.Process.Kill()
	parent.Wait()
	until := time.Now().Add(8 * time.Second)
	for {
		alive, e := groupPresent(r.PGID)
		if e != nil {
			t.Fatal(e)
		}
		if !alive {
			break
		}
		if time.Now().After(until) {
			t.Fatal("watchdog failed to retire group", r.PGID)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if e := foreign.Process.Signal(syscall.Signal(0)); e != nil {
		t.Fatal("foreign process disturbed", e)
	}
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if _, e := Recover(ctx, dir, dir); e != nil {
		t.Fatal(e)
	}
}
func TestIdentityMismatchRefusesSignal(t *testing.T) {
	dir := testDir(t)
	cmd := exec.Command("/bin/sleep", "300")
	r, e := Start(cmd, dir, "owner")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { syscall.Kill(-r.PGID, syscall.SIGKILL); cmd.Wait(); Retire(r) }()
	r.Birth += "wrong"
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if e = terminate(ctx, r, "owner", 0); e == nil {
		t.Fatal("identity mismatch accepted")
	}
	if e = cmd.Process.Signal(syscall.Signal(0)); e != nil {
		t.Fatal("foreign identity killed")
	}
	r.Birth, _ = birth(r.PID)
	if e = terminate(ctx, r, "other", 0); e == nil {
		t.Fatal("foreign owner accepted")
	}
}
func TestRecoverLiveParentRefused(t *testing.T) {
	dir := testDir(t)
	cmd := exec.Command("/bin/sleep", "300")
	r, e := Start(cmd, dir, "owner")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { syscall.Kill(-r.PGID, syscall.SIGKILL); cmd.Wait(); Retire(r) }()
	if _, e = Recover(context.Background(), dir, "owner"); e == nil {
		t.Fatal("live owner killed")
	}
	if e = Retire(r); e == nil {
		t.Fatal("live group retired")
	}
}
func TestPrivateRegistryAndRecord(t *testing.T) {
	dir := testDir(t)
	link := filepath.Join(dir, "link")
	os.Symlink(dir, link)
	if e := privateDir(link); e == nil {
		t.Fatal("symlink accepted")
	}
	p := filepath.Join(dir, "process-bad.json")
	os.WriteFile(p, []byte(`{"PID":`+strconv.Itoa(os.Getpid())+`}`), 0644)
	if _, e := Recover(context.Background(), dir, "owner"); e == nil {
		t.Fatal("malformed record accepted")
	}
}
func TestStartLiteralArgumentsAndRecord(t *testing.T) {
	dir := testDir(t)
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.1; exit 0", "$(touch SHOULD_NOT_EXIST)")
	r, e := Start(cmd, dir, "owner")
	if e != nil {
		t.Fatal(e)
	}
	if r.Command[3] != "$(touch SHOULD_NOT_EXIST)" || r.Birth == "" {
		t.Fatal(r)
	}
	if e = cmd.Wait(); e != nil {
		t.Fatal(e)
	}
	if e = Retire(r); e != nil {
		t.Fatal(e)
	}
}

func testDir(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(p, 0700); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestLeaderlessGroupIsAmbiguous(t *testing.T) {
	dir := testDir(t)
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & wait")
	r, e := Start(cmd, dir, "owner")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { syscall.Kill(-r.PGID, syscall.SIGKILL); Retire(r) }()
	// Allow the child to fork, then remove only the original identity anchor.
	time.Sleep(100 * time.Millisecond)
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = cmd.Wait()
	if alive, e := groupPresent(r.PGID); e != nil || !alive {
		t.Fatalf("expected helper group %v %v", alive, e)
	}
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if e = Terminate(ctx, r, "owner"); e == nil {
		t.Fatal("leaderless group authorized")
	}
}

func TestWaitRetiresHelpersBeforeReapingLeader(t *testing.T) {
	dir := testDir(t)
	cmd := exec.Command("/bin/sh", "-c", "sleep 300 & exit 7")
	r, e := Start(cmd, dir, "owner")
	if e != nil {
		t.Fatal(e)
	}
	defer syscall.Kill(-r.PGID, syscall.SIGKILL)
	if e = Wait(cmd, r); e == nil {
		t.Fatal("exit status lost")
	}
	if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 7 {
		t.Fatal(cmd.ProcessState, e)
	}
	alive, e := groupPresent(r.PGID)
	if e != nil || alive {
		t.Fatalf("helper survived: %v %v", alive, e)
	}
	if e = Retire(r); e != nil {
		t.Fatal(e)
	}
}

func TestCanceledLaunchGateReturnsPromptly(t *testing.T) {
	dir := testDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sleep", "300")
	started := time.Now()
	record, e := Start(cmd, dir, "owner")
	if e == nil {
		_ = Wait(cmd, record)
		_ = Retire(record)
	}
	if time.Since(started) > time.Second {
		t.Fatal("canceled gate did not return promptly")
	}
}

func TestLinkedRetirementRequiresCompleteExactReceipts(t *testing.T) {
	dir := testDir(t)
	cmd := exec.Command("/bin/sleep", "300")
	r, e := StartLinked(cmd, dir, "owner", "session")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait(); Retire(r) }()
	check := func(owner, session string, parent int, birth string, expected int, want bool) {
		t.Helper()
		got, e := LinkedRetirement(dir, owner, session, parent, birth, expected)
		if e != nil || got != want {
			t.Fatalf("linked proof=%v err=%v want=%v", got, e, want)
		}
	}
	check("owner", "session", r.ParentPID, r.ParentBirth, 1, false)
	ctx, c := context.WithTimeout(context.Background(), time.Second)
	defer c()
	if e = Terminate(ctx, r, "owner"); e != nil {
		t.Fatal(e)
	}
	cmd.Wait()
	check("owner", "session", r.ParentPID, r.ParentBirth, 1, true)
	check("foreign", "session", r.ParentPID, r.ParentBirth, 1, false)
	check("owner", "session", r.ParentPID, r.ParentBirth+"reused", 1, false)
	check("owner", "other", r.ParentPID, r.ParentBirth, 1, false)
	check("owner", "session", r.ParentPID, r.ParentBirth, 2, false)
	check("owner", "session", r.ParentPID, r.ParentBirth, 0, false)
	raw, e := os.ReadFile(r.Path + ".retired")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "process-duplicate.json.retired"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	check("owner", "session", r.ParentPID, r.ParentBirth, 2, false)
}

func TestRetirePreservesLinkedReceiptUntilDurableAcknowledgment(t *testing.T) {
	dir := testDir(t)
	cmd := exec.Command("/bin/sleep", "300")
	r, e := StartLinked(cmd, dir, "owner", "handoff")
	if e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait(); Retire(r) }()
	if e = AcknowledgeLinkedRetirement(dir, "owner", "handoff", r.ParentPID, r.ParentBirth); e == nil {
		t.Fatal("active group acknowledged")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if e = Terminate(ctx, r, "owner"); e != nil {
		t.Fatal(e)
	}
	cmd.Wait()
	// This is the process-retired / lease-journal-still-launching crash window.
	if e = Retire(r); e != nil {
		t.Fatal(e)
	}
	if e = Retire(r); e != nil {
		t.Fatal("retire not idempotent", e)
	}
	complete, e := LinkedRetirement(dir, "owner", "handoff", r.ParentPID, r.ParentBirth, 1)
	if e != nil || !complete {
		t.Fatal("lost durable crash proof", complete, e)
	}
	if e = AcknowledgeLinkedRetirement(dir, "foreign", "handoff", r.ParentPID, r.ParentBirth); e == nil {
		t.Fatal("foreign owner acknowledged")
	}
	complete, e = LinkedRetirement(dir, "owner", "handoff", r.ParentPID, r.ParentBirth, 1)
	if e != nil || !complete {
		t.Fatal("foreign ack destroyed proof", complete, e)
	}
	// The caller now durably records retired; receipts can be discarded.
	if e = AcknowledgeLinkedRetirement(dir, "owner", "handoff", r.ParentPID, r.ParentBirth); e != nil {
		t.Fatal(e)
	}
	if e = AcknowledgeLinkedRetirement(dir, "owner", "handoff", r.ParentPID, r.ParentBirth); e != nil {
		t.Fatal("ack not idempotent", e)
	}
	complete, e = LinkedRetirement(dir, "owner", "handoff", r.ParentPID, r.ParentBirth, 1)
	if e != nil || complete {
		t.Fatal("receipt remained after ack", complete, e)
	}
}

// A launch the stop-before-exec gate never released is typed: the gate group
// was killed and reaped, and its absence is positive retirement proof.
func TestUnreleasedLaunchGateProvesRetirement(t *testing.T) {
	for _, kind := range []string{"canceled", "unregistered"} {
		t.Run(kind, func(t *testing.T) {
			dir := testDir(t)
			registry := dir
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sleep", "300")
			if kind == "canceled" {
				// Cancellation lands while the gate holds the workload stopped.
				cmd.Cancel = func() error { return cmd.Process.Kill() }
				go func() { time.Sleep(time.Millisecond); cancel() }()
			} else {
				registry = filepath.Join(dir, "shared")
				if e := os.Mkdir(registry, 0755); e != nil {
					t.Fatal(e)
				}
			}
			record, e := StartLinked(cmd, registry, "owner", "session")
			if e == nil {
				// Cancellation lost the race to registration; nothing to prove.
				_ = Wait(cmd, record)
				_ = Retire(record)
				t.Skip("gate released before cancellation")
			}
			var gate *GateError
			if !errors.As(e, &gate) || !gate.Retired {
				t.Fatalf("unreleased gate not proven retired: %v", e)
			}
			if cmd.ProcessState == nil {
				t.Fatal("gate leader not reaped")
			}
			if alive, err := groupPresent(cmd.Process.Pid); err != nil || alive {
				t.Fatal("gate group survived", alive, err)
			}
			if records, _ := filepath.Glob(filepath.Join(registry, "process-*.json")); len(records) != 0 {
				t.Fatal("active record left for an unreleased gate", records)
			}
		})
	}
}

// A registration publishes its record whole: a concurrent scan of the
// registry, such as another session's retirement acknowledgement, sees the
// record complete or not at all, never created but still unwritten.
func TestRegistryScanNeverReadsAnUnwrittenRecord(t *testing.T) {
	dir := testDir(t)
	cmd := exec.Command("/bin/sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var registered atomic.Bool
	scans := make(chan error, 4)
	for range cap(scans) {
		go func() {
			for !registered.Load() {
				if e := AcknowledgeLinkedRetirement(dir, "owner", "mine", os.Getpid(), "birth"); e != nil {
					scans <- e
					return
				}
			}
			scans <- nil
		}()
	}
	// Each record is removed once registered, so a scan stays one short
	// read and keeps landing inside the next registration.
	for range 300 {
		r, e := RegisterRole(cmd.Process.Pid, dir, "owner", "other", "")
		if e != nil {
			registered.Store(true)
			t.Fatal(e)
		}
		os.Remove(r.Path)
	}
	registered.Store(true)
	for range cap(scans) {
		if e := <-scans; e != nil {
			t.Fatalf("scan read an unwritten record: %v", e)
		}
	}
}

// StartRole publishes its record whole, command and directory included, so a
// concurrent registry scan (another session's retirement acknowledgement, the
// inventory) never reads a started process's record empty or partway
// rewritten (FAE-HOST-021, FAE-ROB-016). Each launch is retired and
// its receipt removed, so a scan stays one short read.
func TestStartRoleRecordIsNeverReadPartway(t *testing.T) {
	dir := testDir(t)
	var done atomic.Bool
	scans := make(chan error, 4)
	for range cap(scans) {
		go func() {
			for !done.Load() {
				if e := AcknowledgeLinkedRetirement(dir, "owner", "mine", os.Getpid(), "birth"); e != nil {
					scans <- e
					return
				}
				if _, e := Inventory(dir); e != nil {
					scans <- e
					return
				}
			}
			scans <- nil
		}()
	}
	defer done.Store(true)
	for range 150 {
		cmd := exec.Command("/bin/true", "started")
		cmd.Dir = dir
		r, e := StartRole(cmd, dir, "owner", "other", "host-command")
		if e != nil {
			t.Fatal(e)
		}
		if len(r.Command) != 2 || r.Command[1] != "started" || r.Directory != dir {
			t.Fatalf("record lacks its command or directory: %+v", r)
		}
		if e = Wait(cmd, r); e != nil {
			t.Fatal(e)
		}
		if e = os.Remove(r.Path + ".retired"); e != nil {
			t.Fatal(e)
		}
	}
	done.Store(true)
	for range cap(scans) {
		if e := <-scans; e != nil {
			t.Fatalf("scan read a started record partway: %v", e)
		}
	}
}
