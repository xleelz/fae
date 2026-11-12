package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/daemon"
)

// planeRoot makes a scratch root whose instance's derived Dolt port is free:
// other suites' planes and leaked ones share the port range, and Start
// refuses adoption of an occupied port. The probe connects, as Start does.
func planeRoot(t testing.TB, prefix, instance string) string {
	t.Helper()
	for n := 0; ; n++ {
		root, err := os.MkdirTemp("/var/tmp", prefix)
		if err != nil {
			t.Fatal(err)
		}
		l, err := daemon.Paths(filepath.Join(root, "instances"), instance)
		if err != nil {
			t.Fatal(err)
		}
		port, err := controlplane.Port(l.Data, 0)
		if err != nil {
			t.Fatal(err)
		}
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 2*time.Second)
		if err == nil {
			conn.Close()
		}
		if errors.Is(err, syscall.ECONNREFUSED) || n == 20 {
			return root
		}
		t.Logf("plane port %d in use (%v); choosing another root", port, err)
		os.RemoveAll(root)
	}
}

// reapProcesses kills, at the end of a test, every process of this user that
// still runs in or names the scratch root, such as a detached daemon and its
// Dolt plane left by a failed test. A leaked Dolt keeps its port, and later
// fixtures can hash to it. Processes get a short grace to exit first; one that
// outlives a passing test is a leak and fails it.
func reapProcesses(t testing.TB, root string) {
	t.Helper()
	find := func() map[int]string {
		out := map[int]string{}
		entries, _ := os.ReadDir("/proc")
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid == os.Getpid() {
				continue
			}
			dir := filepath.Join("/proc", entry.Name())
			if info, err := os.Stat(dir); err != nil || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
				continue
			}
			stat, err := os.ReadFile(filepath.Join(dir, "stat"))
			if i := strings.LastIndexByte(string(stat), ')'); err != nil || i < 0 || i+2 >= len(stat) || stat[i+2] == 'Z' || stat[i+2] == 'X' {
				continue
			}
			raw, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
			cmdline := strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " "))
			cwd, _ := os.Readlink(filepath.Join(dir, "cwd"))
			if strings.Contains(cmdline, root) || cwd == root || strings.HasPrefix(cwd, root+"/") {
				out[pid] = cmdline
			}
		}
		return out
	}
	left := find()
	for deadline := time.Now().Add(5 * time.Second); len(left) > 0 && time.Now().Before(deadline); left = find() {
		time.Sleep(100 * time.Millisecond)
	}
	// A failed test may leave its processes behind; a passing one must not.
	report := t.Errorf
	if t.Failed() {
		report = t.Logf
	}
	for pid, cmdline := range left {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		report("reaped leftover process %d after the test: %s", pid, cmdline)
	}
	for deadline := time.Now().Add(5 * time.Second); len(left) > 0 && time.Now().Before(deadline); left = find() {
		time.Sleep(50 * time.Millisecond)
	}
	if len(left) > 0 {
		t.Errorf("processes survived SIGKILL: %v", left)
	}
}
