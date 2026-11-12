// Package server is Fae's reference operator UI (FAE-API-030..033), shipped
// as the separate fae-ui executable (cmd/fae-ui), never in the core fae
// binary (FAE-GEN-010 non-goal 6). It is a loopback-only, token-gated HTTP
// server that reads the daemon through the instance RPC socket like any other
// client; the daemon supervises it through the generic UI child contract of
// package ui.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/ui"
)

//go:embed assets
var assets embed.FS

// Options is the child's launch contract; the secrets come from SecretsDir.
type Options struct {
	Bind, Dist, Socket, SecretsDir, RPCLog, Instance string
	Port                                             int
	// WatchParent exits when the launching daemon goes away.
	WatchParent bool
}

// Main parses the child flags and serves until SIGTERM, SIGINT or parent death.
func Main(args []string, stderr io.Writer) int {
	f := flag.NewFlagSet("fae-ui", flag.ContinueOnError)
	f.SetOutput(stderr)
	o := Options{WatchParent: true}
	f.StringVar(&o.Bind, "bind", "", "literal loopback bind address")
	f.IntVar(&o.Port, "port", 0, "listen port")
	f.StringVar(&o.Dist, "dist", "", "static assets directory (default embedded)")
	f.StringVar(&o.Socket, "socket", "", "daemon RPC socket")
	f.StringVar(&o.SecretsDir, "secrets-dir", "", "private directory holding the launch secrets")
	f.StringVar(&o.RPCLog, "rpc-log", "", "append-only RPC log")
	f.StringVar(&o.Instance, "instance", "", "instance name")
	if f.Parse(args) != nil || f.NArg() != 0 {
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := Run(ctx, o, nil); err != nil {
		fmt.Fprintln(stderr, "fae-ui:", err)
		if errors.Is(err, errRefused) {
			return 2
		}
		return 1
	}
	return 0
}

var errRefused = errors.New("refused")

type server struct {
	o            Options
	token        [32]byte
	cookie       string
	readCred     []byte
	writeCred    []byte
	files        fs.FS
	hosts        map[string]bool
	origins      map[string]bool
	logMu        sync.Mutex
	rpcLog       *os.File
	call         func(context.Context, []byte, api.Request) (api.Response, error)
	shutdownWait time.Duration
}

// Run consumes the launch secrets, binds the loopback listener and serves.
// ready, when set, receives the bound address.
func Run(ctx context.Context, o Options, ready func(net.Addr)) error {
	bind, err := ui.ValidateBind(o.Bind)
	if err != nil {
		return fmt.Errorf("%w: %v", errRefused, err)
	}
	if o.Port < 0 || o.Port > 65535 || o.Socket == "" {
		return fmt.Errorf("%w: port 0..65535 and daemon socket required", errRefused)
	}
	s := &server{o: o, shutdownWait: 2 * time.Second, call: func(ctx context.Context, credential []byte, r api.Request) (api.Response, error) {
		return api.Call(ctx, o.Socket, credential, r)
	}}
	token, read, write, err := readSecrets(o.SecretsDir)
	if err != nil {
		return err
	}
	s.token, s.readCred, s.writeCred = sha256.Sum256([]byte(token)), read, write
	if s.files, err = staticFiles(o.Dist); err != nil {
		return err
	}
	if o.RPCLog != "" {
		if s.rpcLog, err = os.OpenFile(o.RPCLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0600); err != nil {
			return err
		}
		defer s.rpcLog.Close()
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(bind, strconv.Itoa(o.Port)))
	if err != nil {
		return err
	}
	defer listener.Close()
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || !ui.MayListen(true, addr.IP.IsLoopback(), addr.IP.IsUnspecified()) {
		return fmt.Errorf("%w: listener %s is not loopback", errRefused, listener.Addr())
	}
	port := strconv.Itoa(addr.Port)
	s.cookie = "fae_ui_" + port
	s.hosts = map[string]bool{net.JoinHostPort(addr.IP.String(), port): true}
	if addr.IP.To4() != nil {
		s.hosts["localhost:"+port] = true
	}
	s.origins = map[string]bool{}
	for h := range s.hosts {
		s.origins["http://"+h] = true
	}
	httpServer := &http.Server{Handler: s, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	if ready != nil {
		ready(listener.Addr())
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if o.WatchParent {
		go watchParent(ctx, cancel, os.Getppid())
	}
	select {
	case err = <-served:
		return err
	case <-ctx.Done():
	}
	shutdown, stop := context.WithTimeout(context.Background(), s.shutdownWait)
	defer stop()
	_ = httpServer.Shutdown(shutdown)
	return nil
}

// watchParent backs up the parent-death signal: a reparented child exits.
func watchParent(ctx context.Context, cancel context.CancelFunc, parent int) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if os.Getppid() != parent {
				cancel()
				return
			}
		}
	}
}

const contentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// tokenMatches hashes the candidate to a fixed-length digest before a
// constant-time comparison, so neither length nor prefix ends it early.
func (s *server) tokenMatches(candidate string) bool {
	digest := sha256.Sum256([]byte(candidate))
	return subtle.ConstantTimeCompare(digest[:], s.token[:]) == 1
}

// authorized checks every presented token form: bearer header, session cookie
// or the one-time ?token= bootstrap.
func (s *server) authorized(r *http.Request) (ok, viaQuery bool) {
	if v, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); found && s.tokenMatches(v) {
		ok = true
	}
	if c, e := r.Cookie(s.cookie); e == nil && s.tokenMatches(c.Value) {
		ok = true
	}
	if v := r.URL.Query().Get("token"); v != "" && s.tokenMatches(v) {
		ok, viaQuery = true, true
	}
	return ok, viaQuery
}

func loopbackPeer(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	ip := net.ParseIP(host)
	return err == nil && ip != nil && ip.IsLoopback()
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	peer, host := loopbackPeer(r.RemoteAddr), s.hosts[r.Host]
	authorized, viaQuery := s.authorized(r)
	if !ui.MayServeRequest(peer, host, authorized) {
		if !peer || !host {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "UI accepts only loopback peers addressing its loopback host"})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "UI token required; run fae ui token"})
		return
	}
	if viaQuery {
		// Exchange the bootstrap token for a session cookie and drop it from the URL.
		http.SetCookie(w, &http.Cookie{Name: s.cookie, Value: r.URL.Query().Get("token"), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		q := r.URL.Query()
		q.Del("token")
		target := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
		http.Redirect(w, r, target.String(), http.StatusSeeOther)
		return
	}
	switch {
	case r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "writes": s.writeCred != nil})
	case r.URL.Path == "/api/alerts/ack":
		s.acknowledge(w, r)
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "UI is read-only"})
	case strings.HasPrefix(r.URL.Path, "/api/"):
		s.handleRead(w, r)
	default:
		s.static(w, r)
	}
}

func (s *server) handleRead(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	number := func(name string, fallback int64) (int64, bool) {
		raw := q.Get(name)
		if raw == "" {
			return fallback, true
		}
		v, e := strconv.ParseInt(raw, 10, 64)
		return v, e == nil && v >= 0
	}
	// Board pages by task ID; history and alerts by a numeric cursor.
	after, okAfter := number("after", 0)
	limit, okLimit := number("limit", 100)
	if !okAfter && r.URL.Path != "/api/board" || !okLimit || limit < 1 || limit > 500 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "after must be a nonnegative cursor and limit 1..500"})
		return
	}
	retired := q.Get("include_retired") == "true"
	var method string
	var params any
	switch r.URL.Path {
	case "/api/session":
		writeJSON(w, http.StatusOK, map[string]any{"instance": s.o.Instance, "writes": s.writeCred != nil})
		return
	case "/api/status":
		method, params = "Status", map[string]any{"include_retired": retired}
	case "/api/board":
		method, params = "Board", map[string]any{"after": q.Get("after"), "limit": limit, "include_retired": retired}
	case "/api/history":
		if q.Get("task") == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "task required"})
			return
		}
		method, params = "History", map[string]any{"id": q.Get("task"), "summary": true, "after": after, "limit": limit, "include_retired": retired}
	case "/api/alerts":
		method, params = "Alerts", map[string]any{"after": after, "limit": limit, "unacknowledged": q.Get("unacknowledged") == "true"}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown UI read"})
		return
	}
	s.forward(w, r.Context(), s.readCred, method, params)
}

