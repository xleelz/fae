package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/ui"
)

type seen struct {
	Actor, Method string
	Params        json.RawMessage
}

// fakeDaemon serves the instance RPC with the UI's delegated principals.
type fakeDaemon struct {
	socket      string
	read, write []byte
	mu          sync.Mutex
	calls       []seen
}

func secret(t *testing.T) []byte {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// shortDir keeps socket paths under the 108-byte Unix limit under any
// TMPDIR (a brokered run's TMPDIR beside a task worktree is long).
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "fae-ui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func startFakeDaemon(t *testing.T, dir string) *fakeDaemon {
	t.Helper()
	run := filepath.Join(dir, "run")
	if err := os.Mkdir(run, 0700); err != nil {
		t.Fatal(err)
	}
	f := &fakeDaemon{socket: filepath.Join(run, "rpc.sock"), read: secret(t), write: secret(t)}
	server, err := api.New(secret(t), func(ctx context.Context, r api.Request) (any, error) {
		p, _ := api.AuthenticatedPrincipal(ctx)
		f.mu.Lock()
		f.calls = append(f.calls, seen{p.Actor, r.Method, r.Params})
		f.mu.Unlock()
		if r.Method == "AlertAcknowledge" && p.Actor != "ui-writer" {
			return nil, &api.RPCError{Code: "forbidden", Message: "actor is not permitted to perform action"}
		}
		return map[string]any{"method": r.Method, "actor": p.Actor}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.AddPrincipal(f.read, api.Principal{Actor: "ui"}); err != nil {
		t.Fatal(err)
	}
	if err = server.AddPrincipal(f.write, api.Principal{Actor: "ui-writer"}); err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(f.socket); err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	t.Cleanup(func() { server.Shutdown(context.Background()) })
	return f
}

func (f *fakeDaemon) seen() []seen {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]seen(nil), f.calls...)
}

type running struct {
	base, token, host, rpcLog, secrets string
	daemon                             *fakeDaemon
}

// startUI writes the launch secrets as the daemon does and runs the server.
func startUI(t *testing.T, writes bool, dist string) running {
	t.Helper()
	dir := shortDir(t)
	f := startFakeDaemon(t, dir)
	secrets := filepath.Join(dir, "ui")
	if err := os.Mkdir(secrets, 0700); err != nil {
		t.Fatal(err)
	}
	token, err := ui.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{ui.TokenFile: []byte(token), ui.CredentialFile: f.read}
	if writes {
		files[ui.WriteCredentialFile] = f.write
	}
	for name, b := range files {
		if err = ui.WriteSecret(filepath.Join(secrets, name), b); err != nil {
			t.Fatal(err)
		}
	}
	rpcLog := filepath.Join(dir, "ui-rpc.log")
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Bind: "127.0.0.1", Socket: f.socket, SecretsDir: secrets, RPCLog: rpcLog, Instance: "t", Dist: dist}, func(a net.Addr) { ready <- a })
	}()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	select {
	case a := <-ready:
		return running{base: "http://" + a.String(), token: token, host: a.String(), rpcLog: rpcLog, secrets: secrets, daemon: f}
	case err := <-done:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("UI did not start")
	}
	return running{}
}

var client = &http.Client{Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (r running) do(t *testing.T, method, path string, header map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, r.base+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res, string(raw)
}

func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token}
}

// FAE-API-030: every non-loopback or wildcard bind is refused, naming the
// value, before any secret is consumed or listener opened.
func TestUIRefusesNonLoopbackBind(t *testing.T) {
	for _, bind := range []string{"0.0.0.0", "::", "[::]", "192.168.1.10", "10.0.0.1", "fe80::1", "", "example.com", "127.0.0.1%lo", "localhost.example"} {
		dir := shortDir(t)
		err := Run(context.Background(), Options{Bind: bind, Socket: filepath.Join(dir, "rpc.sock"), SecretsDir: dir}, func(net.Addr) { t.Errorf("bind %q listened", bind) })
		if !errors.Is(err, errRefused) || !strings.Contains(err.Error(), "\""+bind+"\"") {
			t.Fatalf("Run bind %q: %v", bind, err)
		}
	}
	if code := Main([]string{"--bind", "0.0.0.0", "--port", "0", "--socket", "/nonexistent", "--secrets-dir", "/nonexistent"}, io.Discard); code != 2 {
		t.Fatalf("Main non-loopback exit %d", code)
	}
}

