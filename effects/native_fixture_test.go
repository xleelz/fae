package effects

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Adapter integration tests install the policy under test themselves. When the
// whole suite is brokered, do not feed its prepared argv through an enclosing
// layer of wrappers a second time. Only generated wrapper directories go away;
// the actual installed tools and the tested broker's own wrappers remain.
func nativeAdapterFixture(t *testing.T) {
	t.Helper()
	var dirs []string
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if !filepath.IsAbs(dir) {
			continue
		}
		physical, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		if filepath.Base(physical) == "command-shims" {
			continue
		}
		if _, err := os.Stat(filepath.Join(physical, ".policy.json")); err == nil {
			continue
		}
		if filepath.Base(physical) == "bin" && filepath.Base(filepath.Dir(physical)) == "goroot" {
			if _, err := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(physical)), "command-shims", ".policy.json")); err == nil {
				continue
			}
		}
		dirs = append(dirs, dir)
	}
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
}
