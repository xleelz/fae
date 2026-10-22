package effects

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrokerNestedHeavyCommandRefused(t *testing.T) {
	b, w, _ := setup(t)
	b.Allowed = [][]string{{"/bin/sh", "-c", "go version"}}
	r, e := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	if r.ExitCode != 77 || !strings.Contains(r.Stderr, "broker dispatch") {
		t.Fatalf("nested command escaped: %+v", r)
	}
}
func TestBrokerClampsAdmittedGoBuild(t *testing.T) {
	b, w, _ := setup(t)
	dir := t.TempDir()
	binary := filepath.Join(dir, "go")
	if e := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	b.Allowed = [][]string{{binary, "build", "-p=999", "."}}
	r, e := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(r.Output, "-p=2") || strings.Contains(r.Output, "999") {
		t.Fatalf("%+v", r)
	}
}
