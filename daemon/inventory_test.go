package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xleelz/fae/processowner"
)

func defunctChildren(t *testing.T) map[string]bool {
	t.Helper()
	all, err := processowner.Census()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, p := range all {
		if p.PPID == os.Getpid() && p.Defunct() {
			out[p.Start] = true
		}
	}
	return out
}

// FAE-HOST-021/-023 acceptance: the owned Dolt child is an inventory record
// with its role; after shutdown no inventory process remains and the daemon
// left no defunct child.
func TestShutdownLeavesNoInventoryProcessOrZombie(t *testing.T) {
	cfg, ctx := fixture(t)
	before := defunctChildren(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(d.layout.Run, "plane-processes")
	inventory, err := processowner.Inventory(registry)
	if err != nil || len(inventory) != 1 || inventory[0].Role != "dolt" || inventory[0].PID != d.child.PID() || inventory[0].ParentPID != os.Getpid() {
		t.Fatalf("inventory %+v %v", inventory, err)
	}
	stop(t, d)
	if after, err := processowner.Inventory(registry); err != nil || len(after) != 0 {
		t.Fatal("inventory process remains", after, err)
	}
	if !processowner.ProvedDead(inventory[0].PID, inventory[0].Birth) {
		t.Fatal("dolt survived shutdown")
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		leaked := []string{}
		for start := range defunctChildren(t) {
			if !before[start] {
				leaked = append(leaked, start)
			}
		}
		if len(leaked) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("defunct children after shutdown", leaked)
		}
	}
}
