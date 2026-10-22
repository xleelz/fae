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

// Port is stable across processes and Go versions.
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
	listener, e := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if e != nil {
		return nil, fmt.Errorf("Dolt endpoint occupied; refusing adoption: %w", e)
	}
	listener.Close()
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
		record, e = processowner.Start(cmd, c.OwnerRegistry, c.DataDir)
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
func (s *Server) Stop() error {
	select {
	case <-s.done:
		return nil
	default:
	}
	if e := s.cmd.Process.Signal(syscall.SIGTERM); e != nil {
		select {
		case <-s.done:
			return nil
		default:
			return e
		}
	}
	timer := time.NewTimer(s.wait)
	defer timer.Stop()
	select {
	case <-s.done:
		return nil
	case <-timer.C:
		if e := s.cmd.Process.Kill(); e != nil {
			return e
		}
		<-s.done
		return nil
	}
}
