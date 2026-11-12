package commandshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func configuredPolicy(t *testing.T) Policy {
	t.Helper()
	root := t.TempDir()
	return Policy{Worktree: root, CWD: root, CodeRole: true, LeaseHeld: true}
}
func TestConfiguredCommandsPreserveVerifierAndFutureToolArguments(t *testing.T) {
	p := configuredPolicy(t)
	for _, argv := range [][]string{{"cargo", "kani", "--harness", "check", "--default-unwind", "8"}, {"go", "run", "./cmd/gomela", "--model", "model.go"}, {"java", "-XX:ActiveProcessorCount=1", "-jar", "/installed/gobra.jar", "-i", "proof.go"}, {"future-verifier", "--literal", "a; b | $(not-a-shell)"}} {
		prepared, err := p.PrepareConfigured(argv)
		if err != nil || !slices.Equal(prepared, argv) {
			t.Fatalf("configured command rewritten/refused: %q -> %q %v", argv, prepared, err)
		}
	}
	argv := []string{"echo", "literal; touch SHOULD_NOT_EXIST"}
	prepared, err := p.PrepareConfigured(argv)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(prepared[0], prepared[1:]...)
	cmd.Dir = p.Worktree
	output, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != argv[1] {
		t.Fatal("literal argument not preserved", string(output), err)
	}
	if _, err = os.Stat(filepath.Join(p.Worktree, "SHOULD_NOT_EXIST")); !os.IsNotExist(err) {
		t.Fatal("literal arguments interpreted through a shell", err)
	}
}
func TestConfiguredCommandsRetainExecutionBoundary(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Policy)
	}{{"code-role", func(p *Policy) { p.CodeRole = false }}, {"lease", func(p *Policy) { p.LeaseHeld = false }}, {"nested", func(p *Policy) { p.Nested = true }}, {"outside-cwd", func(p *Policy) { p.CWD = t.TempDir() }}} {
		t.Run(tc.name, func(t *testing.T) {
			p := configuredPolicy(t)
			tc.change(&p)
			if _, err := p.PrepareConfigured([]string{"cargo", "kani"}); err == nil {
				t.Fatal("execution boundary bypass accepted")
			}
		})
	}
	p := configuredPolicy(t)
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(p.Worktree, alias); err != nil {
		t.Fatal(err)
	}
	p.CWD = alias
	if _, err := p.PrepareConfigured([]string{"java", "-jar", "tool.jar"}); err == nil {
		t.Fatal("nonphysical cwd accepted")
	}
	p = configuredPolicy(t)
	for _, argv := range [][]string{nil, {""}, {"git", "status"}, {"tool", "bad\x00arg"}, {"tool", strings.Repeat("a", (128<<10)+1)}} {
		if _, err := p.PrepareConfigured(argv); err == nil {
			t.Fatal("invalid/dedicated command accepted", argv[:min(len(argv), 1)])
		}
	}
}
func TestConfiguredWorktreeExecutableCannotEscape(t *testing.T) {
	p := configuredPolicy(t)
	script := filepath.Join(p.Worktree, "prove")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	prepared, err := p.PrepareConfigured([]string{"./prove", "--mode", "bounded"})
	if err != nil || prepared[0] != script {
		t.Fatal(prepared, err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err = os.WriteFile(outside, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(p.Worktree, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err = p.PrepareConfigured([]string{"./escape"}); err == nil {
		t.Fatal("relative executable escaped worktree")
	}
}
func TestConfiguredEnvironmentPreservesExplicitWorkerControls(t *testing.T) {
	env := []string{"TMPDIR=/owned/scratch", "JAVA_TOOL_OPTIONS=-XX:ActiveProcessorCount=1", "RAYON_NUM_THREADS=1", "KEEP=literal value"}
	result, err := ConfiguredEnvironment([]string{"java", "-jar", "tool.jar"}, env, "/owned/scratch")
	if err != nil || !slices.Equal(result, env) {
		t.Fatal(result, err)
	}
	result[0] = "changed"
	if env[0] != "TMPDIR=/owned/scratch" {
		t.Fatal("environment alias mutated caller")
	}
	if _, err = ConfiguredEnvironment([]string{"tool"}, []string{"bad"}, ""); err == nil {
		t.Fatal("malformed environment accepted")
	}
}
