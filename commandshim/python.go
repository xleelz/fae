package commandshim

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

var pythonName = regexp.MustCompile(`^python(?:3(?:\.[0-9]+)?)?$`)

// PythonCommandKind recognizes only explicit, supported Python tool forms.
// It never treats an arbitrary script, -c string or Python module as a build.
func PythonCommandKind(argv []string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	name := filepath.Base(argv[0])
	if name == "pytest" {
		return "test", true
	}
	if name == "ruff" && len(argv) > 1 {
		switch argv[1] {
		case "check":
			return "build", true
		case "format":
			return "format", true
		}
		return "", false
	}
	if !pythonName.MatchString(name) {
		return "", false
	}
	i := 1
	if i < len(argv) && argv[i] == "-B" {
		i++
	}
	if i+1 >= len(argv) || argv[i] != "-m" {
		return "", false
	}
	switch argv[i+1] {
	case "pytest":
		return "test", true
	case "compileall", "py_compile":
		return "build", true
	}
	return "", false
}
func pythonArguments(argv []string) (string, int) {
	if filepath.Base(argv[0]) == "pytest" {
		return "pytest", 1
	}
	if filepath.Base(argv[0]) == "ruff" {
		return "ruff-" + argv[1], 2
	}
	i := 1
	if argv[i] == "-B" {
		i++
	}
	return argv[i+1], i + 2
}

// PreparePython applies the same physical scope, code-role and lease guard as
// the core adapters. It accepts scoped pytest selectors, stdlib compilation,
// and Ruff's isolated baseline; caller-controlled plugins and output paths are
// deliberately excluded. The broker owns evidence and cache destinations.
func PreparePython(p Policy, argv []string) ([]string, error) {
	if _, ok := PythonCommandKind(argv); !ok {
		return nil, refuse("unsupported Python command; use pytest, python -m pytest/compileall/py_compile, or ruff check/format")
	}
	root, err := filepath.EvalSymlinks(p.Worktree)
	if err != nil {
		return nil, err
	}
	cwd, err := filepath.EvalSymlinks(p.CWD)
	if err != nil {
		return nil, err
	}
	p.Worktree, p.CWD = root, cwd
	if !CanExecute(p.CodeRole, inside(root, cwd), true, p.LeaseHeld, p.Nested) {
		return nil, refuse("code role, own worktree, a heavy lease and non-nested dispatch are required")
	}
	tool, start := pythonArguments(argv)
	out := append([]string(nil), argv[:start]...)
	targets := 0
	switch tool {
	case "pytest":
		out = append(out, "-o", "addopts=")
	case "compileall":
		out = append(out, "-q", "-f", "-j", "1")
	case "ruff-check", "ruff-format":
		out = append(out, "--isolated", "--no-cache")
	}
	for i := start; i < len(argv); i++ {
		arg := argv[i]
		if strings.HasPrefix(arg, "-") {
			switch tool {
			case "pytest":
				switch {
				case arg == "-o":
					i++
					if i >= len(argv) || argv[i] != "addopts=" {
						return nil, refuse("pytest configuration overrides are broker-owned")
					}
				case arg == "-q" || arg == "-v" || arg == "-vv" || arg == "-x" || arg == "--strict-markers" || arg == "--strict-config" || arg == "--disable-warnings" || arg == "--no-header" || arg == "--no-summary" || arg == "--showlocals":
					out = append(out, arg)
				case strings.HasPrefix(arg, "--tb="):
					if arg != "--tb=short" && arg != "--tb=long" && arg != "--tb=line" && arg != "--tb=no" && arg != "--tb=auto" && arg != "--tb=native" {
						return nil, refuse("unsupported pytest traceback mode")
					}
					out = append(out, arg)
				case strings.HasPrefix(arg, "--maxfail="):
					if !positivePythonInteger(strings.TrimPrefix(arg, "--maxfail=")) {
						return nil, refuse("pytest maxfail must be positive")
					}
					out = append(out, arg)
				default:
					return nil, refuse("unsupported pytest flag (selection, plugins, parallel workers and report paths are broker-owned): " + arg)
				}
			case "compileall":
				if arg == "-q" || arg == "-f" || arg == "-l" {
					if arg == "-l" {
						out = append(out, arg)
					}
				} else if arg == "-j" {
					i++
					if i >= len(argv) || argv[i] != "1" {
						return nil, refuse("compileall workers must be exactly 1")
					}
				} else if arg == "-j1" {
				} else {
					return nil, refuse("unsupported compileall flag; workers and output locations are broker-owned")
				}
			case "ruff-check", "ruff-format":
				if arg == "--check" && tool == "ruff-format" || arg == "--diff" || arg == "-q" || arg == "--quiet" || arg == "--no-cache" || arg == "--isolated" {
					if arg != "--no-cache" && arg != "--isolated" {
						out = append(out, arg)
					}
				} else {
					return nil, refuse("unsupported Ruff flag; use the isolated check/format baseline")
				}
			default:
				return nil, refuse("py_compile accepts explicit file operands only")
			}
			continue
		}
		if strings.HasPrefix(arg, "@") || arg == "" {
			return nil, refuse("Python response files and empty operands are unsupported")
		}
		path := arg
		if tool == "pytest" {
			path, _, _ = strings.Cut(arg, "::")
		}
		if _, err := p.scoped(path); err != nil {
			return nil, err
		}
		if strings.ContainsAny(path, "*?[]") || strings.Contains(path, "...") {
			return nil, refuse("Python paths must be literal scoped paths")
		}
		out = append(out, arg)
		targets++
	}
	if targets == 0 {
		return nil, refuse("Python commands require explicit scoped file or directory targets")
	}
	return out, nil
}
func positivePythonInteger(s string) bool {
	if len(s) == 0 || len(s) > 6 {
		return false
	}
	positive := false
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
		positive = positive || c != '0'
	}
	return positive
}

// PythonEnvironment returns the required overrides, merged by key by the
// broker after clearing inherited PYTHON*/PYTEST*/RUFF* settings.
func PythonEnvironment(argv, env []string, cache string) ([]string, error) {
	if _, ok := PythonCommandKind(argv); !ok {
		return append([]string(nil), env...), nil
	}
	if !filepath.IsAbs(cache) || filepath.Clean(cache) != cache {
		return nil, fmt.Errorf("Python cache root must be canonical absolute")
	}
	overrides := []string{"PYTHONPYCACHEPREFIX=" + filepath.Join(cache, "pycache"), "PYTHONDONTWRITEBYTECODE=1", "PYTHONNOUSERSITE=1", "PYTEST_DISABLE_PLUGIN_AUTOLOAD=1", "PYTEST_ADDOPTS=", "RUFF_CACHE_DIR=" + filepath.Join(cache, "ruff"), "RAYON_NUM_THREADS=1", "OMP_NUM_THREADS=1", "OPENBLAS_NUM_THREADS=1", "MKL_NUM_THREADS=1", "NUMEXPR_NUM_THREADS=1"}
	out := []string{}
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "PYTHON") || strings.HasPrefix(key, "PYTEST") || strings.HasPrefix(key, "RUFF") {
			continue
		}
		drop := false
		for _, override := range overrides {
			wanted, _, _ := strings.Cut(override, "=")
			drop = drop || key == wanted
		}
		if !drop {
			out = append(out, entry)
		}
	}
	return append(out, overrides...), nil
}
