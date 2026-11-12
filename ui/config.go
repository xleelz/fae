package ui

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Secret files the daemon writes into the private UI directory before every
// launch. The child reads and removes each one before it listens, like the
// resident worker key; secrets never travel on argv or in the environment.
const (
	TokenFile           = "token"
	CredentialFile      = "rpc.credential"
	WriteCredentialFile = "write.credential"
)

// DefaultBind is the Appendix A UI bind address.
const DefaultBind = "127.0.0.1"

// ValidateBind returns the canonical literal loopback address for a configured
// UI bind, or a refusal naming the value (FAE-API-030). "localhost" is the
// only name accepted, as an alias of 127.0.0.1; nothing is resolved.
func ValidateBind(addr string) (string, error) {
	host := strings.TrimSuffix(strings.TrimPrefix(addr, "["), "]")
	if addr == "localhost" {
		host = DefaultBind
	}
	ip := net.ParseIP(host)
	literal := ip != nil && !strings.Contains(host, "%")
	if !MayListen(literal, literal && ip.IsLoopback(), literal && ip.IsUnspecified()) {
		return "", fmt.Errorf("UI bind address %q refused: only a literal loopback address (127.0.0.0/8 or ::1) may be bound; reach the UI remotely through a port forward over an authenticated channel", addr)
	}
	return ip.String(), nil
}

// Port is the UI port: a pinned value, or a stable per-instance port derived
// from the instance root, outside Dolt's derived range and the ephemeral range.
func Port(root string, pinned int) (int, error) {
	if pinned != 0 {
		if pinned < 1024 || pinned > 65535 {
			return 0, fmt.Errorf("UI port %d outside 1024..65535", pinned)
		}
		return pinned, nil
	}
	hash := sha256.Sum256([]byte("fae-ui:" + root))
	return 18658 + int(binary.BigEndian.Uint64(hash[:8])%4096), nil
}

// URL is the UI's base address; it never carries the token.
func URL(bind string, port int) string {
	return "http://" + net.JoinHostPort(bind, strconv.Itoa(port)) + "/"
}

// NewToken returns a fresh 256-bit token in hex.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}

// WriteSecret replaces a secret file with an owner-only regular file.
func WriteSecret(path string, b []byte) error {
	if e := os.Remove(path); e != nil && !os.IsNotExist(e) {
		return e
	}
	f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return e
	}
	_, e = f.Write(b)
	return errors.Join(e, f.Close())
}
