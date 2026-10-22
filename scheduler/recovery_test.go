package scheduler

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/processowner"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRecoveryPredicateExhaustive(t *testing.T) {
	for n := 0; n < 16; n++ {
		want := n&1 != 0 && n&2 != 0 && (n&4 != 0 || n&8 != 0)
		if CanRecoverLease(n&1 != 0, n&2 != 0, n&4 != 0, n&8 != 0) != want {
			t.Fatal(n)
		}
	}
}
func journalDir(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func TestJournalDurableBoundaryAndRefusal(t *testing.T) {
	root := journalDir(t)
	j, e := OpenJournal(filepath.Join(root, "proofs"))
	if e != nil {
		t.Fatal(e)
	}
	p := OperationProof{ID: "abc", Session: "session", Instance: root, Owner: Owner{5, 6}, Birth: "precise", State: "reserved"}
	if e = j.Put(p); e != nil {
		t.Fatal(e)
	}
	again, e := OpenJournal(filepath.Join(root, "proofs"))
	if e != nil {
		t.Fatal(e)
	}
	actual, e := again.Read("abc")
	if e != nil || actual != p {
		t.Fatal(actual, e)
	}
	p.State = "launching"
	if e = j.Put(p); e != nil {
		t.Fatal(e)
	}
	entries, e := j.Entries()
	if e != nil || len(entries) != 1 || entries[0].State != "launching" {
		t.Fatal(entries, e)
	}
	os.Chmod(filepath.Join(root, "proofs", "abc.json"), 0644)
	if _, e = j.Read("abc"); e == nil {
		t.Fatal("public proof accepted")
	}
	if _, e = j.Read("../abc"); e == nil {
		t.Fatal("traversal accepted")
	}
	link := filepath.Join(root, "link")
	os.Symlink(root, link)
	if _, e = OpenJournal(link); e == nil {
		t.Fatal("symlink accepted")
	}
}
func TestRecoverProofNoForeignOrPartialRelease(t *testing.T) {
	for _, state := range []string{"reserved", "launching", "retired"} {
		for _, dead := range []bool{false, true} {
			for _, matching := range []bool{false, true} {
				p, _ := NewPool(1)
				owner := Owner{100, 123}
				p.Enqueue(Request{ID: "held", Owner: owner, Weight: 1, Queued: time.Now()})
				p.AdvanceObserved(time.Now(), map[Owner]Liveness{})
				proof := OperationProof{ID: "held", Owner: owner, State: state}
				if !matching {
					proof.Owner.Started++
				}
				changed, e := p.RecoverProof(proof, dead)
				want := dead && matching && state != "launching"
				if changed != want || (e == nil) != want || len(p.Holders) != 1-boolInt(want) {
					t.Fatalf("%s %v %v: %v %v", state, dead, matching, changed, e)
				}
			}
		}
	}
}
func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
func TestLeaseCrashHelper(t *testing.T) {
	root := os.Getenv("FAE_LEASE_CRASH_ROOT")
	if root == "" {
		return
	}
	owner, e := CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	birth, e := processowner.Birth(owner.PID)
	if e != nil {
		t.Fatal(e)
	}
	j, e := OpenJournal(filepath.Join(root, "proofs"))
	if e != nil {
		t.Fatal(e)
	}
	l, e := OpenLedger(filepath.Join(root, "ledger"), 2)
	if e != nil {
		t.Fatal(e)
	}
	p := OperationProof{ID: "owned", Session: "s", Instance: root, Owner: owner, Birth: birth, State: "reserved"}
	if e = j.Put(p); e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if e = l.Enqueue(ctx, Request{ID: p.ID, Owner: owner, Weight: 1, Queued: time.Now()}); e != nil {
		t.Fatal(e)
	}
	l.Advance(ctx, time.Now(), map[Owner]Liveness{owner: Alive})
	// Finish an actual owned workload, retaining the lease to simulate a crash
	// after retirement fsync but before either pool release.
	p.State = "launching"
	if e = j.Put(p); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("/bin/sh", "-c", "sleep 0.03")
	r, e := processowner.Start(cmd, filepath.Join(root, "processes"), root)
	if e != nil {
		t.Fatal(e)
	}
	if e = processowner.Wait(cmd, r); e != nil {
		t.Fatal(e)
	}
	p.State = "retired"
	if e = j.Put(p); e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(p)
	if e = os.WriteFile(filepath.Join(root, "ready"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	select {}
}
func TestSIGKILLRetiredLeaseRecoversCapacityPreservesForeign(t *testing.T) {
	root := journalDir(t)
	l, e := OpenLedger(filepath.Join(root, "ledger"), 2)
	if e != nil {
		t.Fatal(e)
	}
	current, e := CurrentOwner()
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	l.Enqueue(ctx, Request{ID: "foreign", Owner: current, Weight: 1, Queued: time.Now()})
	l.Advance(ctx, time.Now(), map[Owner]Liveness{current: Alive})
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^TestLeaseCrashHelper$")
	cmd.Env = append(os.Environ(), "FAE_LEASE_CRASH_ROOT="+root)
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	var proof OperationProof
	deadline := time.Now().Add(8 * time.Second)
	for {
		raw, e := os.ReadFile(filepath.Join(root, "ready"))
		if e == nil {
			if e = json.Unmarshal(raw, &proof); e != nil {
				t.Fatal(e)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("helper timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e = l.Recover(ctx, proof, processowner.ProvedDead(proof.Owner.PID, proof.Birth)); e == nil {
		t.Fatal("live owner reclaimed")
	}
	cmd.Process.Kill()
	cmd.Wait()
	if !processowner.ProvedDead(proof.Owner.PID, proof.Birth) {
		t.Fatal("dead owner unknown")
	}
	if e = l.Recover(ctx, proof, true); e != nil {
		t.Fatal(e)
	}
	if e = l.Recover(ctx, proof, true); e != nil {
		t.Fatal("replay", e)
	}
	p, e := l.Snapshot(ctx)
	if e != nil || len(p.Holders) != 1 || p.Holders[0].ID != "foreign" {
		t.Fatal(p, e)
	}
	audit, e := l.Audit(ctx)
	if e != nil {
		t.Fatal(e)
	}
	count := 0
	for _, a := range audit {
		if a.Action == "recovered" {
			count++
		}
	}
	if count != 1 {
		t.Fatal("duplicate recovery audit", count)
	}
	l.Enqueue(ctx, Request{ID: "new", Owner: current, Weight: 1, Queued: time.Now()})
	granted, _, e := l.Advance(ctx, time.Now(), map[Owner]Liveness{current: Alive})
	if e != nil || len(granted) != 1 {
		t.Fatal("capacity not restored", granted, e)
	}
}
