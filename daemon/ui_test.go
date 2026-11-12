package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/ui"
)

// fastUISupervision shortens Appendix A's UI timings for a test.
func fastUISupervision(t *testing.T, limit int) {
	t.Helper()
	base, top, window, max, probe, wait := uiBackoffBase, uiBackoffMax, uiRestartWindow, uiRestartLimit, uiProbeInterval, uiStopWait
	t.Cleanup(func() {
		uiBackoffBase, uiBackoffMax, uiRestartWindow, uiRestartLimit, uiProbeInterval, uiStopWait = base, top, window, max, probe, wait
	})
	uiBackoffBase, uiBackoffMax, uiRestartWindow, uiRestartLimit, uiProbeInterval, uiStopWait = 20*time.Millisecond, 80*time.Millisecond, time.Hour, limit, 100*time.Millisecond, 3*time.Second
}

func freeLoopbackPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func uiState(d *Daemon) UIStatus {
	if s := d.ui.state.Load(); s != nil {
		return *s
	}
	return UIStatus{}
}

func awaitUI(t *testing.T, d *Daemon, what string, ok func(UIStatus) bool) UIStatus {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if s := uiState(d); ok(s) {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("UI child never %s: %+v", what, uiState(d))
	return UIStatus{}
}

func childEvents(t *testing.T, d *Daemon) map[string]int {
	t.Helper()
	events, err := d.store.Events(context.Background(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, e := range events {
		if strings.HasPrefix(e.Kind, "child-") && bytes.Contains(e.Payload, []byte(`"child":"ui"`)) {
			kinds[e.Kind]++
		}
	}
	return kinds
}

func uiGet(t *testing.T, url, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := (&http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(raw)
}

// FAE-API-030: a non-loopback or wildcard UI bind refuses the start, naming the
// value, before the instance root, lock, plane or any listener exists.
func TestUIRefusesNonLoopbackBindAtStart(t *testing.T) {
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	if err := os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	home := filepath.Join(root, "instances")
	for _, bind := range []string{"0.0.0.0", "::", "192.168.1.10", "fe80::1", "example.com"} {
		_, err := Start(context.Background(), Config{Root: home, Instance: "bind", Workspace: ws, DoltBinary: "/nonexistent/dolt", UI: &UIConfig{Bind: bind}})
		if err == nil || !strings.Contains(err.Error(), "\""+bind+"\"") {
			t.Fatalf("bind %q: %v", bind, err)
		}
		if _, err = os.Stat(home); !os.IsNotExist(err) {
			t.Fatalf("bind %q created the instance root: %v", bind, err)
		}
	}
	if _, err := Start(context.Background(), Config{Root: home, Instance: "bind", Workspace: ws, DoltBinary: "/nonexistent/dolt", UI: &UIConfig{Binary: "relative/ui"}}); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatal("relative UI binary accepted", err)
	}
}

// FAE-API-031..033, FAE-OBS-002: the built-in child starts after readiness,
// is token-gated and read-only, restarts after a crash while the drive keeps
// its round, reports in status even during shutdown, and is reaped on Close.
func TestUIChildLifecycle(t *testing.T) {
	cfg, ctx := fixture(t)
	fastUISupervision(t, 10)
	cfg.UI = &UIConfig{Binary: buildUIChild(t, filepath.Dir(cfg.Workspace)), Port: freeLoopbackPort(t)}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			stop(t, d)
		}
	}()
	first := awaitUI(t, d, "became healthy", func(s UIStatus) bool { return s.Health == "healthy" && s.PID != nil })
	if !first.Enabled || first.URL != ui.URL("127.0.0.1", cfg.UI.Port) || first.Restarts != 0 || first.WritesEnabled {
		t.Fatalf("UI status %+v", first)
	}
	if d.UIURL() != first.URL {
		t.Fatal(d.UIURL())
	}
	reply, err := call(t, d, "Status", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		UI map[string]any `json:"ui"`
	}
	if err = json.Unmarshal(reply.Result, &status); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"url", "pid", "health", "restarts", "writes_enabled"} {
		if _, ok := status.UI[field]; !ok {
			t.Fatalf("status ui lacks %s: %v", field, status.UI)
		}
	}
	reply, err = call(t, d, "UIToken", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var token struct {
		Token, URL string
		LoginURL   string `json:"login_url"`
	}
	if err = json.Unmarshal(reply.Result, &token); err != nil || len(token.Token) != 64 || token.URL != first.URL || token.LoginURL != first.URL+"?token="+token.Token {
		t.Fatalf("UI token %s %v", reply.Result, err)
	}
	if code, _ := uiGet(t, first.URL+"healthz", ""); code != http.StatusUnauthorized {
		t.Fatalf("anonymous request %d", code)
	}
	if code, body := uiGet(t, first.URL+"api/status", token.Token); code != 200 || !strings.Contains(body, `"instance":"test"`) || strings.Contains(body, token.Token) {
		t.Fatalf("UI status read %d %s", code, body)
	}
	// The UI's own credential reads but cannot mutate or fetch the token.
	asUI := func(method string, p any) error {
		raw, _ := json.Marshal(p)
		_, err := api.Call(context.Background(), d.layout.Socket, d.ui.read, api.Request{Version: 1, Method: method, Params: raw})
		return err
	}
	if err = asUI("Board", map[string]any{}); err != nil {
		t.Fatal("UI read refused", err)
	}
	authorize(t, d)
	for method, p := range map[string]any{"Admit": AdmitParams{ID: "ui-write", Tree: "tree", AuthorizingMessage: "message"}, "AlertAcknowledge": map[string]any{"id": 1, "authorizing_message": "message"}, "Shutdown": map[string]any{"authorizing_message": "message"}} {
		if err = asUI(method, p); err == nil || !strings.Contains(err.Error(), "actor is not permitted to perform action") {
			t.Fatalf("UI credential %s: %v", method, err)
		}
	}
	if err = asUI("UIToken", map[string]any{}); err == nil || !strings.Contains(err.Error(), "only to the instance operator") {
		t.Fatalf("UI credential fetched the token: %v", err)
	}
	// Crash the child: it restarts within the bound while the drive admits and
	// completes a task.
	if err = syscall.Kill(*first.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "during-ui-restart", Tree: "tree", AuthorizingMessage: "message"}, "during-ui-restart"); err != nil {
		t.Fatal(err)
	}
	second := awaitUI(t, d, "restarted", func(s UIStatus) bool { return s.Health == "healthy" && s.PID != nil && *s.PID != *first.PID })
	if second.Restarts != 1 || !strings.Contains(second.LastExit, "killed") {
		t.Fatalf("restart status %+v", second)
	}
	awaitState(t, d, "during-ui-restart", "Done")
	if kinds := childEvents(t, d); kinds["child-started"] != 1 || kinds["child-crashed"] != 1 || kinds["child-restarted"] != 1 {
		t.Fatalf("child lifecycle events %v", kinds)
	}
	// FAE-OBS-002: status stays answerable once shutdown begins.
	d.cancel()
	reply, err = call(t, d, "Status", map[string]any{}, "")
	if err != nil || !bytes.Contains(reply.Result, []byte(`"shutting_down":true`)) || !bytes.Contains(reply.Result, []byte(`"url":"`+first.URL+`"`)) {
		t.Fatalf("status during shutdown %s %v", reply.Result, err)
	}
	began := time.Now()
	closed = true
	stop(t, d)
	if took := time.Since(began); took > 30*time.Second {
		t.Fatalf("shutdown took %s", took)
	}
	if err = syscall.Kill(*second.PID, 0); err != syscall.ESRCH {
		t.Fatalf("UI child %d survived shutdown: %v", *second.PID, err)
	}
	if s := uiState(d); s.Health != "stopped" || s.PID != nil {
		t.Fatalf("stopped status %+v", s)
	}
	for _, name := range []string{ui.TokenFile, ui.CredentialFile, ui.WriteCredentialFile} {
		if _, err = os.Stat(filepath.Join(d.layout.Run, "ui", name)); !os.IsNotExist(err) {
			t.Fatalf("launch secret %s left behind: %v", name, err)
		}
	}
	active, _ := filepath.Glob(filepath.Join(d.layout.Run, "ui-processes", "process-*.json"))
	if len(active) != 0 {
		t.Fatalf("unretired UI process records %v", active)
	}
	logRaw, err := os.ReadFile(d.layout.Log)
	if err != nil || bytes.Contains(logRaw, []byte(token.Token)) || !bytes.Contains(logRaw, []byte("UI child restarted")) {
		t.Fatalf("supervision log token leak or missing restart line: %v", err)
	}
}

