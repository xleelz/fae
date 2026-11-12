package commandshim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// syscall.Exec must occur only in this helper subprocess, never the test runner.
func TestConfiguredAccountedHelper(t *testing.T) {
	if os.Getenv("FAE_TEST_CONFIGURED_ACCOUNTED_HELPER") != "1" {
		return
	}
	code, err := RunShim(context.Background(), os.Getenv("FAE_TEST_CONFIGURED_POLICY"), HeavyOp{Argv: []string{"cargo", "kani", "--harness", "parent_proof"}, Dir: os.Getenv("FAE_TEST_CONFIGURED_WORKTREE"), Env: os.Environ(), Stdout: os.Stdout, Stderr: os.Stderr})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}

func TestConfiguredAccountedNestedToolUsesParentLeaseAndEnvironment(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "worktree")
	scratch := filepath.Join(root, "scratch")
	tools := filepath.Join(root, "tools")
	for _, d := range []string{worktree, scratch, tools} {
		if err = os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	cargo := filepath.Join(tools, "cargo")
	script := "#!/bin/sh\nprintf 'argv=%s\\n' \"$*\"\nprintf 'target=%s\\ncompiler=%s\\nparent=%s\\n' \"$CARGO_TARGET_DIR\" \"$RUSTC\" \"$FAE_TEST_PARENT_LEASE\"\n"
	if err = os.WriteFile(cargo, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(root, "invalid-ledger")
	if err = os.WriteFile(ledger, []byte("deliberately invalid ledger: nested calls must never acquire"), 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range []bool{true, false} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			policy := RoleShims{Accounted: true, Configured: configured, Worktree: worktree, Scratch: scratch, Ledger: ledger, Tools: map[string]string{"cargo": cargo}, Build: true, Test: true}
			raw, e := json.Marshal(policy)
			if e != nil {
				t.Fatal(e)
			}
			p := filepath.Join(root, fmt.Sprintf("policy-%t.json", configured))
			if e = os.WriteFile(p, raw, 0600); e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestConfiguredAccountedHelper$")
			cmd.Env = append(os.Environ(), "FAE_TEST_CONFIGURED_ACCOUNTED_HELPER=1", "FAE_TEST_CONFIGURED_POLICY="+p, "FAE_TEST_CONFIGURED_WORKTREE="+worktree, "FAE_TEST_PARENT_LEASE=already-held", "CARGO_TARGET_DIR="+filepath.Join(scratch, "parent-target"), "RUSTC=parent-pinned-compiler")
			output, e := cmd.CombinedOutput()
			if configured {
				if e != nil {
					t.Fatalf("%s: %v", output, e)
				}
				for _, want := range []string{"argv=kani --harness parent_proof", "target=" + filepath.Join(scratch, "parent-target"), "compiler=parent-pinned-compiler", "parent=already-held"} {
					if !strings.Contains(string(output), want) {
						t.Fatalf("missing %q: %s", want, output)
					}
				}
			} else {
				status, ok := e.(*exec.ExitError)
				if !ok || status.ExitCode() != Refused || strings.Contains(string(output), "argv=") {
					t.Fatalf("want builtin policy refusal77, got %s %v", output, e)
				}
			}
		})
	}
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("configured accounted invocation allocated/reset nested language scratch: %v", entries)
	}
	for _, suffix := range []string{".lock", ".tmp"} {
		if _, e := os.Stat(ledger + suffix); !os.IsNotExist(e) {
			t.Fatalf("nested invocation touched lease ledger%s: %v", suffix, e)
		}
	}
}
