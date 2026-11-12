package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/commandshim"
)

// FAE-HOST-014: fae doctor warns, with a remedy, when the go on PATH sits
// outside any GOROOT (brokered runs then get no GOROOT mirror), passes a go
// in a GOROOT, and says nothing without a go.
func TestDoctorWarnsOfAGoOutsideAnyGoRoot(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil || commandshim.GoRoot(gobin, nil) == "" {
		t.Skip("no go in a GOROOT")
	}
	shims := t.TempDir()
	shim := filepath.Join(shims, "go")
	if err = os.WriteFile(shim, []byte("#!/bin/sh\nexec "+gobin+" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	host := func(path string) doctorEnv {
		return doctorEnv{home: t.TempDir(), instance: "default", dolt: "dolt",
			lookPath:  func(name string) (string, error) { return "/usr/bin/" + name, nil },
			version:   func(string, ...string) (string, error) { return "git version 2.45.1", nil },
			statfs:    func(string) (uint64, uint64, error) { return 64 << 30, 1 << 20, nil },
			sandboxed: func() (bool, error) { return false, nil },
			goRootGap: func() (string, string) { return commandshim.GoRootGap([]string{"PATH=" + path}) }}
	}
	check := func(env doctorEnv) (doctorCheck, bool) {
		for _, c := range doctorChecks(env) {
			if c.Name == "go-toolchain:goroot" {
				return c, true
			}
		}
		return doctorCheck{}, false
	}
	if c, ok := check(host(shims + ":" + filepath.Dir(gobin))); !ok || c.Status != "warn" || !strings.Contains(c.Detail, shim+", sits outside any GOROOT") || !strings.Contains(c.Remedy, "GOROOT's bin/") {
		t.Errorf("a go outside any GOROOT: %+v %t", c, ok)
	}
	if c, ok := check(host(filepath.Dir(gobin))); !ok || c.Status != "pass" || c.Detail != gobin {
		t.Errorf("a go in a GOROOT: %+v %t", c, ok)
	}
	if c, ok := check(host(t.TempDir())); ok {
		t.Errorf("no go on PATH reported %+v", c)
	}
}
