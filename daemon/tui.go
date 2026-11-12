package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/ui"
)

// TUIConfig configures the supervised terminal UI child (fae-tui). A nil
// Config.TUI disables daemon-supervised TUI execution.
type TUIConfig struct {
	Binary      string        // absolute; empty runs DefaultTUIBinary beside this executable
	AllowWrites bool          // passes --write to fae-tui
	Refresh     time.Duration // base polling interval (250ms..10m); 0 uses 2s default
	TTY         string        // optional clean absolute terminal path for interactive attachment
}

// TUIBinaryName is the terminal UI executable (cmd/fae-tui). It ships beside
// the fae binary and never inside it (FAE-GEN-010 non-goal 6).
const TUIBinaryName = "fae-tui"

// DefaultTUIBinary returns the default TUI child path: fae-tui in the
// directory of the given fae executable.
func DefaultTUIBinary(executable string) string {
	return filepath.Join(filepath.Dir(executable), TUIBinaryName)
}

// TUIStatus is the status row of the supervised TUI child.
type TUIStatus struct {
	Enabled       bool       `json:"enabled"`
	PID           *int       `json:"pid"`
	Health        string     `json:"health"`
	Restarts      int        `json:"restarts"`
	WritesEnabled bool       `json:"writes_enabled"`
	Binary        string     `json:"binary"`
	TTY           string     `json:"tty,omitempty"`
	HerdrPane     string     `json:"herdr_pane,omitempty"`
	LastExit      string     `json:"last_exit,omitempty"`
	CheckedAt     *time.Time `json:"checked_at,omitempty"`
}

type herdrPaneMarker struct {
	WorkspaceID string `json:"workspace_id,omitempty"`
	PaneID      string `json:"pane_id"`
	TTY         string `json:"tty,omitempty"`
	ShellPID    int    `json:"shell_pid,omitempty"`
}

type tuiChild struct {
	cfg                  TUIConfig
	dir, registry        string
	binary               string
	herdrMarkerPath      string
	herdrWorkspace       string
	herdrPane            string
	herdrShellPID        int
	herdrTTY             string
	allowTestHerdrPane   bool
	state                atomic.Pointer[TUIStatus]
	stop, done           chan struct{}
	stopOnce, launchOnce sync.Once
	started              atomic.Bool
}

// normalizeTUI validates the TUI configuration before the instance root or lock
// is created.
func normalizeTUI(c *TUIConfig) (*TUIConfig, error) {
	if c == nil {
		return nil, nil
	}
	t := *c
	for name, p := range map[string]string{"TUI binary": t.Binary, "TUI tty": t.TTY} {
		if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) != p) {
			return nil, fmt.Errorf("%s must be a clean absolute path: %q", name, p)
		}
	}
	if t.Refresh != 0 && (t.Refresh < 250*time.Millisecond || t.Refresh > 10*time.Minute) {
		return nil, fmt.Errorf("TUI refresh %s must be 250ms..10m", t.Refresh)
	}
	return &t, nil
}

// setupTUI recovers orphaned TUI children and initializes the TUI supervisor
// state when Config.TUI is enabled. A TUI failure is logged and never blocks
// the daemon.
func (d *Daemon) setupTUI(ctx context.Context) {
	registry := filepath.Join(d.layout.Run, "tui-processes")
	dir := filepath.Join(d.layout.Run, "tui")
	recovered, cleared, recoverErr := processowner.RecoverClearing(ctx, registry, dir)
	d.logClearedProcessRecords("tui", cleared)
	if auditErr := d.auditClearedProcessRecords(ctx, cleared); auditErr != nil {
		d.logLine("tui", "warning: empty TUI process record removal not audited: %v\n", auditErr)
	}
	if recoverErr != nil {
		d.logLine("tui", "warning: orphaned TUI child recovery refused: %v\n", recoverErr)
		if d.config.TUI != nil {
			d.tui = &tuiChild{}
			d.tui.publish(TUIStatus{Enabled: true, Health: "unavailable", LastExit: "orphan recovery refused: " + recoverErr.Error()})
		}
		return
	} else if len(recovered) > 0 {
		d.logLine("tui", "recovered %d orphaned TUI child record(s)\n", len(recovered))
	}
	if d.config.TUI == nil {
		return
	}
	t := &tuiChild{
		cfg:             *d.config.TUI,
		dir:             dir,
		registry:        registry,
		binary:          d.config.TUI.Binary,
		herdrMarkerPath: filepath.Join(d.layout.Root, "tui-herdr-pane.json"),
		stop:            make(chan struct{}),
		done:            make(chan struct{}),
	}
	if t.binary == "" {
		t.binary = DefaultTUIBinary(d.binaryPath)
	}
	d.tui = t
	if err := mkdirPrivate(dir); err != nil {
		d.logLine("tui", "warning: TUI child unavailable: %v\n", err)
		t.publish(TUIStatus{Enabled: true, Health: "unavailable", WritesEnabled: t.cfg.AllowWrites, Binary: t.binary, LastExit: err.Error()})
		return
	}
	t.publish(TUIStatus{Enabled: true, Health: "stopped", WritesEnabled: t.cfg.AllowWrites, Binary: t.binary})
}

