package effects

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/workflow"
)

func TestConfiguredUnknownToolCheckIsExecutionNotProof(t *testing.T) {
	b, w, _ := setup(t)
	tool := filepath.Join(w.Path, "future-check")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf '%s\\n' '{\"checked\":2,\"ok\":true}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	argv := []string{"./future-check", "--report=json"}
	b.Allowed = [][]string{argv}
	command := Command{Argv: argv, Adapter: "configured", Check: &workflow.CheckSpec{Format: "json", CountField: "checked"}, Timeout: 5 * time.Second}
	run, err := b.Run(context.Background(), w, command)
	if err != nil || run.CheckError != "" || run.CheckCount != 2 || run.CheckPayload["ok"] != true || run.Proof != nil || run.ExecutedCount != 0 {
		t.Fatalf("configured check confused with proof/test: %+v %v", run, err)
	}
	bad := command
	bad.Argv = []string{"./future-check", "--different"}
	if _, err = b.Run(context.Background(), w, bad); err == nil {
		t.Fatal("configured adapter bypassed exact target allowlist")
	}
	command.Check = nil
	plain, err := b.Run(context.Background(), w, command)
	if err != nil || plain.ExitCode != 0 || plain.Proof != nil || plain.ExecutedCount != 0 || plain.CheckPayload != nil {
		t.Fatalf("plain command invented semantics: %+v %v", plain, err)
	}
}
func TestConfiguredAdapterDoesNotRelaxBuiltinCargoGrammar(t *testing.T) {
	b, w, _ := setup(t)
	cargo := filepath.Join(w.Path, "cargo")
	if err := os.WriteFile(cargo, []byte("#!/bin/sh\nprintf '%s\\n' '{\"checked\":1}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	argv := []string{cargo, "kani", "--harness", "bounded"}
	b.Allowed = [][]string{argv}
	command := Command{Argv: argv, Adapter: "builtin", Check: &workflow.CheckSpec{Format: "json", CountField: "checked"}, Timeout: 5 * time.Second}
	if _, err := b.Run(context.Background(), w, command); err == nil || !strings.Contains(err.Error(), "Cargo") {
		t.Fatal("builtin Cargo grammar silently broadened", err)
	}
	command.Adapter = "configured"
	run, err := b.Run(context.Background(), w, command)
	if err != nil || run.CheckCount != 1 || run.Proof != nil {
		t.Fatalf("explicit configured command did not run: %+v %v", run, err)
	}
}
