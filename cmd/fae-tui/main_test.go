package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestMain lets the test binary act as the fae-tui executable.
func TestMain(m *testing.M) {
	if os.Getenv("TUI_TEST_AS_MAIN") == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// The executable passes its arguments to tui.Main and exits with its code:
// usage on --help with 0, and a refused flag named with the usage exit code
// before any terminal or socket is opened.
func TestFaeTUIExitCodes(t *testing.T) {
	for _, c := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--help"}, 0, "usage: fae-tui"},
		{[]string{"--home", t.TempDir(), "--refresh", "1ms"}, 2, "--refresh 1ms is outside 250ms..10m"},
		{[]string{"--home", t.TempDir(), "extra"}, 2, `unexpected argument "extra"`},
	} {
		cmd := exec.Command(os.Args[0], c.args...)
		cmd.Env = append(os.Environ(), "TUI_TEST_AS_MAIN=1")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		err := cmd.Run()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != c.code || !strings.Contains(stderr.String(), c.want) {
			t.Errorf("fae-tui %v: exit %d, stderr %q", c.args, code, stderr.String())
		}
	}
}
