package scheduler

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestExpiredUnknownAndLiveOwnersRetainCapacity(t *testing.T) {
	for _, state := range []Liveness{Unknown, Alive, Dead} {
		p, _ := NewPool(1)
		o := Owner{1, 1}
		now := time.Now()
		p.Enqueue(Request{ID: "old", Owner: o, Weight: 1, Queued: now})
		p.AdvanceObserved(now, map[Owner]Liveness{o: Alive})
		p.Enqueue(Request{ID: "next", Owner: Owner{2, 2}, Weight: 1, Queued: now.Add(7 * time.Hour)})
		granted, _, e := p.AdvanceObserved(now.Add(7*time.Hour), map[Owner]Liveness{o: state})
		if e != nil {
			t.Fatal(e)
		}
		if (len(granted) == 1) != (state == Dead) {
			t.Fatalf("state %v grants %v", state, granted)
		}
	}
}
func TestHostLedgerDurabilityFairnessAndOwnerBinding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(physicalTemp(t), "ledger")
	l, e := OpenLedger(path, 2)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	o := Owner{1, 1}
	for i, w := range []int{2, 2, 1} {
		if e = l.Enqueue(ctx, Request{ID: fmt.Sprint(i), Owner: o, Weight: w, Class: i, Queued: now.Add(time.Duration(i) * time.Second)}); e != nil {
			t.Fatal(e)
		}
	}
	g, _, e := l.Advance(ctx, now, map[Owner]Liveness{})
	if e != nil || len(g) != 1 || g[0].ID != "0" {
		t.Fatal(g, e)
	}
	l, e = OpenLedger(path, 2)
	if e != nil {
		t.Fatal(e)
	}
	if e = l.Release(ctx, "0", Owner{1, 2}, "wrong identity"); e == nil {
		t.Fatal("owner mismatch released")
	}
	if e = l.Release(ctx, "0", o, "exited"); e != nil {
		t.Fatal(e)
	}
	g, _, e = l.Advance(ctx, now.Add(time.Minute), map[Owner]Liveness{})
	if e != nil || len(g) != 1 || g[0].ID != "1" {
		t.Fatal(g, e)
	}
	if e = l.Cancel(ctx, "1", o, "cannot cancel granted"); e == nil {
		t.Fatal("granted lease cancelled")
	}
	if e = l.Cancel(ctx, "2", Owner{1, 2}, "wrong owner"); e == nil {
		t.Fatal("wrong owner cancelled waiter")
	}
	if e = l.Cancel(ctx, "2", o, "cancelled before launch"); e != nil {
		t.Fatal(e)
	}
	audit, e := l.Audit(ctx)
	if e != nil || len(audit) != 7 {
		t.Fatal(audit, e)
	}
	if _, e = OpenLedger(path, 3); e == nil {
		t.Fatal("conflicting capacity accepted")
	}
	if e = os.WriteFile(filepath.Join(path, "state.json"), []byte("corrupt"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, _, e = l.Advance(ctx, now, map[Owner]Liveness{}); e == nil {
		t.Fatal("corrupt ledger admitted")
	}
}
func TestHostLedgerSymlinksAndContext(t *testing.T) {
	root := physicalTemp(t)
	link := filepath.Join(root, "link")
	if e := os.Symlink(root, link); e != nil {
		t.Fatal(e)
	}
	if _, e := OpenLedger(link, 1); e == nil {
		t.Fatal("symlink opened")
	}
	l, e := OpenLedger(filepath.Join(root, "real"), 1)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = l.Snapshot(ctx); e == nil {
		t.Fatal("cancel ignored")
	}
	if e = os.Remove(filepath.Join(l.path, "state.json")); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(filepath.Join(root, "missing"), filepath.Join(l.path, "state.json")); e != nil {
		t.Fatal(e)
	}
	if _, e = l.Snapshot(context.Background()); e == nil {
		t.Fatal("state symlink opened")
	}
}
func TestHostLedgerProcessHelper(t *testing.T) {
	path := os.Getenv("FAE_LEASE_TEST_PATH")
	if path == "" {
		return
	}
	l, e := OpenLedger(path, 2)
	if e != nil {
		t.Fatal(e)
	}
	id := os.Getenv("FAE_LEASE_TEST_ID")
	now := time.Unix(100, 0)
	if e = l.Enqueue(context.Background(), Request{ID: id, Owner: Owner{1, 1}, Weight: 1, Queued: now}); e != nil {
		t.Fatal(e)
	}
	if _, _, e = l.Advance(context.Background(), now, map[Owner]Liveness{}); e != nil {
		t.Fatal(e)
	}
}
func TestHostLedgerConcurrentProcessesCannotOversubscribe(t *testing.T) {
	path := filepath.Join(physicalTemp(t), "shared")
	l, e := OpenLedger(path, 2)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestHostLedgerProcessHelper$")
			cmd.Env = append(os.Environ(), "FAE_LEASE_TEST_PATH="+path, fmt.Sprintf("FAE_LEASE_TEST_ID=%d", i))
			out, e := cmd.CombinedOutput()
			if e != nil {
				errs <- fmt.Errorf("%v %s", e, out)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	p, e := l.Snapshot(context.Background())
	if e != nil || len(p.Holders) != 2 || len(p.Waiting) != 6 {
		t.Fatal(p, e)
	}
}

func physicalTemp(t *testing.T) string {
	t.Helper()
	p, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return p
}

func TestCanReleaseExhaustiveIdentity(t *testing.T) {
	for pid := -1; pid < 4; pid++ {
		for started := -1; started < 4; started++ {
			for hp := -1; hp < 4; hp++ {
				for hs := -1; hs < 4; hs++ {
					expected := pid > 0 && started > 0 && pid == hp && started == hs
					if CanRelease(pid, started, hp, hs) != expected {
						t.Fatal(pid, started, hp, hs)
					}
				}
			}
		}
	}
}
