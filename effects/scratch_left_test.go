package effects

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAE-HOST-020: a scratch or policy directory the broker cannot remove is
// reported through ScratchLeft, not dropped.
func TestBrokerReportsAScratchItCannotRemove(t *testing.T) {
	b, w, _ := setup(t)
	saved := removeRunScratchTree
	t.Cleanup(func() { removeRunScratchTree = saved })
	removeRunScratchTree = func(string) error { return os.ErrPermission }
	left := map[string]error{}
	b.ScratchLeft = func(dir string, err error) { left[dir] = err }
	argv := []string{"/bin/sh", "-c", `printf %s "$TMPDIR"`}
	b.Allowed = [][]string{argv}
	r, err := b.Run(context.Background(), w, Command{Argv: argv, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	policies := 0
	for dir, err := range left {
		if err != os.ErrPermission {
			t.Fatal(dir, err)
		}
		if strings.HasPrefix(filepath.Base(dir), "command-policy-") {
			policies++
		}
		if err = saved(dir); err != nil {
			t.Fatal(err)
		}
	}
	if len(left) != 2 || left[r.Output] == nil || policies != 1 {
		t.Fatalf("unremoved run directories reported: %v", left)
	}
}
