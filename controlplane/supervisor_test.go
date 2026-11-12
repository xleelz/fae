package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// FAE-ARC-006: the daemon binds no TCP listener. The Dolt endpoint probe
// connects instead: a listening endpoint is refused unadopted, while a port
// another socket merely holds bound (which a bind probe would trip over) is
// left for the owned child to claim or fail on.
func TestStartServerProbesEndpointWithoutBinding(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "started")
	binary := filepath.Join(root, "fake-dolt")
	if e := os.WriteFile(binary, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0700); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	occupied, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer occupied.Close()
	if _, e = StartServer(ctx, ServerConfig{Binary: binary, DataDir: root, Port: occupied.Addr().(*net.TCPAddr).Port}); e == nil {
		t.Fatal("listening Dolt endpoint adopted")
	}
	if _, e = os.Stat(marker); !os.IsNotExist(e) {
		t.Fatal("child started against an occupied endpoint", e)
	}
	fd, e := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer syscall.Close(fd)
	if e = syscall.Bind(fd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}); e != nil {
		t.Fatal(e)
	}
	bound, e := syscall.Getsockname(fd)
	if e != nil {
		t.Fatal(e)
	}
	s, e := StartServer(ctx, ServerConfig{Binary: binary, DataDir: root, Port: bound.(*syscall.SockaddrInet4).Port, StopWait: time.Second})
	if e != nil {
		t.Fatal("endpoint probe bound a listener:", e)
	}
	<-s.Done()
	if _, e = os.Stat(marker); e != nil {
		t.Fatal("owned child not started", e)
	}
}

// FAE-CP-004: an occupied hash-derived endpoint is refused instead of
// changing the instance identity to a free port. A legacy port file cannot
// override the deterministic default; an explicit pin remains supported.
func TestPortCollisionIsDeterministicAndRefused(t *testing.T) {
	var dataDir string
	var occupied net.Listener
	var expected int
	for i := 0; i < 64; i++ {
		candidateDir := filepath.Join(t.TempDir(), fmt.Sprintf("plane-%d", i))
		hash := sha256.Sum256([]byte(candidateDir))
		port := 3658 + int(binary.BigEndian.Uint64(hash[:8])%4096)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			dataDir, occupied, expected = candidateDir, ln, port
			break
		}
	}
	if occupied == nil {
		t.Fatal("could not reserve a hash-derived endpoint")
	}
	defer occupied.Close()
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "dolt.port"), []byte("12345\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		port, err := Port(dataDir, 0)
		if err != nil || port != expected {
			t.Fatalf("default port %d want %d: %v", port, expected, err)
		}
	}
	if _, err := StartServer(context.Background(), ServerConfig{Binary: "/does-not-exist", DataDir: dataDir}); err == nil {
		t.Fatal("foreign endpoint not refused")
	}
	if port, err := Port(dataDir, 12345); err != nil || port != 12345 {
		t.Fatalf("explicit pin %d: %v", port, err)
	}
}
