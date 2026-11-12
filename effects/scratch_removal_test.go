package effects

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAE-HOST-020: a run that leaves a read-only tree in its scratch (Go's
// module cache under a test's own GOPATH: 0555 directories and 0444 files;
// a directory nobody may list) still has its scratch removed, whatever its
// exit, and a link it left to a read-only directory outside the scratch is
// not followed. Before, the broker ignored os.RemoveAll's error and left the
// tree, and the next daemon start's sweep then failed on it.
func TestBrokerRemovesAScratchTreeATestMadeReadOnly(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	b, w, _ := setup(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(outside, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outside, 0700) })
	parent := filepath.Dir(w.Path)
	for _, exit := range []int{0, 3} {
		script := `m="$TMPDIR/gopath/pkg/mod/example.com/m@v1.0.0" && mkdir -p "$m/sub" "$TMPDIR/locked/inner" && echo module m > "$m/go.mod" && ln -s ` + outside + ` "$m/outside" && chmod -R a-w "$TMPDIR/gopath" && chmod 000 "$TMPDIR/locked" && printf %s "$TMPDIR"; exit ` + map[int]string{0: "0", 3: "3"}[exit]
		argv := []string{"/bin/sh", "-c", script}
		b.Allowed = [][]string{argv}
		r, err := b.Run(context.Background(), w, Command{Argv: argv, Timeout: 10 * time.Second})
		if err != nil || r.ExitCode != exit {
			t.Fatalf("exit %d: %+v %v", exit, r, err)
		}
		if filepath.Dir(r.Output) != parent || !strings.HasPrefix(filepath.Base(r.Output), "command-tmp-") {
			t.Fatal("scratch not beside the worktree", r.Output)
		}
		if _, err = os.Lstat(r.Output); !os.IsNotExist(err) {
			// Leave nothing read-only behind for t.TempDir's cleanup.
			_ = filepath.WalkDir(r.Output, func(p string, d os.DirEntry, _ error) error {
				if d != nil && d.IsDir() {
					_ = os.Chmod(p, 0700)
				}
				return nil
			})
			t.Fatalf("exit %d: a scratch holding a read-only tree survived the run: %v", exit, err)
		}
	}
	if residue, err := filepath.Glob(filepath.Join(parent, "command-*")); err != nil || len(residue) != 0 {
		t.Fatal("run directories left beside the worktree", residue, err)
	}
	if info, err := os.Stat(outside); err != nil || info.Mode().Perm() != 0555 {
		t.Fatal("a directory outside the scratch changed through a link", info, err)
	}
}
