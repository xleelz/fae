package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAE-CP-004: a foreign listener on the derived endpoint is refused naming
// the process and the remedy.
func TestEndpointForeignListenerRefusedWithRemedy(t *testing.T) {
	root := t.TempDir()
	occupied, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer occupied.Close()
	port := occupied.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, e = StartServer(ctx, ServerConfig{Binary: "/bin/false", DataDir: root, Port: port})
	var refused *EndpointError
	if !errors.As(e, &refused) || refused.Own || !strings.Contains(e.Error(), "Remedy:") || !strings.Contains(e.Error(), "serve --dolt-port") {
		t.Fatalf("foreign listener refusal %v", e)
	}
	if _, err := os.Stat("/proc/net/tcp"); err == nil && refused.PID != os.Getpid() {
		t.Fatalf("listener identified as pid %d, want %d", refused.PID, os.Getpid())
	}
}

// FAE-CP-004: an unrecorded Dolt already serving this instance's own data
// directory is refused adoption, naming its pid and the remedy.
func TestEndpointUnrecordedOwnServerRefusedWithRemedy(t *testing.T) {
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("set FAE_TEST_DOLT_BINARY to enable isolated Dolt integration")
	}
	if _, err := os.Stat("/proc/net/tcp"); err != nil {
		t.Skip("listener identification reads /proc")
	}
	root := t.TempDir()
	data := filepath.Join(root, "plane")
	if e := os.Mkdir(data, 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("DOLT_ROOT_PATH", filepath.Join(root, "config"))
	t.Setenv("DOLT_DISABLE_EVENT_FLUSH", "1")
	init := exec.Command(binary, "init", "--name", "Fae Tests", "--email", "tests@localhost")
	init.Dir = data
	if output, e := init.CombinedOutput(); e != nil {
		t.Fatalf("initialize isolated Dolt: %v: %s", e, output)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Started without an owner registry: nothing records it.
	unrecorded, e := StartServer(ctx, ServerConfig{Binary: binary, DataDir: data, StopWait: 2 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	defer unrecorded.Stop()
	deadline := time.Now().Add(20 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", unrecorded.Port()), time.Second)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Dolt never listened", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	_, e = StartServer(ctx, ServerConfig{Binary: binary, DataDir: data})
	var refused *EndpointError
	if !errors.As(e, &refused) || !refused.Own || refused.PID != unrecorded.PID() || !strings.Contains(e.Error(), fmt.Sprintf("kill %d", unrecorded.PID())) {
		t.Fatalf("unrecorded own server refusal %v (%+v)", e, refused)
	}
}
