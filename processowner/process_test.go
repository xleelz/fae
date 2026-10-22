package processowner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	dir := os.Getenv("FAE_OWNER_TEST")
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
	parent.Env = append(os.Environ(), "FAE_OWNER_TEST="+dir)
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
	if e = terminate(ctx, r, "owner"); e == nil {
		t.Fatal("identity mismatch accepted")
	}
	if e = cmd.Process.Signal(syscall.Signal(0)); e != nil {
		t.Fatal("foreign identity killed")
	}
	r.Birth, _ = birth(r.PID)
	if e = terminate(ctx, r, "other"); e == nil {
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
