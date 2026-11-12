// Package api implements versioned, credential-protected RPC over Unix sockets.
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/domain"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"
)

const Version = 1
const MaxResponseBytes = 1 << 20
const maxResultBytes = MaxResponseBytes - 1024
const MethodImportLegacy = "import_legacy"

type RPCError struct {
	Code    string
	Message string
}

func (e *RPCError) Error() string { return e.Code + ": " + e.Message }

type Request struct {
	Deadline time.Time       `json:"deadline,omitempty"`
	Version  int             `json:"version"`
	Method   string          `json:"method"`
	Key      string          `json:"key,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
}
type Response struct {
	Version   int             `json:"version"`
	Result    json.RawMessage `json:"result,omitempty"`
	Error     string          `json:"error,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
}
type Handler func(context.Context, Request) (any, error)
type Server struct {
	token      [32]byte
	principals map[[32]byte]Principal
	handler    Handler
	shutting   atomic.Bool
	server     *http.Server
	listener   net.Listener
	stream     http.HandlerFunc
}

// New requires a full 256-bit credential; no credentials enter response bodies.
func New(credential []byte, handler Handler) (*Server, error) {
	if len(credential) != 32 {
		return nil, errors.New("credential must contain 32 bytes")
	}
	if handler == nil {
		return nil, errors.New("handler required")
	}
	return &Server{token: sha256.Sum256(credential), handler: handler}, nil
}

// OutsideRepository refuses a secret file inside any Git repository or
// worktree (FAE-SEC-003): no ancestor of its physical directory may hold .git.
func OutsideRepository(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("secret file path must be absolute")
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return err
	}
	for p := dir; ; p = filepath.Dir(p) {
		if _, e := os.Lstat(filepath.Join(p, ".git")); e == nil {
			return fmt.Errorf("secret file %s must be outside any repository or worktree (found %s)", path, filepath.Join(p, ".git"))
		} else if !os.IsNotExist(e) {
			return e
		}
		if filepath.Dir(p) == p {
			return nil
		}
	}
}

// RepositoryFreeCredential is Credential for a delegated credential file,
// which must also lie outside every repository and worktree (FAE-SEC-003).
func RepositoryFreeCredential(path string) ([]byte, error) {
	if err := OutsideRepository(path); err != nil {
		return nil, err
	}
	return Credential(path)
}

