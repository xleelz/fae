package workflow

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

func ValidateShellAllow(shapes [][]string) error {
	for _, shape := range shapes {
		if len(shape) == 0 || shape[0] == "" {
			return fmt.Errorf("shell allow shape needs an executable")
		}
		for i, a := range shape {
			if a == "*" && i == len(shape)-1 {
				continue
			}
			if strings.ContainsAny(a, "\x00\n\r") || a == "*" || strings.Contains(a, "{{") {
				return fmt.Errorf("invalid shell allow argument %q", a)
			}
		}
		if shape[0] == "sh" || shape[0] == "bash" || shape[0] == "zsh" {
			return fmt.Errorf("shell wrappers cannot be allowlisted")
		}
	}
	return nil
}
func ShellAllowNarrows(child, parent [][]string) bool {
	if parent == nil {
		return true
	}
	for _, c := range child {
		found := false
		for _, p := range parent {
			if shapeContains(p, c) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func shapeContains(shape, args []string) bool {
	wildcard := len(shape) > 0 && shape[len(shape)-1] == "*"
	n := len(shape)
	if wildcard {
		n--
	}
	if len(args) < n || !wildcard && len(args) != n {
		return false
	}
	for i := 0; i < n; i++ {
		if shape[i] != args[i] {
			return false
		}
	}
	return true
}

// SimpleShellArgv accepts literal words and quoting, never shell evaluation.
func SimpleShellArgv(command string) ([]string, error) {
	var out []string
	var word strings.Builder
	quote := rune(0)
	started := false
	escaped := false
	for _, r := range command {
		if r == '\n' || r == '\r' {
			return nil, fmt.Errorf("multiple command lines refused")
		}
		if escaped {
			word.WriteRune(r)
			started = true
			escaped = false
			continue
		}
		if quote == '\'' {
			if r == '\'' {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			continue
		}
		if r == '\\' {
			escaped = true
			started = true
			continue
		}
		if quote == '"' {
			if r == '"' {
				quote = 0
				continue
			}
			if r == '$' || r == '`' {
				return nil, fmt.Errorf("shell expansion refused")
			}
			word.WriteRune(r)
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			started = true
			continue
		}
		if unicode.IsSpace(r) {
			if started {
				out = append(out, word.String())
				word.Reset()
				started = false
			}
			continue
		}
		if strings.ContainsRune(";|&<>()$`\n\r", r) {
			return nil, fmt.Errorf("only one literal simple command is permitted")
		}
		word.WriteRune(r)
		started = true
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("unterminated shell quoting")
	}
	if started {
		out = append(out, word.String())
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty shell command")
	}
	if strings.Contains(out[0], "=") || slices.Contains([]string{"cd", "eval", "exec", "command", "env", "source", ".", "sh", "bash", "zsh", "dash"}, filepath.Base(out[0])) {
		return nil, fmt.Errorf("shell wrappers and environment assignments refused")
	}
	return out, nil
}

// CheckShellCommand returns literal argv under a restricted shell profile.
// An unrestricted profile retains its existing shell behavior unless test runs
// are explicitly denied; named target runners supplement the common runners.
func CheckShellCommand(profile Profile, command string, testCommands [][]string) ([]string, error) {
	if profile.ShellAllow == nil && !profile.DenyTestRuns {
		return nil, nil
	}
	argv, err := SimpleShellArgv(command)
	if err != nil {
		return nil, err
	}
	if profile.DenyTestRuns {
		base := filepath.Base(argv[0])
		isTest := slices.Contains([]string{"pytest", "py.test", "pytest-3", "cargo-nextest", "ctest", "jest", "vitest"}, base)
		if (base == "go" || base == "cargo" || base == "npm" || base == "yarn" || base == "pnpm" || base == "bun") && len(argv) > 1 && (argv[1] == "test" || argv[1] == "nextest") {
			isTest = true
		}
		if (base == "python" || base == "python3") && len(argv) > 2 && argv[1] == "-m" && argv[2] == "pytest" {
			isTest = true
		}
		for _, v := range testCommands {
			if len(v) > 0 && len(argv) >= len(v) && slices.Equal(argv[:len(v)], v) {
				isTest = true
			}
		}
		if isTest {
			return nil, fmt.Errorf("test runner denied by profile")
		}
	}
	if profile.ShellAllow != nil {
		ok := false
		for _, shape := range profile.ShellAllow {
			if shapeContains(shape, argv) {
				ok = true
				break
			}
		}
		if !ok {
			return nil, fmt.Errorf("command does not match shell_allow")
		}
	}
	return argv, nil
}
