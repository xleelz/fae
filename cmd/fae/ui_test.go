package main

import (
	"bytes"
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/daemon"
)

// FAE-CLI-010, FAE-API-030: serve refuses a non-loopback UI bind naming the
// value (exit 1) and malformed UI flags (exit 2) before detaching or creating
// anything.
func TestServeUIFlagValidation(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	home := filepath.Join(root, "instances")
	serve := func(extra ...string) error {
		args := append([]string{"--home", home, "--instance", "uiflags", "serve", "--workspace", ws, "--adapter", "scripted", "--dolt", "/nonexistent/dolt"}, extra...)
		return execute(context.Background(), args, &bytes.Buffer{})
	}
	for _, bind := range []string{"0.0.0.0", "::", "[::]", "192.168.1.10", "example.com"} {
		err := serve("--ui", "--ui-bind", bind)
		if exitCode(err) != 1 || !strings.Contains(err.Error(), "\""+bind+"\"") {
			t.Fatalf("bind %q: code %d %v", bind, exitCode(err), err)
		}
	}
	for _, extra := range [][]string{{"--ui-port", "18000"}, {"--ui-bin", "/bin/true"}, {"--ui-allow-writes"}, {"--ui", "--ui-port", "80"}, {"--ui", "--ui-port", "70000"}} {
		if err := serve(extra...); exitCode(err) != 2 {
			t.Fatalf("%v: code %d %v", extra, exitCode(err), err)
		}
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("refused serve created %s: %v", home, err)
	}
	var warned bytes.Buffer
	uiWarnings = &warned
	t.Cleanup(func() { uiWarnings = os.Stderr })
	f := flag.NewFlagSet("serve", flag.ContinueOnError)
	u := addUIFlags(f)
	if err := f.Parse([]string{"--ui", "--ui-bin", "bin/ui", "--ui-bind", "localhost", "--ui-rpc-log", "ui.log", "--ui-allow-writes"}); err != nil {
		t.Fatal(err)
	}
	c, err := u.config(f)
	if err != nil || c.Binary != filepath.Join(ws, "bin/ui") || c.RPCLog != filepath.Join(ws, "ui.log") || c.Bind != "127.0.0.1" || !c.AllowWrites || c.Port != 0 {
		t.Fatalf("UI config %+v %v", c, err)
	}
	if !strings.Contains(warned.String(), filepath.Join(ws, "bin/ui")+" is missing") {
		t.Fatalf("missing explicit UI binary not warned: %q", warned.String())
	}
	// Without --ui-bin the UI is the separate fae-ui beside this executable.
	warned.Reset()
	f = flag.NewFlagSet("serve", flag.ContinueOnError)
	u = addUIFlags(f)
	if err = f.Parse([]string{"--ui"}); err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if c, err = u.config(f); err != nil || c.Binary != "" || !strings.Contains(warned.String(), daemon.DefaultUIBinary(exe)+" is missing") {
		t.Fatalf("default UI binary %+v %v %q", c, err, warned.String())
	}
	f = flag.NewFlagSet("serve", flag.ContinueOnError)
	if c, err = addUIFlags(f).config(f); c != nil || err != nil {
		t.Fatal("UI enabled without --ui", c, err)
	}
}
