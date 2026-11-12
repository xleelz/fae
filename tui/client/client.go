// Package client is fae-tui's connection to a running daemon: the instance
// RPC socket and credential that the fae CLI uses, the same versioned request
// framing (package api), and typed reads and writes over it. It never starts
// a daemon, never creates a credential and never imports the daemon package:
// like the web UI, the TUI reaches the daemon only over RPC.
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workspace"
)

// DefaultInstance is the instance the fae CLI uses when --instance is absent.
const DefaultInstance = "default"

// instanceName is the daemon's instance-name rule (daemon/layout.go).
var instanceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// DefaultHome is the fae CLI's default instance home, ~/.fae/instances.
func DefaultHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".fae", "instances"), nil
}

// Paths resolves an instance's RPC socket and credential exactly as the fae
// CLI does (daemon.Paths): HOME/INSTANCE/run/rpc.sock and .../run/credential,
// with HOME an absolute, canonical path and INSTANCE a valid instance name.
func Paths(home, instance string) (socket, credential string, err error) {
	if err = workspace.Validate(home); err != nil {
		return "", "", err
	}
	if !instanceName.MatchString(instance) {
		return "", "", errors.New("invalid instance name")
	}
	run := filepath.Join(home, instance, "run")
	return filepath.Join(run, "rpc.sock"), filepath.Join(run, "credential"), nil
}

// ErrUnreachable marks a daemon that cannot be reached: no credential file
// yet, or no listener on the socket. Callers show it and retry.
var ErrUnreachable = errors.New("daemon unreachable")

type unreachableError struct{ err error }

func (e *unreachableError) Error() string { return "daemon unreachable: " + e.err.Error() }
func (e *unreachableError) Unwrap() []error {
	return []error{ErrUnreachable, e.err}
}

// Unreachable wraps err so that errors.Is(err, ErrUnreachable) holds.
func Unreachable(err error) error { return &unreachableError{err} }

// Caller sends one RPC and returns its result document.
type Caller interface {
	Call(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// Socket is the Caller over an instance's Unix socket.
type Socket struct {
	Path, Credential string
}

// NewSocket resolves the instance's socket and credential paths. It reads
// nothing: a daemon that is not running yet is reported per call.
func NewSocket(home, instance string) (*Socket, error) {
	socket, credential, err := Paths(home, instance)
	if err != nil {
		return nil, err
	}
	return &Socket{Path: socket, Credential: credential}, nil
}

// Call frames params as the fae CLI does (canonical JSON in a version-1
// request) and returns the result. A typed daemon refusal is *api.RPCError.
func (s *Socket) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	credential, err := readCredential(s.Credential)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, Unreachable(fmt.Errorf("no credential at %s; is the daemon running?", s.Credential))
		}
		return nil, err
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if raw, err = domain.CanonicalJSON(raw); err != nil {
		return nil, err
	}
	response, err := api.Call(ctx, s.Path, credential, api.Request{Version: api.Version, Method: method, Params: raw})
	if err != nil {
		return nil, classify(err)
	}
	if response.Version != api.Version {
		return nil, fmt.Errorf("unsupported RPC response version %d", response.Version)
	}
	return response.Result, nil
}

// classify marks a failed dial (no socket, nobody listening) unreachable.
func classify(err error) error {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return Unreachable(err)
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
		return Unreachable(err)
	}
	return err
}

// readCredential reads the instance credential without creating, following
// or widening it: an owner-only regular file of exactly 32 bytes, as the
// fae CLI requires.
func readCredential(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 32 {
		return nil, fmt.Errorf("unsafe credential file %s", path)
	}
	b := make([]byte, 32)
	_, err = io.ReadFull(f, b)
	return b, err
}

// Code is a refusal's typed error code, or "" for an untyped error.
func Code(err error) string {
	var typed *api.RPCError
	if errors.As(err, &typed) {
		return typed.Code
	}
	return ""
}
