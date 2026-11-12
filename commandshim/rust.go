package commandshim

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RustCommandKind recognizes only the supported native Rust tool surface.
func RustCommandKind(argv []string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	switch filepath.Base(argv[0]) {
	case "rustfmt":
		return "format", true
	case "cargo":
		if len(argv) > 1 {
			switch argv[1] {
			case "build", "check":
				return "build", true
			case "test":
				return "test", true
			}
		}
	}
	return "", false
}

var rustName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_-]*$`)
var rustExternalMod = regexp.MustCompile(`(?m)\bmod\s+[A-Za-z_][A-Za-z0-9_]*\s*;|#\s*\[\s*path\s*=`)
var rustUnsafeManifestKey = regexp.MustCompile(`^["']?(?:build|links|harness|proc-macro|workspace)["']?\s*=`)
var rustWorkspaceHeader = regexp.MustCompile(`^\[\s*["']?workspace`)
var rustPath = regexp.MustCompile(`\bpath\s*=\s*["']([^"']+)["']`)

func (p Policy) prepareRust(args []string) ([]string, error) {
	if filepath.Base(args[0]) == "rustfmt" {
		return p.prepareRustfmt(args)
	}
	if _, ok := RustCommandKind(args); !ok {
		return nil, refuse("unsupported Cargo operation")
	}
	out := []string{args[0], args[1]}
	manifest := filepath.Join(p.CWD, "Cargo.toml")
	harness := false
	for i := 2; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			harness = true
			for _, v := range args[i+1:] {
				if v != "--exact" && v != "--test-threads=1" && v != "--color=never" && !rustTestName(v) {
					return nil, refuse("unsupported Rust test harness option")
				}
			}
			out = append(out, args[i:]...)
			break
		}
		switch a {
		case "--locked", "--offline", "--frozen", "--release", "--lib", "--tests", "--no-default-features", "--all-features", "--color=never":
			out = append(out, a)
		case "-j", "--jobs":
			if i+1 >= len(args) {
				return nil, refuse("missing jobs value")
			}
			i++
		case "--manifest-path", "--test", "--bin", "--package", "-p", "--features", "-F":
			if i+1 >= len(args) {
				return nil, refuse("missing Cargo option value")
			}
			i++
			v := args[i]
			if a == "--manifest-path" {
				r, e := p.scoped(v)
				if e != nil {
					return nil, e
				}
				if !inside(p.Worktree, r) || filepath.Base(r) != "Cargo.toml" {
					return nil, refuse("Cargo manifest must belong to this worktree")
				}
				manifest = r
				v = r
			} else if !rustName.MatchString(v) {
				return nil, refuse("Cargo target/package/features must be a literal name")
			}
			out = append(out, a, v)
		default:
			if strings.HasPrefix(a, "--jobs=") || strings.HasPrefix(a, "-j") {
				continue
			}
			if args[1] == "test" && rustTestName(a) && !strings.HasPrefix(a, "-") {
				out = append(out, a)
				continue
			}
			return nil, refuse("unsupported Cargo argument: " + a)
		}
	}
	if err := p.validateRustManifest(manifest); err != nil {
		return nil, err
	}
	if args[1] == "test" {
		target := false
		for _, a := range out {
			if a == "--lib" || a == "--tests" || a == "--test" || a == "--bin" {
				target = true
			}
		}
		if !target {
			for i, a := range out {
				if a == "--" {
					out = append(out[:i], append([]string{"--tests"}, out[i:]...)...)
					target = true
					break
				}
			}
			if !target {
				out = append(out, "--tests")
			}
		}
	}
	// Cargo options precede the libtest delimiter.
	n := len(out)
	for i, v := range out {
		if v == "--" {
			n = i
			break
		}
	}
	tail := append([]string(nil), out[n:]...)
	out = append(out[:n], "--jobs=1", "--offline", "--locked", "--color=never")
	out = append(out, tail...)
	if args[1] == "test" && !harness {
		out = append(out, "--", "--test-threads=1", "--color=never")
	}
	return out, nil
}
func rustTestName(v string) bool {
	if v == "" || len(v) > 512 {
		return false
	}
	for _, r := range v {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == ':') {
			return false
		}
	}
	return true
}
func (p Policy) validateRustManifest(manifest string) error {
	return p.validateRustManifestSeen(manifest, map[string]bool{})
}
func (p Policy) validateRustManifestSeen(manifest string, seen map[string]bool) error {
	if seen[manifest] {
		return nil
	}
	if len(seen) >= 128 {
		return refuse("Cargo package graph exceeds supported bound")
	}
	seen[manifest] = true
	for d := filepath.Dir(manifest); ; d = filepath.Dir(d) {
		for _, name := range []string{"config", "config.toml"} {
			if _, e := os.Lstat(filepath.Join(d, ".cargo", name)); e == nil {
				return refuse("Cargo configuration requires a separately supported adapter")
			}
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	b, e := os.ReadFile(manifest)
	if e != nil {
		return e
	}
	if strings.Contains(string(b), "\\") {
		return refuse("escaped Cargo manifest strings are unsupported")
	}
	if len(b) > 1<<20 {
		return refuse("Cargo manifest exceeds 1 MiB")
	}
	// This baseline deliberately excludes workspace/build-script/custom harness manifests.
	for _, line := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if rustWorkspaceHeader.MatchString(s) || rustUnsafeManifestKey.MatchString(s) {
			return refuse("unsupported Cargo build script, workspace or custom harness")
		}
	}
	if _, e := os.Lstat(filepath.Join(filepath.Dir(manifest), "build.rs")); e == nil {
		return refuse("Cargo build scripts are unsupported")
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, "path") && strings.Contains(line, "=") && (strings.Contains(line, "\\") || strings.Contains(line, "\"\"\"") || strings.Contains(line, "'''") || strings.Contains(line, "\"path\"") || strings.Contains(line, "'path'")) {
			return refuse("escaped manifest paths are unsupported")
		}
	}
	for _, m := range rustPath.FindAllStringSubmatch(string(b), -1) {
		r, e := resolve(filepath.Dir(manifest), m[1])
		if e != nil {
			return e
		}
		if !inside(p.Worktree, r) {
			return refuse("Cargo manifest path escapes worktree")
		}
		if st, e := os.Stat(r); e == nil && st.IsDir() {
			if e = p.validateRustManifestSeen(filepath.Join(r, "Cargo.toml"), seen); e != nil {
				return e
			}
		}
	}
	return nil
}
func (p Policy) prepareRustfmt(args []string) ([]string, error) {
	out := []string{args[0], "--config-path", "/dev/null"}
	files := 0
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--check", "-l", "--files-with-diff":
			out = append(out, a)
		case "--edition":
			if i+1 >= len(args) {
				return nil, refuse("missing Rust edition")
			}
			i++
			if args[i] != "2015" && args[i] != "2018" && args[i] != "2021" && args[i] != "2024" {
				return nil, refuse("unsupported Rust edition")
			}
			out = append(out, a, args[i])
		default:
			if strings.HasPrefix(a, "-") {
				return nil, refuse("unsupported rustfmt argument")
			}
			r, e := p.scoped(a)
			if e != nil {
				return nil, e
			}
			if filepath.Ext(r) != ".rs" {
				return nil, refuse("rustfmt requires explicit .rs files")
			}
			if e = CheckFormatFile(r); e != nil {
				return nil, e
			}
			b, e := os.ReadFile(r)
			if e != nil {
				return nil, e
			}
			if rustExternalMod.Match(b) {
				return nil, refuse("rustfmt external modules require item-scoped leaf files")
			}
			out = append(out, r)
			files++
		}
	}
	if files == 0 {
		return nil, refuse("rustfmt requires explicit files")
	}
	return out, nil
}
