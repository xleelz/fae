package controller

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

type launchResponse struct {
	PID       int
	Error     string
	Operation *OperationResult `json:"operation,omitempty"`
}

// launchRequest carries the launcher key beside the spec, never on argv.
type launchRequest struct {
	Token     string            `json:"token"`
	Spec      LaunchSpec        `json:"spec"`
	Operation *OperationRequest `json:"operation,omitempty"`
}

// LauncherKeyPath is the private key file of the launcher serving socket.
func LauncherKeyPath(socket string) string {
	return filepath.Join(filepath.Dir(socket), "launcher.key")
}

// ServeLauncher must be started by the operator outside agent containment. Both
// its own native state and each actual child are checked, never inferred from a
// binary name or wrapper script.
//
// FAE-SEC-001: the socket runs commands, so socket reachability alone grants
// nothing. The launcher writes a fresh 32-byte key to an owner-only file beside
// its owner-only socket (never argv), and every request must present it,
// compared in constant time. A request only selects a launch the instance's
// daemon already authorized: the spec must equal the Launch or Replacement of
// the maintenance marker at markerPath, which names this socket and is in an
// offline-start state, and the binary is hashed against the marker's digest,
// never against a digest the caller supplies.
func ServeLauncher(ctx context.Context, socket, markerPath string) error {
	safe, e := Unsandboxed(os.Getpid())
	if e != nil {
		return e
	}
	if !safe {
		return errors.New("runtime launcher must run outside inherited sandbox")
	}
	if !filepath.IsAbs(socket) || !filepath.IsAbs(markerPath) {
		return errors.New("absolute launcher socket and maintenance marker path required")
	}
	dir := filepath.Dir(socket)
	info, e := os.Stat(dir)
	if e != nil {
		return e
	}
	physical, e := filepath.EvalSymlinks(dir)
	if e != nil || physical != dir || info.Mode().Perm() != 0700 {
		return errors.New("private physical launcher directory required")
	}
	// Owner-only from creation: no connection can precede the permission check.
	mask := syscall.Umask(0177)
	listener, e := net.Listen("unix", socket)
	syscall.Umask(mask)
	if e != nil {
		return e
	}
	defer listener.Close()
	if info, e := os.Lstat(socket); e != nil || info.Mode().Perm() != 0600 {
		return errors.Join(e, errors.New("launcher socket is not owner-only"))
	}
	key, e := writeLauncherKey(LauncherKeyPath(socket))
	if e != nil {
		return e
	}
	defer os.Remove(LauncherKeyPath(socket))
	go func() { <-ctx.Done(); listener.Close() }()
	var mu sync.Mutex
	operations := newOperationService(ctx, socket, markerPath)
	operations.receiptDir = filepath.Join(filepath.Dir(markerPath), "run", "controller-operations")
	for {
		conn, e := listener.Accept()
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			var request launchRequest
			if json.NewDecoder(conn).Decode(&request) != nil {
				return
			}
			response := launchResponse{}
			if subtle.ConstantTimeCompare([]byte(request.Token), key) != 1 {
				response.Error = "unauthenticated launch request refused"
				_ = json.NewEncoder(conn).Encode(response)
				return
			}
			if request.Operation != nil {
				if err := authorizeOperationRequest(markerPath, *request.Operation); err != nil {
					response.Error = err.Error()
					_ = json.NewEncoder(conn).Encode(response)
					return
				}
				result, err := operations.request(*request.Operation)
				response.Operation = &result
				if err != nil {
					response.Error = err.Error()
				}
				_ = json.NewEncoder(conn).Encode(response)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			authorized, e := authorizedLaunch(markerPath, socket, request.Spec)
			if e == nil {
				response.PID, e = launchSpec(request.Spec, authorized)
			}
			if e != nil {
				response.Error = e.Error()
			}
			_ = json.NewEncoder(conn).Encode(response)
		}()
	}
}