// startTUI launches the TUI supervisor once the daemon is ready. A missing
// binary logs a warning and leaves the daemon running.
func (d *Daemon) startTUI() {
	t := d.tui
	if t == nil || t.stop == nil {
		return
	}
	if s := t.state.Load(); s != nil && s.Health == "unavailable" {
		return
	}
	if info, err := os.Stat(t.binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		d.logLine("tui", "warning: TUI child binary %s is missing or not executable; the daemon continues without the TUI\n", t.binary)
		t.publish(TUIStatus{Enabled: true, Health: "missing-binary", WritesEnabled: t.cfg.AllowWrites, Binary: t.binary})
		return
	}
	t.launchOnce.Do(func() {
		t.started.Store(true)
		go t.supervise(d)
	})
}

// stopTUI stops the supervised TUI child within the shutdown bound.
func (d *Daemon) stopTUI(ctx context.Context) {
	t := d.tui
	if t == nil || t.stop == nil {
		return
	}
	t.stopOnce.Do(func() { close(t.stop) })
	if t.started.Load() {
		select {
		case <-t.done:
		case <-ctx.Done():
			d.logLine("tui", "warning: TUI child stop exceeded the shutdown bound\n")
		}
	}
	t.releaseHerdrTTY()
	if s := t.state.Load(); s != nil && s.Health != "missing-binary" && s.Health != "unavailable" {
		next := *s
		next.PID, next.Health = nil, "stopped"
		t.publish(next)
	}
}

func (t *tuiChild) publish(s TUIStatus) { t.state.Store(&s) }

func (t *tuiChild) update(f func(*TUIStatus)) {
	next := TUIStatus{}
	if s := t.state.Load(); s != nil {
		next = *s
	}
	f(&next)
	t.publish(next)
}

func (t *tuiChild) stopping(d *Daemon) bool {
	select {
	case <-t.stop:
		return true
	default:
		return d.ctx.Err() != nil
	}
}

// supervise runs the TUI child and keeps it running for as long as the daemon
// is up, restarting it with bounded exponential backoff if it exits.
func (t *tuiChild) supervise(d *Daemon) {
	defer close(t.done)
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
			stopping := t.stopping(d)
			if !MayRestartUIChild(stopping, len(recent), uiRestartLimit) {
				if !stopping {
					t.exhausted(d, len(recent))
				}
				return
			}
			t.update(func(s *TUIStatus) { s.Health, s.PID = "restarting", nil })
			timer := time.NewTimer(backoff)
			select {
			case <-t.stop:
				timer.Stop()
				return
			case <-timer.C:
			}
			backoff = min(backoff*2, uiBackoffMax)
			recent = append(recent, time.Now())
			t.update(func(s *TUIStatus) { s.Restarts++ })
		}
		began := time.Now()
		cmd, record, hold, err := t.launch(d)
		if err != nil {
			d.logLine("tui", "TUI child launch failed: %v\n", err)
			t.update(func(s *TUIStatus) { s.Health, s.PID, s.LastExit = "restarting", nil, "launch failed: "+err.Error() })
			continue
		}
		pid := cmd.Process.Pid
		phase := "started"
		if launches > 0 {
			phase = "restarted"
		}
		t.update(func(s *TUIStatus) { s.Health, s.PID = "starting", &pid })
		d.logLine("tui", "TUI child %s pid=%d writes=%t\n", phase, pid, t.cfg.AllowWrites)
		d.recordTUILifecycle(phase, map[string]any{"pid": pid, "restarts": t.state.Load().Restarts})
		exit, stopped := t.watch(d, cmd, record, hold)
		if stopped {
			d.logLine("tui", "TUI child pid=%d stopped\n", pid)
			return
		}
		if time.Since(began) >= uiBackoffMax {
			backoff = uiBackoffBase
		}
		t.update(func(s *TUIStatus) { s.Health, s.PID, s.LastExit = "restarting", nil, exit })
		d.logLine("tui", "TUI child pid=%d exited unexpectedly: %s\n", pid, exit)
		d.recordTUILifecycle("crashed", map[string]any{"pid": pid, "exit": exit})
	}
}

