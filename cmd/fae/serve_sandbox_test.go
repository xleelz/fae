package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestServeSandboxedLaunchHelper runs fae serve inside the sandboxed child of
// TestServeRefusesSandboxedLaunch and exits with the command's code.
func TestServeSandboxedLaunchHelper(t *testing.T) {
	home, workspace := os.Getenv("SANDBOX_SERVE_HOME"), os.Getenv("SANDBOX_SERVE_WORKSPACE")
	if home == "" {
		return
	}
	err := execute(context.Background(), []string{"serve", "--home", home, "--instance", "sandboxed", "--workspace", workspace, "--adapter", "scripted", "--foreground"}, &bytes.Buffer{})
	if err != nil {
		os.Stderr.WriteString(err.Error() + "\n")
	}
	os.Exit(exitCode(err))
}

// FAE-CLI-010, FAE-HOST-030: serve refuses a sandboxed launch context (here
// no-new-privileges, as an agent session would pass on) with exit 1, before
// it creates anything.
func TestServeRefusesSandboxedLaunch(t *testing.T) {
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		t.Skip("setpriv (util-linux) is required to launch with no-new-privileges")
	}
	home, workspace := shortHome(t), shortHome(t)
	if err = exec.Command("git", "init", "-q", workspace).Run(); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(setpriv, "--no-new-privs", os.Args[0], "-test.run=^TestServeSandboxedLaunchHelper$")
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "SANDBOX_SERVE_HOME="+home, "SANDBOX_SERVE_WORKSPACE="+workspace)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	code := cmd.ProcessState.ExitCode()
	if code != 1 || !strings.Contains(stderr.String(), "unsandboxed") {
		t.Fatalf("sandboxed serve: code=%s err=%v stderr=%q", strconv.Itoa(code), err, stderr.String())
	}
	if entries, _ := os.ReadDir(home); len(entries) != 0 {
		t.Fatal("a refused sandboxed launch created instance files", entries)
	}
	if _, err = os.Stat(filepath.Join(workspace, ".fae")); !os.IsNotExist(err) {
		t.Fatal("a refused sandboxed launch wrote into the workspace", err)
	}
}
