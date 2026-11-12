package daemon

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/ui"
)

// UIConfig configures the optional supervised UI child (FAE-API-033). A nil
// Config.UI leaves the instance byte-identical to one without the capability.
type UIConfig struct {
	Binary      string // absolute; empty runs DefaultUIBinary beside this executable
	Dist        string // absolute assets directory; empty serves the child's own assets
	Bind        string // literal loopback address; empty is 127.0.0.1
	Port        int    // 0 derives a stable per-instance port
	AllowWrites bool
	RPCLog      string // absolute; empty disables the child's RPC log
}

// UIBinaryName is the reference UI executable (cmd/fae-ui). It ships beside
// the fae binary and never inside it (FAE-GEN-010 non-goal 6); the daemon only
// supervises a UI child through the generic contract (FAE-API-033).
const UIBinaryName = "fae-ui"

// DefaultUIBinary is the UI child used when none is configured: fae-ui in the
// directory of the given fae executable.
func DefaultUIBinary(executable string) string {
	return filepath.Join(filepath.Dir(executable), UIBinaryName)
}

// Appendix A: restart backoff 1 s doubling to 60 s, at most 10 restarts per
// hour, then the child is left stopped with an alert.
var (
	uiBackoffBase   = time.Second
	uiBackoffMax    = time.Minute
	uiRestartWindow = time.Hour
	uiRestartLimit  = 10
	uiStopWait      = 5 * time.Second
	uiProbeInterval = 30 * time.Second
	uiReadyWait     = 30 * time.Second
)

// UIStatus is the status row of the UI child (FAE-CLI-012, FAE-OBS-002). It
// never carries the token.
type UIStatus struct {
	Enabled       bool       `json:"enabled"`
	URL           string     `json:"url"`
	PID           *int       `json:"pid"`
	Health        string     `json:"health"`
	Restarts      int        `json:"restarts"`
	WritesEnabled bool       `json:"writes_enabled"`
	Binary        string     `json:"binary"`
	LastExit      string     `json:"last_exit,omitempty"`
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
}

type uiChild struct {
	cfg                  UIConfig
	url, dir, registry   string
	binary               string
	token                string
	read, write          []byte
	state                atomic.Pointer[UIStatus]
	stop, done           chan struct{}
	stopOnce, launchOnce sync.Once
	started              atomic.Bool
}

// normalizeUI validates the UI configuration before anything is created
// (FAE-API-030): a non-loopback bind is refused naming the value.
func normalizeUI(c *UIConfig, l Layout, doltPort int) (*UIConfig, error) {
	if c == nil {
		return nil, nil
	}
	u := *c
	if u.Bind == "" {
		u.Bind = ui.DefaultBind
	}
	bind, err := ui.ValidateBind(u.Bind)
	if err != nil {
		return nil, err
	}
	u.Bind = bind
	if u.Port, err = ui.Port(l.Root, u.Port); err != nil {
		return nil, err
	}
	if u.Port == doltPort {
		return nil, fmt.Errorf("UI port %d equals the Dolt port", u.Port)
	}
	for name, p := range map[string]string{"UI binary": u.Binary, "UI assets directory": u.Dist, "UI RPC log": u.RPCLog} {
		if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) != p) {
			return nil, fmt.Errorf("%s must be a clean absolute path: %q", name, p)
		}
	}
	return &u, nil
}