// FAE-API-031: the token is required on every request, static or API, and the
// bootstrap exchanges it for an HttpOnly, SameSite=Strict session cookie.
func TestUITokenRequiredOnEveryRequest(t *testing.T) {
	r := startUI(t, false, "")
	paths := []string{"/", "/index.html", "/app.js", "/app.css", "/healthz", "/api/status", "/api/board", "/api/alerts", "/api/session", "/api/history?task=x", "/missing"}
	wrong := []map[string]string{nil, bearer(""), bearer(strings.Repeat("0", 64)), bearer(r.token[:63]), bearer(r.token + "0"), bearer(r.token[1:] + "0"), {"Authorization": r.token}, {"Cookie": "fae_ui_0=" + r.token}}
	for _, p := range paths {
		for i, h := range wrong {
			if res, body := r.do(t, "GET", p, h, ""); res.StatusCode != http.StatusUnauthorized || strings.Contains(body, r.token) {
				t.Fatalf("%s credential %d: %d %s", p, i, res.StatusCode, body)
			}
		}
		res, body := r.do(t, "GET", p, bearer(r.token), "")
		if want := http.StatusOK; p == "/missing" && res.StatusCode != http.StatusNotFound || p != "/missing" && res.StatusCode != want {
			t.Fatalf("%s authorized: %d %s", p, res.StatusCode, body)
		}
		if res.Header.Get("Content-Security-Policy") == "" || res.Header.Get("Cache-Control") != "no-store" {
			t.Fatalf("%s headers %v", p, res.Header)
		}
	}
	// A rebinding host name is refused even with the token.
	if res, _ := r.do(t, "GET", "/api/status", map[string]string{"Authorization": "Bearer " + r.token, "Host": "attacker.example"}, ""); res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host %d", res.StatusCode)
	}
	res, _ := r.do(t, "GET", "/?token="+r.token, nil, "")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("bootstrap %d %v", res.StatusCode, res.Header)
	}
	cookies := res.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode || cookies[0].Value != r.token {
		t.Fatalf("session cookie %+v", cookies)
	}
	if res, body := r.do(t, "GET", "/", map[string]string{"Cookie": cookies[0].Name + "=" + cookies[0].Value}, ""); res.StatusCode != 200 || !strings.Contains(body, "app.js") {
		t.Fatalf("cookie session %d %s", res.StatusCode, body)
	}
	if res, _ := r.do(t, "GET", "/?token="+r.token[:63]+"x", nil, ""); res.StatusCode != http.StatusUnauthorized || len(res.Cookies()) != 0 {
		t.Fatalf("wrong bootstrap token %d", res.StatusCode)
	}
	if res, _ := r.do(t, "GET", "/api/board?after=a&token="+r.token, nil, ""); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/api/board?after=a" {
		t.Fatalf("bootstrap keeps other parameters: %v", res.Header)
	}
	for _, name := range []string{ui.TokenFile, ui.CredentialFile} {
		if _, err := os.Stat(filepath.Join(r.secrets, name)); !os.IsNotExist(err) {
			t.Fatalf("launch secret %s not consumed: %v", name, err)
		}
	}
}

// FAE-API-032: reads use the UI's read principal; a write without the
// capability is refused before reaching the daemon.
func TestUIReadOnlyByDefault(t *testing.T) {
	r := startUI(t, false, "")
	for path, method := range map[string]string{"/api/status": "Status", "/api/board?limit=5&after=task-b": "Board", "/api/history?task=t1&after=3": "History", "/api/alerts?unacknowledged=true": "Alerts"} {
		res, body := r.do(t, "GET", path, bearer(r.token), "")
		var got map[string]string
		if res.StatusCode != 200 || json.Unmarshal([]byte(body), &got) != nil || got["method"] != method || got["actor"] != "ui" {
			t.Fatalf("%s: %d %s", path, res.StatusCode, body)
		}
	}
	calls := r.daemon.seen()
	sort.Slice(calls, func(i, j int) bool { return calls[i].Method < calls[j].Method })
	var history map[string]any
	for _, c := range calls {
		if c.Method == "History" {
			_ = json.Unmarshal(c.Params, &history)
		}
	}
	if history["id"] != "t1" || history["summary"] != true || history["after"] != float64(3) {
		t.Fatalf("history params %v", history)
	}
	for _, c := range calls {
		if c.Method == "Board" && !bytes.Contains(c.Params, []byte(`"after":"task-b"`)) {
			t.Fatalf("board cursor %s", c.Params)
		}
	}
	for _, bad := range []string{"/api/history?task=t1&after=x", "/api/alerts?after=-1", "/api/board?limit=0", "/api/board?limit=501"} {
		if res, _ := r.do(t, "GET", bad, bearer(r.token), ""); res.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s: %d", bad, res.StatusCode)
		}
	}
	before := len(r.daemon.seen())
	header := map[string]string{"Authorization": "Bearer " + r.token, "Content-Type": "application/json"}
	if res, body := r.do(t, "POST", "/api/alerts/ack", header, `{"id":1,"authorizing_message":"m"}`); res.StatusCode != http.StatusForbidden || !strings.Contains(body, "read-only") {
		t.Fatalf("read-only write %d %s", res.StatusCode, body)
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		if res, _ := r.do(t, method, "/api/status", bearer(r.token), "{}"); res.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s accepted: %d", method, res.StatusCode)
		}
	}
	if len(r.daemon.seen()) != before {
		t.Fatal("refused write reached the daemon")
	}
	if res, body := r.do(t, "GET", "/api/session", bearer(r.token), ""); res.StatusCode != 200 || !strings.Contains(body, `"writes":false`) {
		t.Fatal(body)
	}
	raw, err := os.ReadFile(r.rpcLog)
	if err != nil || !bytes.Contains(raw, []byte(`"method":"Status"`)) {
		t.Fatalf("rpc log %s %v", raw, err)
	}
	for _, s := range []string{r.token, hex.EncodeToString(r.daemon.read), hex.EncodeToString(r.daemon.write)} {
		if bytes.Contains(raw, []byte(s)) {
			t.Fatal("rpc log contains a secret")
		}
	}
	if info, err := os.Stat(r.rpcLog); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}

