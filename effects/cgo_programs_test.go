package effects

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAE-HOST-014: an account-mode run's nested cgo build runs the C compiler
// on the run's own PATH, not one a test puts first on the PATH it gives the
// nested go. Here the command builds a cgo package with CGO_ENABLED=1, no CC
// and a fake gcc first on its PATH: the build uses the host's gcc and the
// fake never runs. Before, go found the fake by name and ran it.
func TestBrokerNestedCgoBuildRunsTheRunsCompiler(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	if _, err := exec.LookPath("gcc"); err != nil {
		t.Skip("no gcc on PATH: nested cgo builds need the host's C compiler")
	}
	// The nested build deliberately writes its binary under TMPDIR. Keep the
	// fixture on disk so the RAM-output refusal is not the behavior under test.
	disk, err := os.MkdirTemp("/var/tmp", "fae-cgo-program-")
	if err != nil {
		t.Skipf("on-disk temporary directory unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(disk) })
	t.Setenv("TMPDIR", disk)
	b, w, _ := setup(t)
	if err := os.Mkdir(filepath.Join(w.Path, "cgopkg"), 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(w.Path, "cgopkg"), "c.go", "package main\n\n// int seven(void) { return 7; }\nimport \"C\"\n\nfunc main() { println(C.seven()) }\n")
	marker := filepath.Join(t.TempDir(), "fake-gcc-ran")
	script := `mkdir "$TMPDIR/fakebin" && printf '#!/bin/sh\ntouch %s\nexit 1\n' "$1" > "$TMPDIR/fakebin/gcc" && chmod 700 "$TMPDIR/fakebin/gcc" && PATH="$TMPDIR/fakebin:$PATH" CGO_ENABLED=1 go build -o "$TMPDIR/seven" ./cgopkg; echo "build=$?"`
	b.Allowed = [][]string{{"/bin/sh", "-c", script, "sh", marker}}
	accountMode(t, b)
	r, err := b.Run(nestedRunContext(true), w, Command{Argv: b.Allowed[0], Timeout: 5 * time.Minute})
	if err != nil {
		t.Fatalf("%v %s", err, r.Stderr)
	}
	if _, err = os.Stat(marker); err == nil {
		t.Fatalf("a nested cgo build ran the gcc the test put first on its PATH:\n%s\n%s", r.Output, r.Stderr)
	}
	built := false
	for _, c := range r.Nested {
		built = built || len(c.Argv) > 2 && c.Argv[0] == "go" && c.Argv[1] == "build" && c.Argv[len(c.Argv)-1] == "./cgopkg"
	}
	if !strings.Contains(r.Output, "build=0") || !built {
		t.Fatalf("nested cgo build with the run's gcc (accounted %t, %d calls):\n%s\n%s", built, r.NestedCount, r.Output, r.Stderr)
	}
}
