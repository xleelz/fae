package commandshim

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
)

// The programs a go build runs by name for cgo (FAE-HOST-014): go looks up
// the C compiler, the C++ compiler, the Fortran compiler and pkg-config on
// its PATH when CC, CXX, FC or PKG_CONFIG is unset or names a program
// without a directory, and a nested call's PATH is the test's. A nested go
// call therefore gets each such program pinned to the absolute path the
// run's own PATH gives (NestedGo.Programs, accountedCgoPrograms), so a
// program a test puts first on its PATH is never the one go runs.

// cgoProgramKeys are the variables naming a program go runs for cgo.
var cgoProgramKeys = []string{"CC", "CXX", "FC", "PKG_CONFIG"}

// cgoDefaultProgramNames are every name cgoDefaultProgram can give.
var cgoDefaultProgramNames = []string{"gcc", "g++", "clang", "clang++", "gfortran", "pkg-config"}

// cgoDefaultProgram is the program go runs for key when key is unset or
// empty, as cmd/go chooses it for the target system goos (cfg.DefaultCC,
// DefaultCXX, gfortran and DefaultPkgConfig).
func cgoDefaultProgram(key, goos string) string {
	clang := goos == "darwin" || goos == "ios" || goos == "freebsd" || goos == "openbsd"
	switch {
	case key == "CC" && clang:
		return "clang"
	case key == "CC":
		return "gcc"
	case key == "CXX" && clang:
		return "clang++"
	case key == "CXX":
		return "g++"
	case key == "FC":
		return "gfortran"
	}
	return "pkg-config"
}

// NestedGoPrograms returns, for a brokered run's environment env, the
// absolute path on env's PATH of every program a nested go call may find by
// name for cgo: go's defaults (cgoDefaultProgramNames) and the program the
// run's own CC, CXX, FC or PKG_CONFIG names without a directory. A program
// the run's PATH does not have is left out.
func NestedGoPrograms(env []string) map[string]string {
	names := slices.Clone(cgoDefaultProgramNames)
	for _, key := range cgoProgramKeys {
		if v, ok := lastGoEnv(env, key); ok {
			if fields, err := goQuotedFields(v); err == nil && len(fields) > 0 && cgoProgramByName(fields[0]) {
				names = append(names, fields[0])
			}
		}
	}
	path, programs := BasePath(env), map[string]string{}
	for _, name := range names {
		if p := resolveShimTool(name, "", path); p != "" {
			programs[name] = p
		}
	}
	return programs
}

// cgoProgramByName reports whether go finds program by name on its PATH:
// it has no directory. go refuses a relative path with one.
func cgoProgramByName(program string) bool {
	return program != "" && program == filepath.Base(program) && !filepath.IsAbs(program)
}

// cgoProgram is the run's own path for the program name, "" for none.
func (g *NestedGo) cgoProgram(name string) string {
	if g == nil {
		return ""
	}
	return g.Programs[name]
}

// cgoPinned reports whether value is what accountedCgoPrograms sets key to:
// a program the run pinned, then the run's own value's arguments, if any. A
// nested call inherits it, so a call it starts again may keep it.
func (g *NestedGo) cgoPinned(key, value string) bool {
	if g == nil || !slices.Contains(cgoProgramKeys, key) {
		return false
	}
	fields, err := goQuotedFields(value)
	if err != nil || len(fields) == 0 || !filepath.IsAbs(fields[0]) {
		return false
	}
	pinned := false
	for _, p := range g.Programs {
		pinned = pinned || p == fields[0]
	}
	var own []string
	if v, ok := g.Env[key]; ok {
		if own, err = goQuotedFields(v); err != nil {
			return false
		}
	}
	if len(own) > 0 {
		own = own[1:]
	}
	return pinned && slices.Equal(fields[1:], own)
}

// goQuotedJoin joins fields as go's quoted.Join does, the inverse of
// goQuotedFields: a field with a space or a quote is quoted with the quote
// it does not hold.
func goQuotedJoin(fields []string) (string, error) {
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteByte(' ')
		}
		space, single, double := strings.ContainsAny(f, " \t\n\r"), strings.Contains(f, "'"), strings.Contains(f, `"`)
		switch {
		case !space && !single && !double:
			b.WriteString(f)
		case !single:
			b.WriteString("'" + f + "'")
		case !double:
			b.WriteString(`"` + f + `"`)
		default:
			return "", errors.New("an argument holds both quotes")
		}
	}
	return b.String(), nil
}

// withGoEnv is env with every entry of key replaced by one key=value.
func withGoEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}