// setupUI recovers orphaned UI children and, when enabled, issues the UI's
// delegated RPC credentials before the socket listens. A UI failure is logged
// and leaves the UI unavailable; it never blocks the daemon (FAE-API-033).
func (d *Daemon) setupUI(ctx context.Context) {
	registry := filepath.Join(d.layout.Run, "ui-processes")
	dir := filepath.Join(d.layout.Run, "ui")
	recovered, cleared, recoverErr := processowner.RecoverClearing(ctx, registry, dir)
	d.logClearedProcessRecords("ui", cleared)
	if auditErr := d.auditClearedProcessRecords(ctx, cleared); auditErr != nil {
		d.logLine("ui", "warning: empty UI process record removal not audited: %v\n", auditErr)
	}
	if recoverErr != nil {
		d.logLine("ui", "warning: orphaned UI child recovery refused: %v\n", recoverErr)
		if d.config.UI != nil {
			d.ui = &uiChild{}
			d.ui.publish(UIStatus{Enabled: true, Health: "unavailable", LastExit: "orphan recovery refused: " + recoverErr.Error()})
		}
		return
	} else if len(recovered) > 0 {
		d.logLine("ui", "recovered %d orphaned UI child record(s)\n", len(recovered))
	}
	if d.config.UI == nil {
		return
	}
	u := &uiChild{cfg: *d.config.UI, url: ui.URL(d.config.UI.Bind, d.config.UI.Port), dir: dir, registry: registry, binary: d.config.UI.Binary, stop: make(chan struct{}), done: make(chan struct{})}
	if u.binary == "" {
		u.binary = DefaultUIBinary(d.binaryPath)
	}
	d.ui = u
	fail := func(err error) {
		d.logLine("ui", "warning: UI child unavailable: %v\n", err)
		u.publish(UIStatus{Enabled: true, URL: u.url, Health: "unavailable", WritesEnabled: u.cfg.AllowWrites, Binary: u.binary, LastExit: err.Error()})
	}
	if err := mkdirPrivate(dir); err != nil {
		fail(err)
		return
	}
	var err error
	if u.token, err = ui.NewToken(); err != nil {
		fail(err)
		return
	}
	u.read = make([]byte, 32)
	if _, err = rand.Read(u.read); err == nil {
		err = d.rpc.AddPrincipal(u.read, api.Principal{Actor: "ui"})
	}
	if err == nil && u.cfg.AllowWrites {
		// FAE-API-032: the write capability is a distinct credential, issued only
		// with the explicit flag; the daemon checks it on every mutating RPC.
		u.write = make([]byte, 32)
		if _, err = rand.Read(u.write); err == nil {
			err = d.rpc.AddPrincipal(u.write, api.Principal{Actor: "ui-writer"})
		}
	}
	if err != nil {
		u.token = ""
		fail(err)
		return
	}
	u.publish(UIStatus{Enabled: true, URL: u.url, Health: "stopped", WritesEnabled: u.cfg.AllowWrites, Binary: u.binary})
}

// startUI launches the supervisor once the daemon is ready. A missing binary
// is a warning; the daemon still starts.
func (d *Daemon) startUI() {
	u := d.ui
	if u == nil || u.token == "" {
		return
	}
	if info, err := os.Stat(u.binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		d.logLine("ui", "warning: UI child binary %s is missing or not executable; the daemon continues without the UI\n", u.binary)
		u.publish(UIStatus{Enabled: true, URL: u.url, Health: "missing-binary", WritesEnabled: u.cfg.AllowWrites, Binary: u.binary})
		return
	}
	u.launchOnce.Do(func() {
		u.started.Store(true)
		go u.supervise(d)
	})
}

// stopUI stops the child within the shutdown bound and removes any unconsumed
// launch secrets.
func (d *Daemon) stopUI(ctx context.Context) {
	u := d.ui
	if u == nil || u.stop == nil {
		return
	}
	u.stopOnce.Do(func() { close(u.stop) })
	if u.started.Load() {
		select {
		case <-u.done:
		case <-ctx.Done():
			d.logLine("ui", "warning: UI child stop exceeded the shutdown bound\n")
		}
	}
	u.removeSecrets()
	if s := u.state.Load(); s != nil && s.Health != "missing-binary" && s.Health != "unavailable" {
		next := *s
		next.PID, next.Health = nil, "stopped"
		u.publish(next)
	}
}

func (u *uiChild) publish(s UIStatus) { u.state.Store(&s) }

func (u *uiChild) update(f func(*UIStatus)) {
	next := UIStatus{}
	if s := u.state.Load(); s != nil {
		next = *s
	}
	f(&next)
	u.publish(next)
}

func (u *uiChild) stopping(d *Daemon) bool {
	select {
	case <-u.stop:
		return true
	default:
		return d.ctx.Err() != nil
	}
}