// FAE-API-032: --ui-allow-writes issues a distinct write capability; a UI write
// still needs a message directing the exact verb, and is audited as ui-writer.
func TestUIWriteCapabilityGoesThroughAuthorizedRPC(t *testing.T) {
	cfg, ctx := fixture(t)
	fastUISupervision(t, 10)
	cfg.UI = &UIConfig{Binary: buildUIChild(t, filepath.Dir(cfg.Workspace)), Port: freeLoopbackPort(t), AllowWrites: true}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	s := awaitUI(t, d, "became healthy", func(s UIStatus) bool { return s.Health == "healthy" })
	if !s.WritesEnabled {
		t.Fatalf("status lacks writes_enabled: %+v", s)
	}
	alert, _, err := d.raiseAlert(ctx, controlplane.Alert{Source: "ui-test", Detail: json.RawMessage(`{"x":1}`)}, false)
	if err != nil {
		t.Fatal(err)
	}
	for id, text := range map[string]string{"vague": `{"text":"Acknowledge alerts.","authorize":{"classes":["runtime"]}}`, "directed": `{"text":"Acknowledge the test alert from the UI.","authorize":{"verbs":["AlertAcknowledge"]}}`} {
		if _, err = call(t, d, "Message", map[string]string{"id": id, "text": text}, ""); err != nil {
			t.Fatal(err)
		}
	}
	reply, err := call(t, d, "UIToken", map[string]any{}, "")
	if err != nil {
		t.Fatal(err)
	}
	var token struct{ Token string }
	_ = json.Unmarshal(reply.Result, &token)
	post := func(message string) (int, string) {
		body := strings.NewReader(`{"id":` + jsonInt(alert.ID) + `,"authorizing_message":"` + message + `"}`)
		req, _ := http.NewRequest("POST", s.URL+"api/alerts/ack", body)
		req.Header.Set("Authorization", "Bearer "+token.Token)
		req.Header.Set("Content-Type", "application/json")
		res, err := (&http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		raw, _ := io.ReadAll(res.Body)
		return res.StatusCode, string(raw)
	}
	if code, body := post("vague"); code == 200 || !strings.Contains(body, "explicitly direct") {
		t.Fatalf("undirected UI write %d %s", code, body)
	}
	if code, body := post("directed"); code != 200 {
		t.Fatalf("directed UI write %d %s", code, body)
	}
	rows, err := d.store.Alerts(ctx, 0, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.ID == alert.ID && row.AcknowledgedAt == nil {
			t.Fatal("UI acknowledgement not applied")
		}
	}
	var actor string
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect UI write audit", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT actor FROM audit WHERE action='AlertAcknowledge' AND message_id='directed' ORDER BY id DESC LIMIT 1`).Scan(&actor)
	})
	if err != nil || actor != "ui-writer" {
		t.Fatalf("audited actor %q %v", actor, err)
	}
}

