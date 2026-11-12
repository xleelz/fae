package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This fixture intentionally pushes only between two temporary local
// repositories to test the engine's push policy. An outer Local suite's Git
// wrapper must not replace that policy with its own blanket push refusal.
func nativeGitPushFixture(t *testing.T) {
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
		dirs = append(dirs, dir)
	}
	t.Setenv("PATH", strings.Join(dirs, string(os.PathListSeparator)))
}
