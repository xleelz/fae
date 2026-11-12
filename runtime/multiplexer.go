package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/processowner"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// PaneProcess describes the OS process identity of a multiplexer terminal.
type PaneProcess struct {
	PID            int    `json:"pid"`
	PGID           int    `json:"pgid"`
	SID            int    `json:"sid"`
	ForegroundPGID int    `json:"foreground_pgid"`
	TTY            string `json:"tty"`
}

func herdrTerminalName(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-_")
	if s == "" {
		s = "pane"
	}
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-_")
		if s == "" {
			s = "pane"
		}
	}
	return s
}

func writeHerdrCoreConfig(dir, socket, instance, wrapper string) (config string, extraEnv []string, err error) {
	cfgDir := filepath.Join(dir, "cfg")
	hcDir := filepath.Join(cfgDir, "herdr-core")
	runDir := filepath.Join(dir, "run")
	stateDir := filepath.Join(dir, "state")
	dataDir := filepath.Join(dir, "data")
	for _, p := range []string{hcDir, runDir, stateDir, dataDir} {
		if err := os.MkdirAll(p, 0700); err != nil {
			return "", nil, err
		}
	}
	config = filepath.Join(hcDir, "config.toml")
	content := "notes = false\nshell = [" + strconv.Quote(wrapper) + "]\n"
	if err := os.WriteFile(config, []byte(content), 0600); err != nil {
		return "", nil, err
	}
	_ = os.WriteFile(filepath.Join(dir, "herdr.toml"), []byte(content), 0600)
	extraEnv = []string{
		"HC_SOCKET=" + socket,
		"HC_SESSION=" + herdrTerminalName(instance),
		"XDG_CONFIG_HOME=" + cfgDir,
		"XDG_RUNTIME_DIR=" + runDir,
		"XDG_STATE_HOME=" + stateDir,
		"XDG_DATA_HOME=" + dataDir,
		"HERDR_CONFIG_PATH=" + config,
		"HERDR_SOCKET_PATH=" + socket,
	}
	return config, extraEnv, nil
}