// supervise runs the child and restarts it on crash with bounded exponential
// backoff; once the hourly bound is spent it leaves the child stopped and
// raises one alert (FAE-API-033, FAE-OBS-005).
func (u *uiChild) supervise(d *Daemon) {
	defer close(u.done)
	backoff := uiBackoffBase
	var recent []time.Time
	for launches := 0; ; launches++ {
		if launches > 0 {
			now := time.Now()
			kept := recent[:0]
			for _, at := range recent {
				if now.Sub(at) < uiRestartWindow {
					kept = append(kept, at)
				}
			}
			recent = kept
			stopping := u.stopping(d)
			if !MayRestartUIChild(stopping, len(recent), uiRestartLimit) {
				if !stopping {
					u.exhausted(d, len(recent))
				}
				return
			}
			u.update(func(s *UIStatus) { s.Health, s.PID = "restarting", nil })
			timer := time.NewTimer(backoff)
			select {
			case <-u.stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			backoff = min(backoff*2, uiBackoffMax)
			recent = append(recent, time.Now())
			u.update(func(s *UIStatus) { s.Restarts++ })
		}
		began := time.Now()
		cmd, record, err := u.launch(d)
		if err != nil {
			d.logLine("ui", "UI child launch failed: %v\n", err)
			u.update(func(s *UIStatus) { s.Health, s.PID, s.LastExit = "restarting", nil, "launch failed: "+err.Error() })
			u.removeSecrets()
			continue
		}
		pid := cmd.Process.Pid
		phase := "started"
		if launches > 0 {
			phase = "restarted"
		}
		u.update(func(s *UIStatus) { s.Health, s.PID = "starting", &pid })
		d.logLine("ui", "UI child %s pid=%d url=%s writes=%t\n", phase, pid, u.url, u.cfg.AllowWrites)
		d.recordUILifecycle(phase, map[string]any{"pid": pid, "url": u.url, "restarts": u.state.Load().Restarts})
		exit, stopped := u.watch(cmd, record)
		u.removeSecrets()
		if stopped {
			d.logLine("ui", "UI child pid=%d stopped\n", pid)
			return
		}
		// A run that outlived the longest backoff restarts the doubling.
		if time.Since(began) >= uiBackoffMax {
			backoff = uiBackoffBase
		}
		u.update(func(s *UIStatus) { s.Health, s.PID, s.LastExit = "restarting", nil, exit })
		d.logLine("ui", "UI child pid=%d exited unexpectedly: %s\n", pid, exit)
		d.recordUILifecycle("crashed", map[string]any{"pid": pid, "exit": exit})
	}
}

func (u *uiChild) exhausted(d *Daemon, restarts int) {
	last := ""
	u.update(func(s *UIStatus) { s.Health, s.PID, last = "exhausted", nil, s.LastExit })
	detail, _ := json.Marshal(map[string]any{"reason": "UI child restart exhaustion; left stopped", "restarts": restarts, "limit": uiRestartLimit, "window_seconds": uiRestartWindow.Seconds(), "last_exit": last})
	d.logLine("ui", "UI child restarts exhausted (%d within %s); left stopped\n", restarts, uiRestartWindow)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := d.raiseAlert(ctx, controlplane.Alert{Severity: "error", Source: "ui-child", Detail: detail}, false); err != nil {
		d.logLine("ui", "UI exhaustion alert not recorded: %v\n", err)
	}
}

// recordUILifecycle writes the FAE-API-020 child lifecycle event.
func (d *Daemon) recordUILifecycle(phase string, payload map[string]any) {
	if d.store == nil {
		return
	}
	payload["child"] = "ui"
	raw, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.store.RecordChildLifecycle(ctx, phase, raw); err != nil {
		d.logLine("ui", "UI child %s event not recorded: %v\n", phase, err)
		return
	}
	d.notifyEvents()
}

// launch writes fresh launch secrets, which the child consumes before it
// listens, and starts it under the owned-process registry with a parent-death
// signal. Only paths travel on argv; the environment carries no secrets.
func (u *uiChild) launch(d *Daemon) (*exec.Cmd, *processowner.Record, error) {
	u.removeSecrets()
	secrets := map[string][]byte{ui.TokenFile: []byte(u.token), ui.CredentialFile: u.read}
	if u.write != nil {
		secrets[ui.WriteCredentialFile] = u.write
	}
	for name, b := range secrets {
		if err := ui.WriteSecret(filepath.Join(u.dir, name), b); err != nil {
			return nil, nil, err
		}
	}
	args := []string{"--bind", u.cfg.Bind, "--port", strconv.Itoa(u.cfg.Port), "--socket", d.layout.Socket, "--secrets-dir", u.dir, "--instance", d.config.Instance}
	if u.cfg.Dist != "" {
		args = append(args, "--dist", u.cfg.Dist)
	}
	if u.cfg.RPCLog != "" {
		args = append(args, "--rpc-log", u.cfg.RPCLog)
	}
	cmd := exec.Command(u.binary, args...)
	cmd.Dir = u.dir
	cmd.Env = uiEnvironment()
	secretsText := []string{u.token, hex.EncodeToString(u.read)}
	if u.write != nil {
		secretsText = append(secretsText, hex.EncodeToString(u.write))
	}
	out := newRedactWriter(componentStream(d.log, "ui"), secretsText)
	cmd.Stdout, cmd.Stderr = out, out
	ui.ConfigureParentDeath(cmd)
	// FAE-HOST-021: the inventory records the UI child under its role.
	record, err := processowner.StartRole(cmd, u.registry, u.dir, "", "ui")
	return cmd, record, err
}

// uiEnvironment passes only non-secret basics; provider and Fae variables stay
// with the daemon (FAE-SEC-003).
func uiEnvironment() []string {
	env := []string{}
	for _, key := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL", "TZ"} {
		if v, ok := os.LookupEnv(key); ok {
			env = append(env, key+"="+v)
		}
	}
	return env
}

