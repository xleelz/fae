package commandshim

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// RustEnvironment pins the compiler and isolates Cargo's mutable configuration,
// caches and outputs before the caller installs nested-command wrappers.
func RustEnvironment(argv, env []string, scratch string) ([]string, error) {
	if _, ok := RustCommandKind(argv); !ok {
		return env, nil
	}
	compiler, e := exec.LookPath("rustc")
	if e != nil {
		return nil, fmt.Errorf("Rust compiler: %w", e)
	}
	return RustEnvironmentWithCompiler(argv, env, scratch, compiler)
}

// RustEnvironmentWithCompiler uses the tool pinned before wrapper PATH setup.
// Keep the executable basename: rustup proxies dispatch using argv[0].
func RustEnvironmentWithCompiler(argv, env []string, scratch, compiler string) ([]string, error) {
	if _, ok := RustCommandKind(argv); !ok {
		return env, nil
	}
	if compiler == "" {
		return nil, fmt.Errorf("Rust compiler is not pinned")
	}
	compiler, e := filepath.Abs(compiler)
	if e != nil {
		return nil, e
	}
	out := make([]string, 0, len(env)+4)
	for _, v := range env {
		k, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(k, "CARGO_") || strings.HasPrefix(k, "RUST") || k == "LD_PRELOAD" || k == "LD_LIBRARY_PATH" {
			continue
		}
		out = append(out, v)
	}
	home := filepath.Join(scratch, "cargo-home")
	target := filepath.Join(scratch, "cargo-target")
	for _, d := range []string{home, target} {
		if e := os.MkdirAll(d, 0700); e != nil {
			return nil, e
		}
	}
	return append(out, "RUSTC="+compiler, "CARGO_HOME="+home, "CARGO_TARGET_DIR="+target, "CARGO_NET_OFFLINE=true", "RUSTUP_AUTO_INSTALL=0"), nil
}
