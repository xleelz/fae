package main

import (
	"os"
	"os/exec"
	"testing"
)

func TestBridgeUsageSubprocess(t *testing.T) {
	if os.Getenv("FAE_BRIDGE_USAGE_TEST") == "1" {
		os.Args = []string{"fae-codex-bridge"}
		main()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBridgeUsageSubprocess$")
	cmd.Env = append(os.Environ(), "FAE_BRIDGE_USAGE_TEST=1")
	if e := cmd.Run(); e == nil {
		t.Fatal("missing config succeeded")
	}
}