// watch waits for the child to exit or for a stop request, probing health.
func (u *uiChild) watch(cmd *exec.Cmd, record *processowner.Record) (exit string, stopped bool) {
	exited := make(chan error, 1)
	go func() { exited <- processowner.Wait(cmd, record) }()
	describe := func(err error) string {
		if cmd.ProcessState != nil {
			return cmd.ProcessState.String()
		}
		if err != nil {
			return err.Error()
		}
		return "exited"
	}
	ready := time.Now().Add(uiReadyWait)
	healthy := false
	probe := time.NewTimer(50 * time.Millisecond)
	defer probe.Stop()
	for {
		select {
		case err := <-exited:
			return describe(err), false
		case <-u.stop:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			grace := time.NewTimer(uiStopWait)
			defer grace.Stop()
			select {
			case err := <-exited:
				return describe(err), true
			case <-grace.C:
				_ = cmd.Process.Kill()
				return describe(<-exited), true
			}
		case <-probe.C:
			ok := u.probe()
			now := time.Now().UTC()
			healthy = healthy || ok
			u.update(func(s *UIStatus) {
				s.CheckedAt = &now
				switch {
				case ok:
					s.Health = "healthy"
				case healthy || now.After(ready):
					s.Health = "unhealthy"
				}
			})
			next := uiProbeInterval
			if !healthy && now.Before(ready) {
				next = 100 * time.Millisecond
			}
			probe.Reset(next)
		}
	}
}

var uiProbeClient = &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}}

// probe asks the child's token-gated health endpoint over loopback; the
// daemon only dials, it never listens on TCP (FAE-ARC-006).
func (u *uiChild) probe() bool {
	req, err := http.NewRequest(http.MethodGet, u.url+"healthz", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	res, err := uiProbeClient.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	return res.StatusCode == http.StatusOK
}

func (u *uiChild) removeSecrets() {
	if u.dir == "" {
		return
	}
	for _, name := range []string{ui.TokenFile, ui.CredentialFile, ui.WriteCredentialFile} {
		_ = os.Remove(filepath.Join(u.dir, name))
	}
}

// uiStatus is the status "ui" field; disabled is exactly {"enabled": false}.
func (d *Daemon) uiStatus() any {
	if d.ui == nil {
		return map[string]any{"enabled": false}
	}
	if s := d.ui.state.Load(); s != nil {
		return *s
	}
	return map[string]any{"enabled": true}
}

// UIURL names the UI for the serve start line; empty when disabled.
func (d *Daemon) UIURL() string {
	if d.ui == nil {
		return ""
	}
	return d.ui.url
}

// uiTokenRPC hands the token to the instance operator only (FAE-API-031).
func (d *Daemon) uiTokenRPC(ctx context.Context, r api.Request, principal api.Principal) (any, error) {
	if _, ok := api.AuthenticatedPrincipal(ctx); !ok || principal.Actor != "operator" || principal.Task != "" {
		return nil, errors.New("the UI token is available only to the instance operator")
	}
	if err := decode(r.Params, &struct{}{}); err != nil {
		return nil, err
	}
	if d.ui == nil || d.ui.token == "" {
		return nil, &api.RPCError{Code: "ui-disabled", Message: "the UI child is not enabled; start serve with --ui"}
	}
	return map[string]any{"token": d.ui.token, "url": d.ui.url, "login_url": d.ui.url + "?token=" + d.ui.token, "writes_enabled": d.ui.cfg.AllowWrites}, nil
}

// redactWriter line-buffers child output and replaces every secret before the
// supervision log sees it (FAE-API-031, FAE-SEC-003).
type redactWriter struct {
	mu      sync.Mutex
	out     io.Writer
	secrets secretValues
	partial []byte
}

func newRedactWriter(out io.Writer, secrets []string) *redactWriter {
	values := make([][]byte, 0, len(secrets))
	for _, s := range secrets {
		values = append(values, []byte(s))
	}
	return &redactWriter{out: out, secrets: newSecretValues(values...)}
}

func (w *redactWriter) redact(b []byte) []byte { return w.secrets.redact(b) }

func (w *redactWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.partial = append(w.partial, p...)
	if i := bytes.LastIndexByte(w.partial, '\n'); i >= 0 {
		_, _ = w.out.Write(w.redact(append([]byte(nil), w.partial[:i+1]...)))
		w.partial = append(w.partial[:0], w.partial[i+1:]...)
	}
	// An unterminated flood is emitted redacted, keeping a tail long enough to
	// hold any secret prefix, in any encoded form, that the next write may
	// complete.
	if len(w.partial) > 64<<10 {
		w.partial = w.redact(w.partial)
		if cut := len(w.partial) - w.secrets.hold(); cut > 0 {
			_, _ = w.out.Write(append([]byte(nil), w.partial[:cut]...))
			w.partial = append(w.partial[:0], w.partial[cut:]...)
		}
	}
	return len(p), nil
}
