package commandshim

import (
	"errors"
	"path/filepath"
)

// LanguageCommandKind classifies only supported invocations, before a role
// queues for a host lease. Full path and option validation still runs in Prepare.
func LanguageCommandKind(argv []string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	if kind, ok := RustCommandKind(argv); ok {
		return kind, ok
	}
	if kind, ok := PythonCommandKind(argv); ok {
		return kind, ok
	}
	return TypeScriptCommandKind(argv)
}

func (p Policy) prepareLanguage(argv, env []string) ([]string, bool, error) {
	if len(argv) == 0 {
		return nil, false, nil
	}
	var out []string
	var err error
	switch filepath.Base(argv[0]) {
	case "cargo", "rustc", "rustfmt":
		out, err = p.prepareRust(argv)
	case "pytest", "python", "python3", "ruff":
		if filepath.Base(argv[0]) == "python" || filepath.Base(argv[0]) == "python3" {
			if _, ok := PythonCommandKind(argv); !ok {
				return nil, false, nil
			}
		}
		out, err = PreparePython(p, argv)
	case "node", "tsc":
		if filepath.Base(argv[0]) == "node" {
			if _, ok := TypeScriptCommandKind(argv); !ok {
				return nil, false, nil
			}
		}
		out, err = p.PrepareTypeScript(argv, env)
	default:
		return nil, false, nil
	}
	if err != nil {
		var refusal *Refusal
		if !errors.As(err, &refusal) {
			err = refuse(err.Error())
		}
	}
	return out, true, err
}

// LanguageEnvironment pins tool settings before PATH wrappers are installed.
// Generic interpreters are not globally shadowed: provider CLIs may use them
// in their shebangs. Interpreter-based tool runs use HostCommand stages.
func LanguageEnvironment(argv, env []string, scratch string) ([]string, error) {
	return LanguageEnvironmentWithTools(argv, env, scratch, nil)
}

func LanguageEnvironmentWithTools(argv, env []string, scratch string, tools map[string]string) ([]string, error) {
	if len(argv) == 0 {
		return env, nil
	}
	switch filepath.Base(argv[0]) {
	case "cargo", "rustc", "rustfmt":
		if tools["rustc"] != "" {
			return RustEnvironmentWithCompiler(argv, env, scratch, tools["rustc"])
		}
		return RustEnvironment(argv, env, scratch)
	case "pytest", "python", "python3", "ruff":
		return PythonEnvironment(argv, env, scratch)
	case "node", "tsc":
		return TypeScriptEnvironment(argv, env, scratch)
	}
	return env, nil
}
