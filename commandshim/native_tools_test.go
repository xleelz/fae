package commandshim

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Native adapter tests validate one prepared command against an installed tool.
// An enclosing accounted suite may prepend another layer of Fae wrappers. Those
// are not installed compilers and must not re-parse the already-prepared vector.
func nativeToolTestEnv() []string {
	env := os.Environ()
	var dirs []string
	for _, dir := range filepath.SplitList(BasePath(env)) {
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
	var out []string
	for _, item := range env {
		if !strings.HasPrefix(item, "PATH=") {
			out = append(out, item)
		}
	}
	return append(out, "PATH="+strings.Join(dirs, string(os.PathListSeparator)))
}

func nativeToolForTest(t *testing.T, name, variable string) (string, []string) {
	t.Helper()
	env := nativeToolTestEnv()
	tool := os.Getenv(variable)
	if tool != "" {
		if !filepath.IsAbs(tool) {
			t.Fatalf("%s must name an absolute native executable", variable)
		}
		st, err := os.Stat(tool)
		if err != nil || !st.Mode().IsRegular() || st.Mode()&0111 == 0 {
			t.Fatalf("%s must name an installed executable", variable)
		}
	} else {
		tool = resolveShimTool(name, "", BasePath(env))
	}
	if tool == "" {
		t.Skipf("installed %s required; no package downloads", name)
	}
	return tool, env
}