// FAE-API-033: a child that always dies is restarted with backoff up to the
// bound, then left stopped with exactly one alert; the daemon keeps running.
func TestUIRestartExhaustionRaisesOneAlert(t *testing.T) {
	cfg, ctx := fixture(t)
	fastUISupervision(t, 3)
	dying := filepath.Join(filepath.Dir(cfg.Workspace), "dying-ui")
	if err := os.WriteFile(dying, []byte("#!/bin/sh\necho dying >&2\nexit 3\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.UI = &UIConfig{Binary: dying, Port: freeLoopbackPort(t)}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	s := awaitUI(t, d, "exhausted", func(s UIStatus) bool { return s.Health == "exhausted" })
	if s.Restarts != 3 || s.PID != nil || !strings.Contains(s.LastExit, "exit status 3") {
		t.Fatalf("exhausted status %+v", s)
	}
	time.Sleep(300 * time.Millisecond) // no further launch may follow
	if kinds := childEvents(t, d); kinds["child-started"] != 1 || kinds["child-restarted"] != 3 || kinds["child-crashed"] != 4 {
		t.Fatalf("child lifecycle events %v", kinds)
	}
	rows, err := d.store.Alerts(ctx, 0, 100, false)
	if err != nil {
		t.Fatal(err)
	}
	alerts := 0
	for _, row := range rows {
		if row.Source == "ui-child" {
			alerts++
		}
	}
	if alerts != 1 {
		t.Fatalf("ui-child alerts %d", alerts)
	}
	if _, err = call(t, d, "Status", map[string]any{}, ""); err != nil {
		t.Fatal("daemon stopped answering", err)
	}
}

// buildUIChild builds the separate fae-ui executable into dir: the core
// binary carries no UI pages (FAE-GEN-010 non-goal 6).
func buildUIChild(t *testing.T, dir string) string {
	t.Helper()
	_, file, _, _ := runtime.Caller(0)
	out := filepath.Join(dir, UIBinaryName)
	gobin, err := exec.LookPath("go")
	if err != nil {
		gobin = filepath.Join(runtime.GOROOT(), "bin", "go")
	}
	cmd := exec.Command(gobin, "build", "-o", out, "./cmd/fae-ui")
	cmd.Dir = filepath.Dir(filepath.Dir(file))
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fae-ui: %v\n%s", err, raw)
	}
	return out
}

// FAE-API-033, FAE-CLI-010, FAE-GEN-010: without --ui-bin the daemon runs
// fae-ui beside its own executable; a missing UI binary is a warning and the
// daemon starts. Disabled leaves no UI file or registry behind.
func TestUIMissingBinaryWarnsAndDisabledLeavesNoTrace(t *testing.T) {
	cfg, ctx := fixture(t)
	plain := cfg
	cfg.UI = &UIConfig{Port: freeLoopbackPort(t)}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	sibling := DefaultUIBinary(d.binaryPath)
	if s := uiState(d); s.Health != "missing-binary" || s.PID != nil || s.Binary != sibling || filepath.Base(sibling) != "fae-ui" {
		t.Fatalf("missing binary status %+v", s)
	}
	if _, err = call(t, d, "Status", map[string]any{}, ""); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	logRaw, _ := os.ReadFile(d.layout.Log)
	if !bytes.Contains(logRaw, []byte(sibling+" is missing")) {
		t.Fatal("missing binary not warned")
	}
	plain.Instance = "plain"
	d, err = Start(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	reply, err := call(t, d, "Status", map[string]any{}, "")
	if err != nil || !bytes.Contains(reply.Result, []byte(`"ui":{"enabled":false}`)) {
		t.Fatalf("disabled status %s %v", reply.Result, err)
	}
	if code := func() error { _, e := call(t, d, "UIToken", map[string]any{}, ""); return e }(); code == nil || !strings.Contains(code.Error(), "not enabled") {
		t.Fatal("UI token without UI", code)
	}
	for _, name := range []string{"ui", "ui-processes"} {
		if _, err = os.Stat(filepath.Join(d.layout.Run, name)); !os.IsNotExist(err) {
			t.Fatalf("disabled UI created %s: %v", name, err)
		}
	}
}

func jsonInt(v int64) string { raw, _ := json.Marshal(v); return string(raw) }
