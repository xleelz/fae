package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkflowSchemaUpgradeWaitsForUnawareLiveParticipant(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ledger")
	l, e := OpenDynamicLedger(path, 4)
	if e != nil {
		t.Fatal(e)
	}
	old := exec.Command("sleep", "60")
	if e = old.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { _ = old.Process.Kill(); _ = old.Wait() }()
	owner, e := processOwner(old.Process.Pid)
	if e != nil {
		t.Fatal(e)
	}
	e = l.transact(ctx, true, func(s *ledgerState) error {
		s.Pool.Holders = append(s.Pool.Holders, Lease{Request: Request{ID: "legacy", Owner: owner, Weight: 1}})
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = OpenWorkflowLedger(path); e == nil || !strings.Contains(e.Error(), readerKey(owner)) {
		t.Fatal("unaware live participant must block upgrade", e)
	}
	raw, e := os.ReadFile(filepath.Join(path, "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	var s ledgerState
	if e = json.Unmarshal(raw, &s); e != nil || s.Version != 1 {
		t.Fatal(s.Version, e)
	}
	if _, e = OpenDynamicLedger(path, 4); e != nil {
		t.Fatal("legacy-compatible restart failed", e)
	}
	if e = old.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = old.Wait()
	if _, e = OpenWorkflowLedger(path); e != nil {
		t.Fatal("dead participant blocked upgrade", e)
	}
	raw, _ = os.ReadFile(filepath.Join(path, "state.json"))
	_ = json.Unmarshal(raw, &s)
	if s.Version != 2 {
		t.Fatal(s.Version)
	}
}
