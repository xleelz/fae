package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/xleelz/fae/processowner"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Port derives the same endpoint from the canonical data directory every time.
// A collision is refused by StartServer; it never silently selects another port.
func Port(dataDir string, pinned int) (int, error) {
	if !filepath.IsAbs(dataDir) || filepath.Clean(dataDir) != dataDir {
		return 0, errors.New("data directory must be canonical absolute path")
	}
	if pinned != 0 {
		if pinned < 1024 || pinned > 65535 {
			return 0, errors.New("port outside 1024..65535")
		}
		return pinned, nil
	}
	hash := sha256.Sum256([]byte(dataDir))
	return 3658 + int(binary.BigEndian.Uint64(hash[:8])%4096), nil
}

// DefaultConfig returns a ServerConfig for dataDir with its deterministic port.
func DefaultConfig(dataDir string) ServerConfig {
	port, _ := Port(dataDir, 0)
	return ServerConfig{Binary: "dolt", DataDir: dataDir, Port: port, StopWait: 30 * time.Second}
}

type ServerConfig struct {
	Binary        string
	ConfigFile    string
	Env           []string
	DataDir       string
	Port          int
	Log           io.Writer
	StopWait      time.Duration
	OwnerRegistry string
}
type Server struct {
	cmd    *exec.Cmd
	done   chan struct{}
	mu     sync.Mutex
	err    error
	wait   time.Duration
	port   int
	record *processowner.Record
}

// StartServer starts only its own child and refuses to adopt an existing endpoint.
// With OwnerRegistry, durable identities and a pipe watchdog provide parent-death
// cleanup on Darwin and Linux; restart recovery remains fail-closed.
func StartServer(ctx context.Context, c ServerConfig) (*Server, error) {
	port, e := Port(c.DataDir, c.Port)
	if e != nil {
		return nil, e
	}
	// FAE-ARC-006: probe by connecting, never by binding a TCP listener. Only
	// an outright refusal proves no endpoint is listening there.
	probe, e := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e == nil {
		probe.Close()
		return nil, identifyListener(port, c.DataDir)
	}
	if !errors.Is(e, syscall.ECONNREFUSED) {
		return nil, fmt.Errorf("Dolt endpoint probe inconclusive; refusing adoption (remedy: retry once the endpoint settles, or pin a free port with serve --dolt-port N): %w", e)
	}
	if c.Binary == "" {
		c.Binary = "dolt"
	}
	if c.StopWait <= 0 {
		c.StopWait = 30 * time.Second
	}
	args := []string{"sql-server", "--host", "127.0.0.1", "--port", fmt.Sprint(port), "--data-dir", c.DataDir}
	if c.ConfigFile != "" {
		args = []string{"sql-server", "--config", c.ConfigFile}
	}
	cmd := exec.Command(c.Binary, args...)
	if c.Env != nil {
		cmd.Env = c.Env
	}
	cmd.Dir = c.DataDir
	cmd.Stdout = c.Log
	cmd.Stderr = c.Log
	if c.OwnerRegistry == "" {
		configureParentDeath(cmd)
	}
	var record *processowner.Record
	if c.OwnerRegistry != "" {
		record, e = processowner.StartRole(cmd, c.OwnerRegistry, c.DataDir, "", "dolt")
	} else {
		e = cmd.Start()
	}
	if e != nil {
		return nil, e
	}
	s := &Server{cmd: cmd, done: make(chan struct{}), wait: c.StopWait, port: port, record: record}
	go func() {
		var e error
		if record != nil {
			e = processowner.Wait(cmd, record)
		} else {
			e = cmd.Wait()
		}
		s.mu.Lock()
		s.err = e
		s.mu.Unlock()
		close(s.done)
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Stop()
		case <-s.done:
		}
	}()
	return s, nil
}
func (s *Server) PID() int              { return s.cmd.Process.Pid }
func (s *Server) Port() int             { return s.port }
func (s *Server) Done() <-chan struct{} { return s.done }
func (s *Server) Err() error            { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *Server) Stop() error           { _, e := s.Terminate(); return e }

// StopResult is how a stop ended the child: its duration, and whether the
// bounded wait expired and the child was killed.
type StopResult struct {
	PID           int
	Duration      time.Duration
	Wait          time.Duration
	Forced        bool
	AlreadyExited bool
}

// Terminate stops the child gracefully: SIGTERM (Dolt flushes), the bounded
// wait (default 30 s), then a forced kill (FAE-CP-002).
func (s *Server) Terminate() (StopResult, error) {
	r := StopResult{PID: s.cmd.Process.Pid, Wait: s.wait}
	started := time.Now()
	select {
	case <-s.done:
		r.AlreadyExited = true
		return r, nil
	default:
	}
	if e := s.cmd.Process.Signal(syscall.SIGTERM); e != nil {
		select {
		case <-s.done:
			r.AlreadyExited = true
			return r, nil
		default:
			return r, e
		}
	}
	timer := time.NewTimer(s.wait)
	defer timer.Stop()
	select {
	case <-s.done:
		r.Duration = time.Since(started)
		return r, nil
	case <-timer.C:
		r.Forced = true
		if e := s.cmd.Process.Kill(); e != nil {
			return r, e
		}
		<-s.done
		r.Duration = time.Since(started)
		return r, nil
	}
}