func writeLauncherKey(path string) ([]byte, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return nil, e
	}
	key := []byte(hex.EncodeToString(b))
	f, e := os.CreateTemp(filepath.Dir(path), ".launcher-key-")
	if e != nil {
		return nil, e
	}
	_, e = f.Write(key)
	if e = errors.Join(e, f.Sync(), f.Close()); e == nil {
		e = os.Rename(f.Name(), path)
	}
	if e != nil {
		_ = os.Remove(f.Name())
		return nil, e
	}
	return key, nil
}

func readLauncherKey(path string) (string, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return "", e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return "", e
	}
	key := make([]byte, 65)
	n, _ := f.Read(key)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || n != 64 {
		return "", errors.New("invalid launcher key")
	}
	return string(key[:n]), nil
}

// authorizedLaunch returns the marker's launch digest when spec is exactly a
// launch the daemon authorized for this launcher (FAE-SEC-001).
func authorizedLaunch(markerPath, socket string, spec LaunchSpec) (string, error) {
	m, e := ReadMarker(markerPath)
	if e != nil {
		return "", e
	}
	if m == nil || m.Launcher != socket || m.State != "stopped" && m.State != "failed" {
		return "", errors.New("no authorized offline start for this launcher")
	}
	candidates := []LaunchSpec{m.Launch}
	if m.Replacement != nil {
		candidates = append(candidates, *m.Replacement)
	}
	for _, c := range candidates {
		if c.Binary == spec.Binary && c.Directory == spec.Directory && slices.Equal(c.Arguments, spec.Arguments) && c.SHA256 == spec.SHA256 && c.SHA256 != "" && c.Digest() == EnvironmentHash(spec.Environment) {
			return c.SHA256, nil
		}
	}
	return "", errors.New("launch was not authorized by the maintenance marker")
}

func launchSpec(spec LaunchSpec, authorized string) (int, error) {
	if !filepath.IsAbs(spec.Binary) || !filepath.IsAbs(spec.Directory) || len(spec.Environment) == 0 {
		return 0, errors.New("absolute executable/workspace and pinned environment required")
	}
	hash, e := HashFile(spec.Binary)
	if e != nil {
		return 0, e
	}
	if hash != authorized {
		return 0, errors.New("binary hash changed after authorization")
	}
	cmd := exec.Command(spec.Binary, spec.Arguments...)
	cmd.Dir = spec.Directory
	cmd.Env = spec.Environment
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e = cmd.Start(); e != nil {
		return 0, e
	}
	go func() { _ = cmd.Wait() }()
	safe, e := Unsandboxed(cmd.Process.Pid)
	if e != nil || !safe {
		_ = cmd.Process.Kill()
		return 0, errors.Join(e, errors.New("launched child sandbox acceptance failed"))
	}
	return cmd.Process.Pid, nil
}
func Launch(ctx context.Context, socket string, spec LaunchSpec) (int, error) {
	info, e := os.Lstat(socket)
	if e != nil {
		return 0, e
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return 0, errors.New("invalid launcher capability socket")
	}
	key, e := readLauncherKey(LauncherKeyPath(socket))
	if e != nil {
		return 0, fmt.Errorf("launcher key: %w", e)
	}
	dialer := net.Dialer{}
	conn, e := dialer.DialContext(ctx, "unix", socket)
	if e != nil {
		return 0, e
	}
	defer conn.Close()
	deadline := time.Now().Add(30 * time.Second)
	if bound, ok := ctx.Deadline(); ok && bound.Before(deadline) {
		deadline = bound
	}
	_ = conn.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if e = json.NewEncoder(conn).Encode(launchRequest{Token: key, Spec: spec}); e != nil {
		return 0, e
	}
	var response launchResponse
	if e = json.NewDecoder(conn).Decode(&response); e != nil {
		return 0, e
	}
	if response.Error != "" {
		return 0, errors.New(response.Error)
	}
	return response.PID, nil
}
