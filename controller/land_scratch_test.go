package controller

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// FAE-HOST-020: a landing's gate commands get a TMPDIR in its scratch, which
// is test scratch: it is removed when the landing returns, even when a gate
// command left a read-only tree there (Go's module cache under a test's own
// GOPATH), while the gate output the landing keeps stays. Before, the
// landing never removed it.
func TestLandRemovesTheGateTempATestMadeReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	f := newLandFixture(t)
	r := f.request("temp", f.task())
	r.Policy.Frozen = [][]string{{"sh", "-c", `m="$TMPDIR/gopath/pkg/mod/example.com/m@v1.0.0" && mkdir -p "$m/sub" && echo module m > "$m/go.mod" && chmod -R a-w "$TMPDIR/gopath"`}}
	tmp := filepath.Join(r.Scratch, "tmp")
	t.Cleanup(func() {
		_ = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, _ error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0700)
			}
			return nil
		})
	})
	res, e := Land(context.Background(), r)
	if e != nil || res.Outcome != LandLanded || !gate(res, "frozen").Passed {
		t.Fatalf("%+v %v", res, e)
	}
	if _, e = os.Lstat(tmp); !os.IsNotExist(e) {
		t.Fatal("the gate commands' TMPDIR survived the landing", e)
	}
	kept, e := filepath.Glob(filepath.Join(r.Scratch, "gate-output-*"))
	if e != nil || len(kept) == 0 {
		t.Fatal("the landing's kept gate output was removed", kept, e)
	}
}