func Credential(path string) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		defer f.Close()
		info, e := f.Stat()
		if e != nil {
			return nil, e
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() != 32 {
			return nil, errors.New("unsafe credential file")
		}
		b := make([]byte, 32)
		if _, e = io.ReadFull(f, b); e != nil {
			return nil, e
		}
		return b, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return nil, err
	}
	f, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	_, err = f.Write(b)
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	return b, closeErr
}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	b, e := hex.DecodeString(r.Header.Get("Authorization"))
	digest := sha256.Sum256(b)
	principal := Principal{Actor: "operator"}
	authenticated := subtle.ConstantTimeCompare(digest[:], s.token[:]) == 1
	for key, candidate := range s.principals {
		if subtle.ConstantTimeCompare(digest[:], key[:]) == 1 {
			principal = candidate
			authenticated = true
		}
	}
	if e != nil || !authenticated {
		http.Error(w, "unauthorized", 401)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), principalKey{}, principal))
	if r.Method == "GET" && r.URL.Path == "/v1/events" && s.stream != nil {
		if principal.Task != "" {
			http.Error(w, "task principal cannot read instance events", http.StatusForbidden)
			return
		}
		s.stream(w, r)
		return
	}
	if r.Method != "POST" || r.URL.Path != "/v1/rpc" {
		http.NotFound(w, r)
		return
	}
	var req Request
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	raw, err = domain.CanonicalJSON(raw)
	if err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	var extra any
	if dec.Decode(&extra) != io.EOF || req.Version != Version || req.Method == "" {
		http.Error(w, "invalid version or trailing data", 400)
		return
	}
	if s.shutting.Load() && req.Method != "Status" {
		s.reply(w, nil, &RPCError{Code: "shutting-down", Message: "daemon is shutting down"})
		return
	}
	budget := requestBudget(req.Method)
	if budget > 5*time.Second && principal.Actor == "operator" && principal.Task == "" {
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(budget + time.Second))
	} else {
		budget = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), budget)
	defer cancel()
	if !req.Deadline.IsZero() {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithDeadline(ctx, req.Deadline)
		defer deadlineCancel()
	}
	result, err := s.handler(ctx, req)
	s.reply(w, result, err)
}
func (s *Server) reply(w http.ResponseWriter, result any, err error) {
	out := Response{Version: Version}
	if err == nil {
		out.Result, err = json.Marshal(result)
		if err != nil {
			err = &RPCError{Code: "encoding-failed", Message: "result encoding failed"}
		} else if len(out.Result) > maxResultBytes {
			out.Result = nil
			err = &RPCError{Code: "response-too-large", Message: "use a paginated read or a smaller limit"}
		}
	}
	if err != nil {
		out.Result = nil
		out.Error = err.Error()
		if errors.Is(err, context.DeadlineExceeded) {
			out.ErrorCode = "deadline-exceeded"
		}
		if errors.Is(err, context.Canceled) {
			out.ErrorCode = "cancelled"
		}
		var typed *RPCError
		if errors.As(err, &typed) {
			out.ErrorCode = typed.Code
			out.Error = typed.Message
		}
		if len(out.Error) > 4096 {
			out.Error = out.Error[:4096]
		}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// Listen refuses an existing socket; the singleton owner must reclaim stale sockets.
func (s *Server) Listen(path string) error {
	dir := filepath.Dir(path)
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return errors.New("socket directory must be owner-only")
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return err
	}
	s.listener = listener
	s.server = &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 6 * time.Second, IdleTimeout: 30 * time.Second}
	return nil
}
func (s *Server) Serve() error {
	if s.server == nil {
		return errors.New("not listening")
	}
	err := s.server.Serve(s.listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func (s *Server) Shutdown(ctx context.Context) error {
	s.shutting.Store(true)
	if s.server == nil {
		return nil
	}
	err := s.server.Shutdown(ctx)
	// http.Server.Shutdown closes only the listeners Serve adopted; a daemon
	// that fails between Listen and Serve must not leave its socket listening.
	if e := s.listener.Close(); e != nil && !errors.Is(e, net.ErrClosed) {
		err = errors.Join(err, e)
	}
	return err
}
func (s *Server) ShuttingDown() bool { return s.shutting.Load() }
func Call(ctx context.Context, socket string, credential []byte, req Request) (Response, error) {
	var result Response
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: requestBudget(req.Method)}
	body, err := json.Marshal(req)
	if err != nil {
		return result, err
	}
	r, err := http.NewRequestWithContext(ctx, "POST", "http://unix/v1/rpc", bytes.NewReader(body))
	if err != nil {
		return result, err
	}
	r.Header.Set("Authorization", hex.EncodeToString(credential))
	res, err := client.Do(r)
	if err != nil {
		return result, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return result, fmt.Errorf("RPC HTTP status %d", res.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(res.Body, MaxResponseBytes+1))
	if err != nil {
		return result, err
	}
	if len(raw) > MaxResponseBytes {
		return result, &RPCError{Code: "response-too-large", Message: "server response exceeds transport limit"}
	}
	err = json.Unmarshal(raw, &result)
	if err == nil && result.Error != "" {
		if result.ErrorCode != "" {
			err = &RPCError{Code: result.ErrorCode, Message: result.Error}
		} else {
			err = errors.New(result.Error)
		}
	}

	return result, err
}

// SetEventStream installs the authenticated streaming endpoint before Listen.
func (s *Server) SetEventStream(handler http.HandlerFunc) { s.stream = handler }

// Only filesystem backup/cleanup and bulk legacy import may exceed the ordinary request budget.
func requestBudget(method string) time.Duration {
	if method == "BackupReplicationUnpin" || method == "BackupQuota" || method == "BackupRetention" || method == "BackupReplicate" || method == "PlaneBackup" || method == "CleanupRetired" || method == "ReviewWorkspacePrepare" || method == MethodImportLegacy {
		return 2 * time.Minute
	}
	return 5 * time.Second
}
