package processowner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A record retired or acknowledged between a scan's listing and its read was
// not read at all: LinkedRetirement, like the other scans, skips it instead
// of failing another session's retirement proof (FAE-ROB-016).
func TestLinkedRetirementSkipsARecordRetiredMidScan(t *testing.T) {
	dir := testDir(t)
	var done atomic.Bool
	scans := make(chan error, 4)
	for range cap(scans) {
		go func() {
			for !done.Load() {
				if _, e := LinkedRetirement(dir, "owner", "mine", os.Getpid(), "birth", 1); e != nil {
					scans <- e
					return
				}
			}
			scans <- nil
		}()
	}
	defer done.Store(true)
	// Another session's record keeps moving between its active and retired
	// names, so a listing keeps naming a path that is gone when it is read.
	path, e := publish(dir, &Record{PID: 4242, PGID: 4242, Birth: "b", ParentPID: 4241, ParentBirth: "p", Owner: "owner", Session: "other"})
	if e != nil {
		t.Fatal(e)
	}
	for range 20000 {
		if e = os.Rename(path, path+".retired"); e != nil {
			t.Fatal(e)
		}
		if e = os.Rename(path+".retired", path); e != nil {
			t.Fatal(e)
		}
	}
	done.Store(true)
	for range cap(scans) {
		if e := <-scans; e != nil {
			t.Fatalf("a record retired mid-scan failed the proof: %v", e)
		}
	}
}

// A malformed record that still names another session cannot be one of this
// session's linked processes, so it neither blocks this session's
// acknowledgement nor its proof. One that does not decode, or names this
// session, still fails closed.
func TestRetirementScansIgnoreAnotherSessionsMalformedRecord(t *testing.T) {
	plant := func(t *testing.T, dir, name, body string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(dir, name)
		if e := os.WriteFile(path, []byte(body), mode); e != nil {
			t.Fatal(e)
		}
		if e := os.Chmod(path, mode); e != nil {
			t.Fatal(e)
		}
	}
	parent, parentBirth := os.Getpid(), "parent-birth"
	record := func(session string, pid int) string {
		return fmt.Sprintf(`{"PID":4242,"PGID":4242,"ParentPID":%d,"Birth":"b","ParentBirth":%q,"Owner":"owner","Session":%q}`, pid, parentBirth, session)
	}
	for _, c := range []struct {
		name, body    string
		mode          os.FileMode
		ack, proofErr bool // whether acknowledgement succeeds, whether the proof errors
	}{
		{"other-session-wrong-mode", record("other", parent), 0644, true, false},
		{"other-session-invalid-identity", `{"PID":1,"Owner":"","Session":"other"}`, 0600, true, false},
		{"this-session-other-parent-wrong-mode", record("mine", parent+1), 0644, true, true},
		{"this-session-wrong-mode", record("mine", parent), 0644, false, true},
		{"empty", "", 0600, false, true},
		{"truncated", record("other", parent)[:40], 0600, false, true},
		{"no-session", `{"PID":4242}`, 0644, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := testDir(t)
			plant(t, dir, "process-planted.json", c.body, c.mode)
			e := AcknowledgeLinkedRetirement(dir, "owner", "mine", parent, parentBirth)
			if (e == nil) != c.ack {
				t.Fatalf("acknowledgement err=%v, want success=%v", e, c.ack)
			}
			proved, e := LinkedRetirement(dir, "owner", "mine", parent, parentBirth, 1)
			if (e != nil) != c.proofErr || proved {
				t.Fatalf("proof=%v err=%v, want error=%v", proved, e, c.proofErr)
			}
		})
	}
}

// Recover sweeps the pending files of crashed publishers without touching a
// live publisher's: a whole record of a dead publisher has its stopped launch
// gate terminated under identity proof and is removed; a partial one goes
// once no process has the pid in its name. Live, unprovable and other
// owners' pending files stay.
func TestRecoverSweepsOnlyCrashedPublishersPendingRecords(t *testing.T) {
	dir := testDir(t)
	publisher := exec.Command("/bin/sleep", "300")
	if e := publisher.Start(); e != nil {
		t.Fatal(e)
	}
	deadPID := publisher.Process.Pid
	deadBirth, e := birth(deadPID)
	if e != nil {
		t.Fatal(e)
	}
	publisher.Process.Kill()
	publisher.Wait()
	gate := exec.Command("/bin/sh", "-c", "kill -STOP $$; exec sleep 300")
	gate.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e = gate.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { syscall.Kill(-gate.Process.Pid, syscall.SIGKILL); gate.Wait() }()
	for until := time.Now().Add(3 * time.Second); ; {
		out, _ := exec.Command("ps", "-p", strconv.Itoa(gate.Process.Pid), "-o", "stat=").Output()
		if strings.Contains(string(out), "T") {
			break
		}
		if time.Now().After(until) {
			t.Fatal("gate never stopped")
		}
		time.Sleep(time.Millisecond)
	}
	gateBirth, e := birth(gate.Process.Pid)
	if e != nil {
		t.Fatal(e)
	}
	selfBirth, e := birth(os.Getpid())
	if e != nil {
		t.Fatal(e)
	}
	write := func(name string, r *Record, raw string) string {
		t.Helper()
		if r != nil {
			b, _ := json.Marshal(r)
			raw = string(b)
		}
		path := filepath.Join(dir, name)
		if e := os.WriteFile(path, []byte(raw), 0600); e != nil {
			t.Fatal(e)
		}
		return path
	}
	gateRecord := func(owner string, parent int, parentBirth string) *Record {
		return &Record{PID: gate.Process.Pid, PGID: gate.Process.Pid, Birth: gateBirth, ParentPID: parent, ParentBirth: parentBirth, Owner: owner, Session: "s"}
	}
	crashed := write(fmt.Sprintf("%s%d-1.json", ".pending-process-", deadPID), gateRecord("owner", deadPID, deadBirth), "")
	partialDead := write(fmt.Sprintf("%s%d-2.json", ".pending-process-", deadPID), nil, `{"PID":`)
	kept := []string{
		write(fmt.Sprintf("%s%d-3.json", ".pending-process-", os.Getpid()), gateRecord("owner", os.Getpid(), selfBirth), ""),
		write(fmt.Sprintf("%s%d-4.json", ".pending-process-", os.Getpid()), nil, ""),
		write(".pending-process-5.json", nil, `{"PID":`),
		write(fmt.Sprintf("%s%d-6.json", ".pending-process-", deadPID), gateRecord("foreign", deadPID, deadBirth), ""),
	}
	recovered, e := Recover(context.Background(), dir, "owner")
	if e != nil || len(recovered) != 0 {
		t.Fatal("sweep failed or reported a pending gate as a recovered record", recovered, e)
	}
	if alive, e := groupPresent(gate.Process.Pid); e != nil || alive {
		t.Fatal("crashed publisher's stopped gate survived", alive, e)
	}
	for _, gone := range []string{crashed, crashed + ".retired", partialDead} {
		if _, e := os.Stat(gone); !os.IsNotExist(e) {
			t.Fatal("crashed publisher's pending file kept", gone, e)
		}
	}
	for _, path := range kept {
		if _, e := os.Stat(path); e != nil {
			t.Fatal("a live, legacy or foreign pending file was swept", path, e)
		}
	}
}