// acknowledge is the built-in UI's one write: an audited alert acknowledgement
// through the daemon's own RPC with the operator's authorizing message.
func (s *server) acknowledge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
		return
	}
	// A cross-site form can carry neither a JSON body nor this origin.
	if origin := r.Header.Get("Origin"); origin != "" && !s.origins[origin] || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "same-origin JSON request required"})
		return
	}
	if !ui.MayWrite(true, s.writeCred != nil) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "UI is read-only; start serve with --ui-allow-writes"})
		return
	}
	var p struct {
		ID                 int64  `json:"id"`
		AuthorizingMessage string `json:"authorizing_message"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || p.ID <= 0 || p.AuthorizingMessage == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id and authorizing_message required"})
		return
	}
	s.forward(w, r.Context(), s.writeCred, "AlertAcknowledge", p)
}

func (s *server) forward(w http.ResponseWriter, ctx context.Context, credential []byte, method string, params any) {
	raw, err := json.Marshal(params)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "encoding failed"})
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	started := time.Now()
	res, err := s.call(ctx, credential, api.Request{Version: api.Version, Method: method, Params: raw})
	code := ""
	var typed *api.RPCError
	if errors.As(err, &typed) {
		code = typed.Code
	}
	s.logRPC(method, time.Since(started), err, code)
	if err != nil {
		status := http.StatusBadGateway
		if typed != nil || res.Error != "" {
			status = http.StatusConflict
		}
		writeJSON(w, status, map[string]string{"error": err.Error(), "code": code})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Result)
}

// logRPC appends one line per daemon call: method and outcome only, never
// parameters, credentials or the token.
func (s *server) logRPC(method string, took time.Duration, err error, code string) {
	if s.rpcLog == nil {
		return
	}
	line, _ := json.Marshal(map[string]any{"time": time.Now().UTC().Format(time.RFC3339Nano), "instance": s.o.Instance, "component": "ui", "method": method, "ms": took.Milliseconds(), "ok": err == nil, "code": code})
	s.logMu.Lock()
	defer s.logMu.Unlock()
	_, _ = s.rpcLog.Write(append(line, '\n'))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// staticFiles serves the embedded assets, or an operator directory opened as
// an os.Root so no path or symlink can escape it.
func staticFiles(dist string) (fs.FS, error) {
	if dist == "" {
		return fs.Sub(assets, "assets")
	}
	root, err := os.OpenRoot(dist)
	if err != nil {
		return nil, err
	}
	return root.FS(), nil
}

func (s *server) static(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if name == "" {
		name = "index.html"
	}
	for _, part := range strings.Split(name, "/") {
		if strings.HasPrefix(part, ".") {
			http.NotFound(w, r)
			return
		}
	}
	if !fs.ValidPath(name) {
		http.NotFound(w, r)
		return
	}
	f, err := s.files.Open(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	content, ok := f.(io.ReadSeeker)
	if !ok {
		raw, e := io.ReadAll(io.LimitReader(f, 8<<20))
		if e != nil {
			http.NotFound(w, r)
			return
		}
		content = strings.NewReader(string(raw))
	}
	http.ServeContent(w, r, name, info.ModTime(), content)
}

// consumeSecret reads an owner-only regular secret of exactly size bytes and
// removes it. A missing optional file returns nil, nil.
func consumeSecret(path string, size int, optional bool) ([]byte, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if optional && os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	b := make([]byte, size+1)
	n, _ := io.ReadFull(f, b)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || n != size {
		return nil, fmt.Errorf("invalid UI secret %s", filepath.Base(path))
	}
	if e = os.Remove(path); e != nil {
		return nil, e
	}
	return b[:n], nil
}

// readSecrets consumes the launch secrets from the private directory.
func readSecrets(dir string) (token string, read, write []byte, err error) {
	if !filepath.IsAbs(dir) {
		return "", nil, nil, errors.New("absolute UI secrets directory required")
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", nil, nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return "", nil, nil, errors.New("UI secrets directory must be owner-only")
	}
	raw, err := consumeSecret(filepath.Join(dir, ui.TokenFile), 64, false)
	if err != nil {
		return "", nil, nil, err
	}
	if _, err = hex.DecodeString(string(raw)); err != nil {
		return "", nil, nil, errors.New("invalid UI token encoding")
	}
	if read, err = consumeSecret(filepath.Join(dir, ui.CredentialFile), 32, false); err != nil {
		return "", nil, nil, err
	}
	write, err = consumeSecret(filepath.Join(dir, ui.WriteCredentialFile), 32, true)
	return string(raw), read, write, err
}
