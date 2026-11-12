package processowner

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// FAE-CP-002/-003: with a grace an owned group is first asked to stop with
// SIGTERM, and killed only when it outlives the grace.
func TestTerminateThenKill(t *testing.T) {
	for _, c := range []struct {
		name, script string
		forced       bool
	}{
		{"graceful", `trap 'echo term > "$0"; exit 0' TERM; while :; do sleep 0.05; done`, false},
		{"ignores-term", `trap '' TERM; while :; do sleep 0.05; done`, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := testDir(t)
			marker := filepath.Join(dir, "terminated")
			cmd := exec.Command("/bin/sh", "-c", c.script, marker)
			r, e := Start(cmd, dir, "owner")
			if e != nil {
				t.Fatal(e)
			}
			defer func() { syscall.Kill(-r.PGID, syscall.SIGKILL); Retire(r) }()
			go cmd.Wait()
			time.Sleep(200 * time.Millisecond) // let the shell install its trap
			grace := 1500 * time.Millisecond
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			started := time.Now()
			if e = terminate(ctx, r, "owner", grace); e != nil {
				t.Fatal(e)
			}
			elapsed := time.Since(started)
			if present, e := groupPresent(r.PGID); e != nil || present {
				t.Fatal("group survived", present, e)
			}
			if c.forced && (elapsed < grace || elapsed > grace+3*time.Second) {
				t.Fatalf("forced kill after %s, grace %s", elapsed, grace)
			}
			if !c.forced {
				if _, e = os.Stat(marker); e != nil || elapsed >= grace {
					t.Fatalf("graceful stop not by SIGTERM within the grace: %v after %s", e, elapsed)
				}
			}
		})
	}
}
