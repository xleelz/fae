package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type launchResponse struct {
	PID   int
	Error string
}

// ServeLauncher must be started by the operator outside agent containment. Both
// its own native state and each actual child are checked, never inferred from a
// binary name or wrapper script. The private socket is an operator capability.
func ServeLauncher(ctx context.Context, socket string) error {
	safe, e := Unsandboxed(os.Getpid())
	if e != nil {
		return e
	}
	if !safe {
		return errors.New("runtime launcher must run outside inherited sandbox")
	}
	if !filepath.IsAbs(socket) {
		return errors.New("absolute launcher socket required")
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
	listener, e := net.Listen("unix", socket)
	if e != nil {
		return e
	}
	defer listener.Close()
	if e = os.Chmod(socket, 0600); e != nil {
		return e
	}
	go func() { <-ctx.Done(); listener.Close() }()
	var mu sync.Mutex
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
			var spec LaunchSpec
			if json.NewDecoder(conn).Decode(&spec) != nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			response := launchResponse{}
			pid, e := launchSpec(spec)
			response.PID = pid
			if e != nil {
				response.Error = e.Error()
			}
			_ = json.NewEncoder(conn).Encode(response)
		}()
	}
}
func launchSpec(spec LaunchSpec) (int, error) {
	if !filepath.IsAbs(spec.Binary) || !filepath.IsAbs(spec.Directory) || len(spec.Environment) == 0 {
		return 0, errors.New("absolute executable/workspace and pinned environment required")
	}
	hash, e := HashFile(spec.Binary)
	if e != nil {
		return 0, e
	}
	if hash != spec.SHA256 {
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
	if e = json.NewEncoder(conn).Encode(spec); e != nil {
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
