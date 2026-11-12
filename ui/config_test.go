package ui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAE-API-030: only a literal, specified loopback bind passes; every other
// value is refused naming it.
func TestValidateBindAcceptsOnlyLiteralLoopback(t *testing.T) {
	for _, bind := range []string{"0.0.0.0", "::", "[::]", "192.168.1.10", "10.0.0.1", "fe80::1", "", "example.com", "127.0.0.1%lo", "localhost.example"} {
		if _, err := ValidateBind(bind); err == nil || !strings.Contains(err.Error(), "\""+bind+"\"") {
			t.Fatalf("bind %q: %v", bind, err)
		}
	}
	for bind, want := range map[string]string{"127.0.0.1": "127.0.0.1", "localhost": "127.0.0.1", "::1": "::1", "[::1]": "::1", "127.0.0.2": "127.0.0.2"} {
		if got, err := ValidateBind(bind); err != nil || got != want {
			t.Fatalf("bind %q: %q %v", bind, got, err)
		}
	}
	for _, c := range []struct{ literal, loopback, unspecified, ok bool }{{true, true, false, true}, {false, true, false, false}, {true, false, false, false}, {true, true, true, false}} {
		if MayListen(c.literal, c.loopback, c.unspecified) != c.ok {
			t.Fatalf("MayListen%+v", c)
		}
	}
}

func TestPortAndSecretFiles(t *testing.T) {
	if p, err := Port("/a/root", 0); err != nil || p < 18658 || p >= 18658+4096 {
		t.Fatal(p, err)
	}
	if p, err := Port("/a/root", 20000); err != nil || p != 20000 {
		t.Fatal("pinned port", p, err)
	}
	if _, err := Port("/a/root", 80); err == nil {
		t.Fatal("privileged port accepted")
	}
	if URL("::1", 18700) != "http://[::1]:18700/" {
		t.Fatal(URL("::1", 18700))
	}
	path := filepath.Join(t.TempDir(), TokenFile)
	if err := os.WriteFile(path, []byte("stale"), 0644); err != nil {
		t.Fatal(err)
	}
	token, err := NewToken()
	if err != nil || len(token) != 64 {
		t.Fatal(token, err)
	}
	if err = WriteSecret(path, []byte(token)); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("secret not owner-only", err)
	}
}
