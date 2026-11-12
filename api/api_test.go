package api

import (
	"context"
	"encoding/hex"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAuthenticationAndStrictParsing(t *testing.T) {
	token := make([]byte, 32)
	calls := 0
	s, err := New(token, func(context.Context, Request) (any, error) { calls++; return "ok", nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		auth, body string
		code       int
	}{{"", `{"version":1,"method":"Status"}`, 401}, {hex.EncodeToString(token), `{"version":1,"method":"Status"}`, 200}, {hex.EncodeToString(token), `{"version":2,"method":"Status"}`, 400}, {hex.EncodeToString(token), `{"version":1,"method":"Status","unknown":true}`, 400}, {hex.EncodeToString(token), `{"version":1,"method":"Status"} {}`, 400}} {
		r := httptest.NewRequest("POST", "/v1/rpc", strings.NewReader(tt.body))
		r.Header.Set("Authorization", tt.auth)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tt.code {
			t.Errorf("got %d want %d", w.Code, tt.code)
		}
	}
	if calls != 1 {
		t.Fatalf("unauthorized effect: %d", calls)
	}
}

// apiSocketDir is an owner-only directory short enough for a Unix socket path
// (108 bytes) under any TMPDIR: t.TempDir() under a brokered run's TMPDIR,
// beside a task worktree, is already too long.
func apiSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "fae-api-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestSocketAndCredential(t *testing.T) {
	root := apiSocketDir(t)
	token, err := Credential(filepath.Join(root, "credential"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := Credential(filepath.Join(root, "credential"))
	if err != nil || hex.EncodeToString(token) != hex.EncodeToString(again) {
		t.Fatal("credential changed")
	}
	s, err := New(token, func(context.Context, Request) (any, error) { return "ready", nil })
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "s")
	if err = s.Listen(socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve() }()
	defer func() {
		s.Shutdown(context.Background())
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	r, err := Call(context.Background(), socket, token, Request{Version: 1, Method: "Status"})
	if err != nil || string(r.Result) != `"ready"` {
		t.Fatalf("%s %v", r.Result, err)
	}
	if _, err := Call(context.Background(), socket, make([]byte, 32), Request{Version: 1, Method: "Status"}); err == nil {
		t.Fatal("wrong token accepted")
	}
	info, _ := os.Stat(socket)
	if info.Mode().Perm() != 0600 {
		t.Fatal("unsafe socket")
	}
}
func TestCredentialUnsafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, make([]byte, 32), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Credential(path); err == nil {
		t.Fatal("unsafe token accepted")
	}
	if _, err := New([]byte("short"), nil); err == nil {
		t.Fatal("short token accepted")
	}
}
func TestShutdownRefusesMutation(t *testing.T) {
	token := make([]byte, 32)
	s, _ := New(token, func(context.Context, Request) (any, error) { return true, nil })
	s.Shutdown(context.Background())
	r := httptest.NewRequest("POST", "/v1/rpc", strings.NewReader(`{"version":1,"method":"Admit"}`))
	r.Header.Set("Authorization", hex.EncodeToString(token))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if !strings.Contains(w.Body.String(), `"error_code":"shutting-down"`) {
		t.Fatal(w.Body.String())
	}
}

func TestCredentialSymlinkRefused(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real")
	if _, err := Credential(target); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Credential(link); err == nil {
		t.Fatal("symlink credential accepted")
	}
}

func TestDuplicateRequestKeysRefused(t *testing.T) {
	token := make([]byte, 32)
	calls := 0
	s, _ := New(token, func(context.Context, Request) (any, error) { calls++; return true, nil })
	r := httptest.NewRequest("POST", "/v1/rpc", strings.NewReader(`{"version":1,"method":"Status","method":"Admit"}`))
	r.Header.Set("Authorization", hex.EncodeToString(token))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 400 || calls != 0 {
		t.Fatal("ambiguous request accepted")
	}
}

// A server shut down between Listen and Serve (a daemon start that fails
// after binding its socket) stops listening; a later Shutdown is harmless.
func TestShutdownBeforeServeClosesTheListener(t *testing.T) {
	root := apiSocketDir(t)
	s, err := New(make([]byte, 32), func(context.Context, Request) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "s")
	if err = s.Listen(socket); err != nil {
		t.Fatal(err)
	}
	if err = s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.Dial("unix", socket); err == nil {
		conn.Close()
		t.Fatal("socket still accepts after Shutdown")
	}
	if err = s.Shutdown(context.Background()); err != nil {
		t.Fatal("second Shutdown", err)
	}
}
