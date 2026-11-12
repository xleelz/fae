package daemon

import (
	"os"
	"testing"
)

// outsideCheckout is disk-backed scratch outside every repository:
// scripts/test.sh keeps TMPDIR inside the checkout, where delegated secret
// files are refused (FAE-SEC-003).
func outsideCheckout(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "fae-outside-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}
