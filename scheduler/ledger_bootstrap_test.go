package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPublishedFreshLedgerLockAlreadyExcludesOtherOpeners(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	first, fresh, err := openLedgerLock(path)
	if err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	defer first.Close()
	finished := make(chan error, 1)
	go func() { _, err := OpenWorkflowLedger(path); finished <- err }()
	select {
	case err := <-finished:
		t.Fatal("another opener acquired before creator initialized state", err)
	case <-time.After(30 * time.Millisecond):
	}
	pool, err := NewPool(1)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ledgerState{Version: 1, Pool: *pool})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(path, "state.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(first.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal("bootstrap handoff failed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("opener did not proceed after initial state")
	}
}
func TestEstablishedLedgerStillRefusesMissingState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger")
	ledger, err := OpenWorkflowLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(path, "state.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenWorkflowLedger(path); err == nil || !strings.Contains(err.Error(), "ledger missing") {
		t.Fatal("missing durable state was silently recreated", err)
	}
	if _, err = ledger.Snapshot(context.Background()); err == nil {
		t.Fatal("existing reader accepted missing durable state")
	}
}