func (t *tuiChild) exhausted(d *Daemon, restarts int) {
	last := ""
	t.update(func(s *TUIStatus) { s.Health, s.PID, last = "exhausted", nil, s.LastExit })
	detail, _ := json.Marshal(map[string]any{"reason": "TUI child restart exhaustion; left stopped", "restarts": restarts, "limit": uiRestartLimit, "window_seconds": uiRestartWindow.Seconds(), "last_exit": last})
	d.logLine("tui", "TUI child restarts exhausted (%d within %s); left stopped\n", restarts, uiRestartWindow)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, _, err := d.raiseAlert(ctx, controlplane.Alert{Severity: "error", Source: "tui-child", Detail: detail}, false); err != nil {
		d.logLine("tui", "TUI exhaustion alert not recorded: %v\n", err)
	}
}

func (d *Daemon) recordTUILifecycle(phase string, payload map[string]any) {
	if d.store == nil {
		return
	}
	payload["child"] = "tui"
	raw, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.store.RecordChildLifecycle(ctx, phase, raw); err != nil {
		d.logLine("tui", "TUI child %s event not recorded: %v\n", phase, err)
		return
	}
	d.notifyEvents()
}

func (t *tuiChild) launch(d *Daemon) (*exec.Cmd, *processowner.Record, io.Closer, error) {
	args := []string{"--home", d.config.Root, "--instance", d.config.Instance}
	if t.cfg.AllowWrites {
		args = append(args, "--write")
	}
	if t.cfg.Refresh > 0 {
		args = append(args, "--refresh", t.cfg.Refresh.String())
	}
	cmd := exec.Command(t.binary, args...)
	cmd.Dir = t.dir
	env := uiEnvironment()
	term := os.Getenv("TERM")
	if term == "" || term == "dumb" {
		term = "xterm-256color"
	}
	cmd.Env = append(env, "TERM="+term)
	errOut := newRedactWriter(componentStream(d.log, "tui"), nil)
	cmd.Stderr = errOut

	var parentClose io.Closer
	var childClose io.Closer
	ttyPath := t.cfg.TTY
	if ttyPath == "" {
		ttyPath = t.acquireHerdrTTY(d)
	}
	if ttyPath != "" {
		if tty, err := os.OpenFile(ttyPath, os.O_RDWR|syscall.O_NOCTTY, 0); err == nil {
			cmd.Stdin = tty
			cmd.Stdout = tty
			childClose = tty
			t.update(func(s *TUIStatus) {
				s.TTY = ttyPath
				s.HerdrPane = t.herdrPane
			})
		} else if t.cfg.TTY == "" {
			t.releaseHerdrTTY()
		}
	}
	if cmd.Stdin == nil {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, nil, nil, err
		}
		cmd.Stdin = r
		cmd.Stdout = io.Discard
		childClose = r
		parentClose = w
	}
	ui.ConfigureParentDeath(cmd)
	record, err := processowner.StartRole(cmd, t.registry, t.dir, "", "tui")
	if childClose != nil {
		_ = childClose.Close()
	}
	if err != nil {
		if parentClose != nil {
			_ = parentClose.Close()
		}
		return nil, nil, nil, err
	}
	return cmd, record, parentClose, nil
}

