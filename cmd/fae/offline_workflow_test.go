package main

import (
	"bytes"
	"context"
	"testing"
)

// FAE-CLI-001: the offline workflow forms parse the common flags with the
// flag set every other verb uses, so the single-dash forms, --json=true and
// --authorizing-message are accepted like --instance=x, before, between or
// after the operands. Before, a hand parser knew only --json, --instance and
// --home, and these exited 2. An unknown flag and a common flag without its
// value stay usage errors.
func TestOfflineWorkflowFormsParseTheCommonFlagSet(t *testing.T) {
	const example, script = "../../examples/workflow.json", "../../examples/script.json"
	for _, args := range [][]string{
		{"workflow", "validate", example, "-json"},
		{"workflow", "validate", example, "--json=true"},
		{"workflow", "validate", "-instance", "x", example},
		{"workflow", "validate", example, "-instance=x"},
		{"workflow", "validate", example, "--authorizing-message", "M"},
		{"workflow", "validate", example, "-authorizing-message=M", "-home", "/nonexistent/fae"},
		{"workflow", "-json", "validate", example},
		{"workflow", "simulate", example, "-json", script, "--instance=x", "demo"},
	} {
		var out bytes.Buffer
		if err := execute(context.Background(), args, &out); err != nil || out.Len() == 0 {
			t.Errorf("%v: exit %d %v", args, exitCode(err), err)
		}
	}
	for _, args := range [][]string{
		{"workflow", "validate", example, "-bogus"},
		{"workflow", "validate", example, "-instance"},
		{"workflow", "validate", example, "--authorizing-message"},
		{"workflow", "validate", example, example},
	} {
		if err := execute(context.Background(), args, &bytes.Buffer{}); exitCode(err) != 2 {
			t.Errorf("%v: exit %d %v, want usage 2", args, exitCode(err), err)
		}
	}
}
