package effects

import (
	"context"
	"strings"
	"testing"
)

// FAE-SEC-005, FAE-VCS-006: a reviewed snapshot's files are file names, not
// pathspec patterns. Before, a declared path such as "*" or ":(top)" (both
// pass the declared-path rule) made git stage every changed file, so the
// "unreviewed changes outside declared files" check passed whatever the task
// changed. Now such a name matches only a file of that name: with no such
// file the snapshot is refused, and with one, a change to any other file is
// refused as unreviewed.
func TestSnapshotFilesAreLiteralNotPathspecs(t *testing.T) {
	for _, name := range []string{"*", "*.go", ":(top)x.go", "[x].go"} {
		t.Run(name, func(t *testing.T) {
			b, w, _ := setup(t)
			write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
			if _, err := b.Snapshot(context.Background(), w, []string{name}); err == nil {
				t.Fatal("a pathspec-shaped declared path staged an undeclared change")
			}
		})
	}
	b, w, _ := setup(t)
	write(t, w.Path, "[x].go", "package test\n")
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	if _, err := b.Snapshot(context.Background(), w, []string{"[x].go"}); err == nil || !strings.Contains(err.Error(), "unreviewed changes outside declared files") {
		t.Fatalf("a file named like a pattern staged another file's change: %v", err)
	}
	b, w, _ = setup(t)
	write(t, w.Path, "x.go", "package test\nfunc X() int { return 2 }\n")
	if _, err := b.Snapshot(context.Background(), w, []string{"x.go"}); err != nil {
		t.Fatal("a declared file's change refused:", err)
	}
}