// FAE-API-032: with the capability, a same-origin JSON write reaches the
// daemon under the distinct ui-writer principal.
func TestUIWriteCapability(t *testing.T) {
	r := startUI(t, true, "")
	header := map[string]string{"Authorization": "Bearer " + r.token, "Content-Type": "application/json", "Origin": "http://" + r.host}
	res, body := r.do(t, "POST", "/api/alerts/ack", header, `{"id":7,"authorizing_message":"m"}`)
	if res.StatusCode != 200 || !strings.Contains(body, `"actor":"ui-writer"`) {
		t.Fatalf("write %d %s", res.StatusCode, body)
	}
	for name, h := range map[string]map[string]string{
		"cross origin": {"Authorization": "Bearer " + r.token, "Content-Type": "application/json", "Origin": "http://attacker.example"},
		"form body":    {"Authorization": "Bearer " + r.token, "Content-Type": "application/x-www-form-urlencoded"},
		"anonymous":    {"Content-Type": "application/json"},
	} {
		if res, _ := r.do(t, "POST", "/api/alerts/ack", h, `{"id":7,"authorizing_message":"m"}`); res.StatusCode == 200 {
			t.Fatalf("%s write accepted", name)
		}
	}
	if res, _ := r.do(t, "POST", "/api/alerts/ack", header, `{"id":7}`); res.StatusCode != http.StatusBadRequest {
		t.Fatal("write without authorizing message accepted")
	}
	writes := 0
	for _, c := range r.daemon.seen() {
		if c.Method == "AlertAcknowledge" {
			writes++
			if c.Actor != "ui-writer" || !bytes.Contains(c.Params, []byte(`"authorizing_message":"m"`)) {
				t.Fatalf("write call %+v", c)
			}
		}
	}
	if writes != 1 {
		t.Fatalf("writes reaching daemon %d", writes)
	}
}

// A comparison that exits on the first differing byte would reject a token
// differing at its first byte faster than one differing at its last.
func TestUITokenComparisonIsPrefixIndependent(t *testing.T) {
	token, _ := ui.NewToken()
	s := &server{}
	s.token = sha256.Sum256([]byte(token))
	first := "x" + token[1:]
	last := token[:63] + "x"
	measure := func(candidate string) time.Duration {
		best := time.Duration(1 << 62)
		for range 2000 {
			start := time.Now()
			for range 20 {
				if s.tokenMatches(candidate) {
					t.Fatal("wrong token matched")
				}
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	a, b := measure(first), measure(last)
	if a > 2*b || b > 2*a {
		t.Fatalf("prefix-dependent comparison: first-byte %v last-byte %v", a, b)
	}
	if !s.tokenMatches(token) {
		t.Fatal("token refused")
	}
}

// --ui-dist serves only regular, non-hidden files inside the directory.
func TestUIDistServesOnlyContainedFiles(t *testing.T) {
	dist := shortDir(t)
	for name, body := range map[string]string{"index.html": "custom-index", ".secret": "hidden"} {
		if err := os.WriteFile(filepath.Join(dist, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("/etc/hostname", filepath.Join(dist, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dist, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	r := startUI(t, false, dist)
	if res, body := r.do(t, "GET", "/", bearer(r.token), ""); res.StatusCode != 200 || body != "custom-index" {
		t.Fatalf("index %d %q", res.StatusCode, body)
	}
	for _, p := range []string{"/.secret", "/escape.txt", "/sub", "/sub/", "/%2e%2e/etc/hostname", "/../../etc/hostname"} {
		if res, body := r.do(t, "GET", p, bearer(r.token), ""); res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s served: %d %q", p, res.StatusCode, body)
		}
	}
}

// Launch secrets must be private regular files of the exact size.
func TestUIRefusesUnsafeSecrets(t *testing.T) {
	dir := shortDir(t)
	secrets := filepath.Join(dir, "ui")
	if err := os.Mkdir(secrets, 0700); err != nil {
		t.Fatal(err)
	}
	token, _ := ui.NewToken()
	if err := os.WriteFile(filepath.Join(secrets, ui.TokenFile), []byte(token), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ui.WriteSecret(filepath.Join(secrets, ui.CredentialFile), secret(t)); err != nil {
		t.Fatal(err)
	}
	err := Run(context.Background(), Options{Bind: "127.0.0.1", Socket: filepath.Join(dir, "rpc.sock"), SecretsDir: secrets}, func(net.Addr) { t.Error("listened with unsafe token file") })
	if err == nil || !strings.Contains(err.Error(), "invalid UI secret") {
		t.Fatal(err)
	}
}
