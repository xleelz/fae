package main

import (
	"os"
	"testing"
)

// shortHome is a disk-backed instance home short enough for the RPC socket
// path, whatever the checkout path or test name.
func shortHome(t *testing.T) string {
	t.Helper()
	home, err := os.MkdirTemp("/var/tmp", "fae-cli-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return home
}