func (t *tuiChild) watch(d *Daemon, cmd *exec.Cmd, record *processowner.Record, hold io.Closer) (exit string, stopped bool) {
	defer func() {
		if hold != nil {
			_ = hold.Close()
		}
	}()
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
		case <-t.stop:
			if hold != nil {
				_ = hold.Close()
				hold = nil
			}
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
			ok := t.probe(d, cmd.Process.Pid)
			now := time.Now().UTC()
			healthy = healthy || ok
			t.update(func(s *TUIStatus) {
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

func (t *tuiChild) probe(d *Daemon, pid int) bool {
	if pid <= 0 || syscall.Kill(pid, 0) != nil {
		return false
	}
	_, err := os.Stat(d.layout.Socket)
	return err == nil
}

// tuiStatus is the status "tui" field; disabled is {"enabled": false}.
func (d *Daemon) tuiStatus() any {
	if d.tui == nil {
		return map[string]any{"enabled": false}
	}
	if s := d.tui.state.Load(); s != nil {
		return *s
	}
	return map[string]any{"enabled": true}
}

func findHerdrExecutable() string {
	if p, err := exec.LookPath("herdr-core"); err == nil && filepath.IsAbs(p) {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		cand := filepath.Join(home, ".local", "bin", "herdr-core")
		if info, err := os.Stat(cand); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return cand
		}
	}
	return ""
}

func herdrTUITerminalName(instance string) string {
	var b strings.Builder
	b.WriteString("fae-tui")
	var suffix strings.Builder
	for _, r := range strings.ToLower(instance) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			suffix.WriteRune(r)
		default:
			suffix.WriteByte('-')
		}
	}
	clean := strings.Trim(suffix.String(), "-_")
	if clean != "" {
		b.WriteByte('-')
		b.WriteString(clean)
	}
	name := b.String()
	if len(name) > 32 {
		name = strings.TrimRight(name[:32], "-_")
	}
	if name == "" {
		return "fae-tui"
	}
	return name
}

func closeHerdrTerminal(ctx context.Context, herdrBin, target string) {
	if herdrBin == "" || target == "" {
		return
	}
	_ = exec.CommandContext(ctx, herdrBin, "kill", target, "--json").Run()
	_ = exec.CommandContext(ctx, herdrBin, "rm", target, "--json").Run()
}

// reapStaleHerdrPane closes any orphaned herdr-core terminal recorded in
// tui-herdr-pane.json from a prior crashed daemon run without signaling any
// foreign shell PID (FAE-GEN-003).
func (t *tuiChild) reapStaleHerdrPane(root string) {
	markerPath := t.herdrMarkerPath
	if markerPath == "" && root != "" {
		markerPath = filepath.Join(root, "tui-herdr-pane.json")
	}
	if markerPath == "" {
		return
	}
	raw, err := os.ReadFile(markerPath)
	if err != nil {
		return
	}
	var m herdrPaneMarker
	if json.Unmarshal(raw, &m) == nil {
		if herdrBin := findHerdrExecutable(); herdrBin != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			target := m.PaneID
			if target == "" {
				target = m.WorkspaceID
			}
			closeHerdrTerminal(ctx, herdrBin, target)
			cancel()
		}
	}
	_ = os.Remove(markerPath)
}

