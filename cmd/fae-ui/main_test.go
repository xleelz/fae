package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMain lets the test binary act as the fae-ui executable.
func TestMain(m *testing.M) {
	if os.Getenv("UI_TEST_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// FAE-API-030: the separate UI executable refuses a non-loopback bind, naming
// it, with the usage exit code and no listener.
func TestFaeUIRefusesNonLoopbackBind(t *testing.T) {
	cmd := exec.Command(os.Args[0], "--bind", "0.0.0.0", "--port", "0", "--socket", "/nonexistent/rpc.sock", "--secrets-dir", "/nonexistent")
	cmd.Env = append(os.Environ(), "UI_TEST_AS_MAIN=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 || !strings.Contains(stderr.String(), `"0.0.0.0"`) {
		t.Fatalf("fae-ui non-loopback bind: %v %q", err, stderr.String())
	}
}