func cleanMultiplexerEnv(base []string, extraEnv ...string) []string {
	env := make([]string, 0, len(base)+len(extraEnv))
	for _, entry := range base {
		if strings.HasPrefix(entry, "HERDR_") || strings.HasPrefix(entry, "HC_") ||
			strings.HasPrefix(entry, "XDG_CONFIG_HOME=") || strings.HasPrefix(entry, "XDG_RUNTIME_DIR=") ||
			strings.HasPrefix(entry, "XDG_STATE_HOME=") || strings.HasPrefix(entry, "XDG_DATA_HOME=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, extraEnv...)
}

func socketReady(socket string) bool {
	info, err := os.Lstat(socket)
	if err != nil {
		return false
	}
	if info.Mode()&os.ModeSocket == 0 {
		return true
	}
	c, err := net.DialTimeout("unix", socket, 50*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func herdrTerminalCount(raw []byte) (int, error) {
	var listing struct {
		Terminals *[]json.RawMessage `json:"terminals"`
		Result    *struct {
			Workspaces []json.RawMessage `json:"workspaces"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &listing); err != nil {
		return 0, err
	}
	if listing.Terminals != nil {
		return len(*listing.Terminals), nil
	}
	if listing.Result != nil && listing.Result.Workspaces != nil {
		return len(listing.Result.Workspaces), nil
	}
	return 0, errors.New("invalid herdr terminal listing")
}

func parseHerdrCreatedTerminal(raw []byte) (paneID, name string, err error) {
	var created struct {
		Address string `json:"address"`
		Name    string `json:"name"`
		Result  *struct {
			RootPane struct {
				ID string `json:"pane_id"`
			} `json:"root_pane"`
			Workspace struct {
				Label string `json:"label"`
			} `json:"workspace"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &created); err != nil {
		return "", "", err
	}
	if created.Address != "" {
		return created.Address, created.Name, nil
	}
	if created.Result != nil && created.Result.RootPane.ID != "" {
		return created.Result.RootPane.ID, created.Result.Workspace.Label, nil
	}
	return "", "", errors.New("invalid herdr workspace identity")
}

func parseHerdrProcessInfo(raw []byte, paneID string) (PaneProcess, error) {
	var info struct {
		PID            int    `json:"pid"`
		PGID           int    `json:"pgid"`
		SID            int    `json:"sid"`
		ForegroundPGID int    `json:"foreground_pgid"`
		TTY            string `json:"tty"`
		State          string `json:"state"`
		Result         *struct {
			Info struct {
				PID  int `json:"shell_pid"`
				PGID int `json:"foreground_process_group_id"`
			} `json:"process_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return PaneProcess{}, err
	}
	if info.PID == 0 && info.Result != nil {
		info.PID = info.Result.Info.PID
		info.PGID = info.Result.Info.PGID
	}
	if info.State != "" && info.State != "live" {
		return PaneProcess{}, fmt.Errorf("multiplexer pane process info: terminal %s state is %s", paneID, info.State)
	}
	if info.PID <= 1 {
		return PaneProcess{}, errors.New("invalid herdr pane process identity")
	}
	if info.PGID == 0 {
		if pgid, err := syscall.Getpgid(info.PID); err == nil && pgid > 1 {
			info.PGID = pgid
		} else {
			info.PGID = info.PID
		}
	}
	if info.SID == 0 {
		if procs, err := processowner.CensusOf([]int{info.PID}); err == nil && len(procs) == 1 && procs[0].SID > 1 {
			info.SID = procs[0].SID
		} else {
			info.SID = info.PGID
		}
	}
	if info.ForegroundPGID == 0 {
		info.ForegroundPGID = info.PGID
	}
	if info.TTY == "" {
		if link, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/0", info.PID)); err == nil {
			info.TTY = link
		}
	}
	return PaneProcess{
		PID:            info.PID,
		PGID:           info.PGID,
		SID:            info.SID,
		ForegroundPGID: info.ForegroundPGID,
		TTY:            info.TTY,
	}, nil
}

func registerHerdrHolder(panePID, serverPID int, dir string) *processowner.Record {
	procs, err := processowner.CensusOf([]int{panePID})
	if err != nil || len(procs) != 1 {
		return nil
	}
	holdPID := procs[0].PPID
	if holdPID <= 1 || holdPID == serverPID || holdPID == panePID || holdPID == os.Getpid() {
		return nil
	}
	h, err := processowner.Register(holdPID, filepath.Join(dir, ".holder"), dir)
	if err != nil {
		return nil
	}
	if err := processowner.Watch(h); err != nil {
		_ = processowner.Retire(h)
		return nil
	}
	return h
}

// Multiplexer runs the same strict bridge contract as Direct in an isolated herdr
// server. Each turn has a private socket and an instance-named session. Existing
// servers are never adopted, and ambient HERDR_* are never inherited.
type Multiplexer struct {
	Direct      *Direct
	Binary      string
	Instance    string
	Labels      map[string]string
	BeforeStart func(context.Context, string, string) error
	once        sync.Once
}

func buildHerdrLabels(instance, defaultRole string, launch *Launch, custom map[string]string) []string {
	merged := map[string]string{
		"fae.instance": instance,
		"fae.role":     defaultRole,
		"herdr.space":  instance,
	}
	if launch != nil {
		if launch.TaskID != "" {
			merged["fae.task"] = launch.TaskID
		}
		if launch.Stage != "" {
			merged["fae.stage"] = launch.Stage
		}
		if launch.Binding.Provider != "" || launch.Binding.Model != "" {
			merged["fae.binding"] = strings.Trim(launch.Binding.Provider+":"+launch.Binding.Model, ":")
		}
	}
	for k, v := range custom {
		if k == "" || v == "" {
			continue
		}
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k, v := range merged {
		if k != "" && v != "" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

func applyHerdrLabels(ctx context.Context, binary string, env []string, paneID string, labels []string) {
	if binary == "" || paneID == "" || len(labels) == 0 {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	args := make([]string, 0, len(labels)+3)
	args = append(args, "label", paneID)
	args = append(args, labels...)
	args = append(args, "--json")
	c := exec.CommandContext(cctx, binary, args...)
	c.Env = env
	_ = c.Run()
}

func (m *Multiplexer) Run(ctx context.Context, launch Launch) (Collected, error) {
	if m.Direct == nil {
		return Collected{}, errors.New("direct bridge configuration required")
	}
	m.once.Do(func() { m.Direct.prepareProcess = m.prepare })
	return m.Direct.Run(ctx, launch)
}
func (m *Multiplexer) Sessions() map[string]int {
	if m.Direct == nil {
		return nil
	}
	return m.Direct.Sessions()
}
func (m *Multiplexer) Live() []LiveSession {
	if m.Direct == nil {
		return nil
	}
	return m.Direct.Live()
}
func (m *Multiplexer) Retire(ctx context.Context, session string) error {
	if m.Direct == nil {
		return ErrUnknownSession
	}
	return m.Direct.Retire(ctx, session)
}

// muxPaneExitStatus is the bridge's nonzero exit status as the pane wrapper
// recorded it. Like *exec.ExitError it reports ExitCode, so Direct reads a
// typed bridge refusal the same way under Herdr.
type muxPaneExitStatus int

func (s muxPaneExitStatus) Error() string { return fmt.Sprintf("herdr pane exit status %d", int(s)) }
func (s muxPaneExitStatus) ExitCode() int { return int(s) }

// muxBeforeStartDeferred is a one-shot pane's failed BeforeStart (its pane
// ledger bind): a typed admission deferral, as the executor reads it.
type muxBeforeStartDeferred struct{ cause error }

func (e muxBeforeStartDeferred) Error() string         { return e.cause.Error() }
func (e muxBeforeStartDeferred) Unwrap() error         { return e.cause }
func (muxBeforeStartDeferred) AdmissionDeferred() bool { return true }

// quote is POSIX shell quoting for generated, private herdr configuration only.
// Provider arguments remain literal, including quotes, dollars and newlines.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func (m *Multiplexer) prepare(ctx context.Context, cmd *exec.Cmd, dir string) (_ *processHooks, err error) {
	if !filepath.IsAbs(m.Binary) {
		return nil, errors.New("herdr executable must be absolute")
	}
	if m.Instance == "" || strings.ContainsAny(m.Instance, ".:\x00\n\r") {
		return nil, errors.New("invalid herdr instance name")
	}
	socket := filepath.Join(dir, "mux.sock")
	if len(socket) > 100 {
		return nil, errors.New("herdr socket path exceeds platform bound; use a shorter runtime root")
	}
	if _, e := os.Lstat(socket); !os.IsNotExist(e) {
		return nil, errors.New("refusing existing herdr workspace")
	}
	fifo := filepath.Join(dir, "output.pipe")
	gate := filepath.Join(dir, "start.pipe")
	for _, p := range []string{fifo, gate} {
		if e := syscall.Mkfifo(p, 0600); e != nil {
			return nil, e
		}
	}
	output, e := syscall.Open(fifo, syscall.O_RDWR|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return nil, e
	}
	defer func() {
		if err != nil {
			syscall.Close(output)
		}
	}()
	gateFile, e := os.OpenFile(gate, os.O_RDWR|syscall.O_NONBLOCK, 0600)
	if e != nil {
		return nil, e
	}
	defer func() {
		if err != nil {
			gateFile.Close()
		}
	}()
	rawConfig, e := os.ReadFile(filepath.Join(dir, "config.json"))
	if e != nil {
		return nil, e
	}
	var bridgeConfig BridgeConfig
	if e = json.Unmarshal(rawConfig, &bridgeConfig); e != nil || bridgeConfig.SessionID == "" {
		return nil, errors.New("owned bridge session identity required")
	}
	if m.BeforeStart != nil {
		if e := m.BeforeStart(ctx, bridgeConfig.SessionID, socket); e != nil {
			// FAE-RT-004: binding the pane to its ledger reservation is local
			// admission. No server or agent has started and no provider is
			// involved, so the turn defers as infrastructure: never a startup
			// retry, whose exhaustion is charged to the provider binding.
			return nil, muxBeforeStartDeferred{cause: e}
		}
	}
	status := filepath.Join(dir, "exit.status")
	args := make([]string, len(cmd.Args))
	for i, a := range cmd.Args {
		args[i] = quote(a)
	}
	wrapper := filepath.Join(dir, "pane.sh")
	// read blocks before any provider code; the owner records the pane before
	// releasing this gate. Explicit redirects make terminal output diagnostic only.
	// OSC 7501 sequences are emitted to the PTY (stdout of pane.sh or /dev/tty)
	// and never touch fifo (which captures the bridge's stdout/stderr).
	script := "#!/bin/sh\n" +
		"osc7501() {\n" +
		"  if [ -t 1 ]; then\n" +
		"    printf '\\033]7501;state=%s;app=fae\\033\\\\' \"$1\" 2>/dev/null || true\n" +
		"  elif [ -c /dev/tty ]; then\n" +
		"    { printf '\\033]7501;state=%s;app=fae\\033\\\\' \"$1\"; } >/dev/tty 2>/dev/null || true\n" +
		"  fi\n" +
		"}\n" +
		"IFS= read -r permit < " + quote(gate) + " || exit 125\n" +
		"[ \"$permit\" = start ] || exit 125\n" +
		"osc7501 working\n" +
		strings.Join(args, " ") + " < " + quote(filepath.Join(dir, "brief.json")) + " > " + quote(fifo) + " 2>&1\n" +
		"result=$?\n" +
		"if [ \"$result\" -eq 0 ]; then\n" +
		"  osc7501 done\n" +
		"else\n" +
		"  osc7501 blocked\n" +
		"fi\n" +
		"printf '%s\\n' \"$result\" > " + quote(status+".tmp") + "\n" +
		"mv -- " + quote(status+".tmp") + " " + quote(status) + "\n" +
		"kill -STOP $$\n" +
		"exit \"$result\"\n"
	if e = os.WriteFile(wrapper, []byte(script), 0700); e != nil {
		return nil, e
	}
	_, extraEnv, e := writeHerdrCoreConfig(dir, socket, m.Instance, wrapper)
	if e != nil {
		return nil, e
	}
	cmd.Env = cleanMultiplexerEnv(cmd.Env, extraEnv...)
	cmd.Path = m.Binary
	cmd.Args = []string{m.Binary, "serve"}
	done := make(chan struct{})
	stopOutput := make(chan struct{})
	writer := cmd.Stdout
	go func() {
		defer close(done)
		buffer := make([]byte, 8192)
		stopping := false
		flushed := 0
		for {
			select {
			case <-stopOutput:
				stopping = true
			default:
			}
			if stopping && flushed >= 1<<20 {
				return
			}
			n, e := syscall.Read(output, buffer)
			if n > 0 {
				if stopping {
					flushed += n
				}
				if _, e := writer.Write(buffer[:n]); e != nil {
					return
				}
				continue
			}
			if e != nil && e != syscall.EAGAIN && e != syscall.EINTR {
				return
			}
			select {
			case <-stopOutput:
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	var outputOnce sync.Once
	closeStreams := func() {
		outputOnce.Do(func() { _ = gateFile.Close(); close(stopOutput); <-done; _ = syscall.Close(output) })
	}
	query := func(args ...string) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		c := exec.CommandContext(cctx, m.Binary, args...)
		c.Env = cmd.Env
		return c.Output()
	}
	stopServer := func() error {
		if !socketReady(socket) {
			return nil
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c := exec.CommandContext(stopCtx, m.Binary, "server", "stop")
		c.Env = cmd.Env
		if e := c.Run(); e != nil {
			return fmt.Errorf("herdr server stop: %w", e)
		}
		return nil
	}
	panePID := 0
	var pane *processowner.Record
	var holder *processowner.Record
	var paneMu sync.Mutex
	stopPane := func() error {
		paneMu.Lock()
		defer paneMu.Unlock()
		var paneErr error
		if pane != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			if e := processowner.Terminate(stopCtx, pane, m.Direct.Root); e != nil {
				paneErr = fmt.Errorf("herdr pane %d retirement: %w", panePID, e)
			}
			cancel()
		}
		if holder != nil {
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			_ = processowner.Terminate(stopCtx, holder, dir)
			_ = processowner.Retire(holder)
			cancel()
		}
		return paneErr
	}
	originalCancel := cmd.Cancel
	cmd.Cancel = func() error { return errors.Join(stopPane(), stopServer(), originalCancel()) }
	monitorDone := make(chan struct{})
	monitorCtx, cancelMonitor := context.WithCancel(ctx)
	monitorStarted := false
	providerReleased := false
	var monitorError error
	return &processHooks{
		ActivityPID:  func() int { return panePID },
		NonExecution: func() bool { return !providerReleased },
		Started: func() error {
			timer := time.NewTimer(startupLimit(m.Direct.StartTimeout))
			defer timer.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				if socketReady(socket) {
					raw, err := query("ls", "--json")
					if err == nil {
						count, err := herdrTerminalCount(raw)
						if err != nil || count != 0 {
							return errors.New("refusing foreign herdr workspace")
						}
						paneName := herdrTerminalName(m.Instance)
						runArgs := []string{"run", "--bg", "--no-parent", "--no-notify", "--name", paneName, "--cwd", cmd.Dir}
						for _, kv := range buildHerdrLabels(m.Instance, "multiplexer-turn", &bridgeConfig.Launch, m.Labels) {
							runArgs = append(runArgs, "--label", kv)
						}
						runArgs = append(runArgs, "--json")
						raw, err = query(runArgs...)
						if err != nil {
							return err
						}
						paneID, gotName, err := parseHerdrCreatedTerminal(raw)
						if err != nil || paneID == "" || (gotName != "" && gotName != paneName && gotName != m.Instance) {
							return errors.New("invalid herdr workspace identity")
						}
						raw, err = query("get", paneID, "--json")
						if err != nil {
							return err
						}
						proc, err := parseHerdrProcessInfo(raw, paneID)
						if err != nil || proc.PID <= 1 || proc.PID != proc.PGID {
							return errors.New("invalid herdr pane process identity")
						}
						panePID = proc.PID
						paneMu.Lock()
						pane, err = processowner.RegisterRole(panePID, filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, bridgeConfig.SessionID, "agent-pane")
						if err == nil {
							err = processowner.Watch(pane)
						}
						if err == nil && cmd.Process != nil {
							holder = registerHerdrHolder(panePID, cmd.Process.Pid, dir)
						}
						paneMu.Unlock()
						if err != nil {
							return err
						}
						// Mark before writing: even a partial/ambiguous delivery is not retry proof.
						providerReleased = true
						if _, err = gateFile.WriteString("start\n"); err != nil {
							return err
						}
						monitorStarted = true
						watched, checked := pane, time.Now()
						go func() {
							defer close(monitorDone)
							for {
								if _, e := os.Stat(status); e == nil {
									monitorError = errors.Join(stopPane(), stopServer())
									return
								}
								// The pane shell stops itself after writing its status,
								// so a pane proved dead without one was lost: its
								// released provider's outcome is unknown, and the turn
								// ends now rather than at its bound (FAE-VER-010).
								if time.Since(checked) >= 100*time.Millisecond {
									checked = time.Now()
									if processowner.ProvedDead(watched.PID, watched.Birth) {
										monitorError = errors.Join(errors.New("herdr pane lost before reporting its exit status"), stopPane(), stopServer())
										return
									}
								}
								select {
								case <-monitorCtx.Done():
									monitorError = stopPane()
									return
								case <-time.After(5 * time.Millisecond):
								}
							}
						}()
						return nil
					}
				}

				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
					return errors.New("herdr startup timeout")
				case <-ticker.C:
				}
			}
		},
		Finished: func(processErr error) error {
			cancelMonitor()
			if monitorStarted {
				<-monitorDone
			}
			closeStreams()
			if processErr != nil || monitorError != nil {
				return errors.Join(processErr, monitorError)
			}
			raw, e := os.ReadFile(status)
			if e != nil {
				return fmt.Errorf("herdr pane exit status: %w", e)
			}
			code, e := strconv.Atoi(strings.TrimSpace(string(raw)))
			if e != nil {
				return e
			}
			if code != 0 {
				return muxPaneExitStatus(code)
			}
			return nil
		},
		Close: func() error {
			cancelMonitor()
			e := stopPane()
			_ = stopServer()
			closeStreams()
			return e
		},
	}, nil
}
