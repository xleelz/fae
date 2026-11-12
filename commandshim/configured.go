package commandshim

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PrepareConfigured validates the common host-command execution boundary
// without interpreting a verifier's or future tool's option grammar. The caller
// MUST first match the complete vector against the target's exact allowlist;
// merely labelling a command as formal evidence does not authorize it.
func (p Policy) PrepareConfigured(argv []string) ([]string, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, refuse("configured command requires a literal executable")
	}
	if len(argv) > 1024 {
		return nil, refuse("configured command exceeds argument-count bound")
	}
	size := 0
	for _, arg := range argv {
		size += len(arg) + 1
		if strings.ContainsAny(arg, "\x00\n\r") {
			return nil, refuse("configured command arguments must be literal single-line values")
		}
		if len(arg) > 128<<10 {
			return nil, refuse("configured command argument exceeds executable argument bound")
		}
	}
	if size > 1<<20 {
		return nil, refuse("configured command vector exceeds executable argument bound")
	}
	if !filepath.IsAbs(p.Worktree) {
		return nil, refuse("configured command requires an absolute physical worktree")
	}
	root, err := filepath.EvalSymlinks(p.Worktree)
	if err != nil {
		return nil, err
	}
	cwd := p.CWD
	if cwd == "" {
		cwd = root
	}
	physicalCWD, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return nil, err
	}
	if filepath.Clean(p.Worktree) != root || filepath.Clean(cwd) != physicalCWD {
		return nil, refuse("configured command requires a physical worktree and working directory")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, refuse("configured command worktree is not a directory")
	}
	if !CanExecute(p.CodeRole, inside(root, physicalCWD), true, p.LeaseHeld, p.Nested) {
		return nil, refuse("configured command requires code role, own worktree, a heavy lease and non-nested dispatch")
	}
	if name := filepath.Base(argv[0]); name == "git" || name == "git.exe" {
		return nil, refuse("configured VCS commands require the dedicated VCS broker")
	}
	result := append([]string{}, argv...)
	if !filepath.IsAbs(result[0]) && strings.ContainsAny(result[0], "/\\") {
		executable, err := resolve(physicalCWD, result[0])
		if err != nil {
			return nil, err
		}
		if !inside(root, executable) {
			return nil, refuse("configured relative executable escapes own worktree")
		}
		file, err := os.Stat(executable)
		if err != nil || !file.Mode().IsRegular() || file.Mode().Perm()&0111 == 0 {
			return nil, refuse("configured worktree executable is not an executable regular file")
		}
		result[0] = executable
	}
	// No strings are joined or passed through a shell, and no language flags are
	// silently inserted: the configured exact vector remains its execution plan.
	return result, nil
}

// ConfiguredEnvironment preserves the explicitly admitted environment. It is
// intentionally distinct from language-specific grammars: e.g. cargo kani and
// go run verifier tools are not Cargo-build/Go-test adapters. The shared broker
// still supplies its sanitized base and owns the whole process group.
func ConfiguredEnvironment(argv, env []string, scratch string) ([]string, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("configured environment requires command identity")
	}
	out := append([]string{}, env...)
	for _, entry := range out {
		key, _, ok := strings.Cut(entry, "=")
		if !ok || key == "" || strings.ContainsRune(entry, 0) {
			return nil, refuse("malformed configured command environment")
		}
	}
	return out, nil
}
