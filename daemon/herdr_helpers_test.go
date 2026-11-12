package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// testHerdr is the injected Herdr binary for multiplexer tests (FAE-VER-006):
// FAE_TEST_HERDR_BINARY when set, else herdr-core on the test PATH; never a path
// under the developer's home. A wrong explicit injection fails rather than skips.
func testHerdr(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("FAE_TEST_HERDR_BINARY"); p != "" {
		if info, err := os.Stat(p); !filepath.IsAbs(p) || err != nil || info.IsDir() {
			t.Fatalf("FAE_TEST_HERDR_BINARY=%q is not an absolute Herdr executable: %v", p, err)
		}
		return p
	}
	p, err := exec.LookPath("herdr-core")
	if err != nil {
		p, err = exec.LookPath("herdr")
		if err != nil {
			t.Skip("herdr-core required: set FAE_TEST_HERDR_BINARY or put herdr-core on PATH")
		}
	}
	if p, err = filepath.Abs(p); err != nil {
		t.Fatal(err)
	}
	return p
}