// acquireHerdrTTY provisions or reuses a herdr-core terminal when the daemon is
// running outside go test and no explicit TTY was configured, returning its
// /dev/pts/* path so fae-tui renders directly inside the operator's herdr-core
// session. It never signals herdr-core's foreign shell PID (FAE-GEN-003).
func (t *tuiChild) acquireHerdrTTY(d *Daemon) string {
	if (strings.HasSuffix(os.Args[0], ".test") || os.Getenv("CLI_TEST_AS_MAIN") != "") && !t.allowTestHerdrPane && os.Getenv("FAE_TEST_ENABLE_HERDR_PANE") != "true" {
		return ""
	}
	if t.herdrTTY != "" && t.herdrShellPID > 0 {
		if target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", t.herdrShellPID)); err == nil && target == t.herdrTTY {
			return t.herdrTTY
		}
		t.herdrTTY, t.herdrPane, t.herdrWorkspace, t.herdrShellPID = "", "", "", 0
	}
	t.reapStaleHerdrPane(d.layout.Root)

	herdrBin := findHerdrExecutable()
	if herdrBin == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	cwd := d.config.Workspace
	if cwd == "" {
		cwd = d.layout.Root
	}
	termName := herdrTUITerminalName(d.config.Instance)
	runArgs := []string{
		"run", "--bg", "--no-parent", "--no-notify", "--keep",
		"--name", termName, "--cwd", cwd, "--json",
		"sh", "-c", "trap 'exit 0' INT TERM HUP; while :; do sleep 3600; done",
	}
	out, err := exec.CommandContext(ctx, herdrBin, runArgs...).Output()
	if err != nil {
		closeHerdrTerminal(ctx, herdrBin, termName)
		out, err = exec.CommandContext(ctx, herdrBin, runArgs...).Output()
		if err != nil {
			return ""
		}
	}
	var created struct {
		Address string `json:"address"`
	}
	if json.Unmarshal(out, &created) != nil || created.Address == "" {
		return ""
	}
	paneID := created.Address

	var shellPID int
	var pts string
	for attempt := 0; attempt < 10; attempt++ {
		out, err := exec.CommandContext(ctx, herdrBin, "get", paneID, "--json").Output()
		if err == nil {
			var info struct {
				PID int    `json:"pid"`
				TTY string `json:"tty"`
			}
			if json.Unmarshal(out, &info) == nil && (info.PID > 0 || info.TTY != "") {
				shellPID = info.PID
				pts = info.TTY
				if pts == "" && shellPID > 1 {
					for _, fd := range []int{0, 1, 2} {
						if link, linkErr := os.Readlink(fmt.Sprintf("/proc/%d/fd/%d", shellPID, fd)); linkErr == nil && strings.HasPrefix(link, "/dev/") {
							pts = link
							break
						}
					}
				}
				if pts != "" {
					break
				}
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if pts == "" || !filepath.IsAbs(pts) || !strings.HasPrefix(pts, "/dev/") {
		closeHerdrTerminal(ctx, herdrBin, paneID)
		return ""
	}
	cleanPTS := filepath.Clean(pts)
	f, err := os.OpenFile(cleanPTS, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		closeHerdrTerminal(ctx, herdrBin, paneID)
		return ""
	}
	_ = f.Close()

	markerPath := t.herdrMarkerPath
	if markerPath == "" && d.layout.Root != "" {
		markerPath = filepath.Join(d.layout.Root, "tui-herdr-pane.json")
		t.herdrMarkerPath = markerPath
	}
	if markerPath != "" {
		rawMarker, _ := json.Marshal(herdrPaneMarker{
			PaneID:   paneID,
			TTY:      cleanPTS,
			ShellPID: shellPID,
		})
		_ = writePrivate(markerPath, append(rawMarker, '\n'))
	}
	t.herdrWorkspace = ""
	t.herdrPane = paneID
	t.herdrShellPID = shellPID
	t.herdrTTY = cleanPTS
	return t.herdrTTY
}

func (t *tuiChild) releaseHerdrTTY() {
	target := t.herdrPane
	if target == "" {
		target = t.herdrWorkspace
	}
	if target == "" && t.herdrMarkerPath != "" {
		if raw, err := os.ReadFile(t.herdrMarkerPath); err == nil {
			var m herdrPaneMarker
			if json.Unmarshal(raw, &m) == nil {
				if m.PaneID != "" {
					target = m.PaneID
				} else {
					target = m.WorkspaceID
				}
			}
		}
	}
	if herdrBin := findHerdrExecutable(); herdrBin != "" && target != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		closeHerdrTerminal(ctx, herdrBin, target)
		cancel()
	}
	if t.herdrMarkerPath != "" {
		_ = os.Remove(t.herdrMarkerPath)
	}
	t.herdrWorkspace, t.herdrPane, t.herdrTTY, t.herdrShellPID = "", "", "", 0
}
