package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionAndUsage(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "partial") {
		t.Fatal(out.String())
	}
	for _, args := range [][]string{nil, {"run"}, {"workflow", "validate", "/missing"}} {
		if run(args, &out) == nil {
			t.Fatal("invalid invocation accepted")
		}
	}
}

func TestExampleValidateAndReplay(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"workflow", "validate", "../../examples/workflow.json"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"valid": true`) {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := run([]string{"workflow", "simulate", "../../examples/workflow.json", "../../examples/script.json", "demo"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"state": "Done"`) {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := run([]string{"workflow", "simulate", "../../examples/workflow.json", "../../examples/script.json", "other-task"}, &out); err == nil {
		t.Fatal("cross-task replay accepted")
	}
	if strings.Contains(out.String(), `"state": "Done"`) {
		t.Fatal("cross-task replay succeeded")
	}
}
