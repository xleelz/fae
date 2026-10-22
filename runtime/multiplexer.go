package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/processowner"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Multiplexer runs the same strict bridge contract as Direct in an isolated herdr
// server. Each turn has a private socket and an instance-named session. Existing
// servers are never adopted, and ambient HERDR_* are never inherited.
type Multiplexer struct {
	Direct      *Direct
	Binary      string
	Instance    string
	BeforeStart func(context.Context, string, string) error
	once        sync.Once
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
			return nil, e
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
	script := "#!/bin/sh\nIFS= read -r permit < " + quote(gate) + " || exit 125\n[ \"$permit\" = start ] || exit 125\n" + strings.Join(args, " ") + " < " + quote(filepath.Join(dir, "brief.json")) + " > " + quote(fifo) + " 2>&1\nresult=$?\nprintf '%s\\n' \"$result\" > " + quote(status+".tmp") + "\nmv -- " + quote(status+".tmp") + " " + quote(status) + "\nkill -STOP $$\nexit \"$result\"\n"
	if e = os.WriteFile(wrapper, []byte(script), 0700); e != nil {
		return nil, e
	}
	config := filepath.Join(dir, "herdr.toml")
	configuration := "onboarding=false\n[terminal]\ndefault_shell=" + strconv.Quote(wrapper) + "\nshell_mode=\"non_login\"\n[update]\nversion_check=false\n"
	if e = os.WriteFile(config, []byte(configuration), 0600); e != nil {
		return nil, e
	}
	env := cmd.Env[:0]
	for _, entry := range cmd.Env {
		if strings.HasPrefix(entry, "HERDR_") {
			continue
		}
		env = append(env, entry)
	}
	cmd.Env = append(env, "HERDR_CONFIG_PATH="+config, "HERDR_SOCKET_PATH="+socket)
	cmd.Path = m.Binary
	cmd.Args = []string{m.Binary, "server"}
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
	var paneMu sync.Mutex
	stopPane := func() error {
		paneMu.Lock()
		defer paneMu.Unlock()
		if pane == nil {
			return nil
		}
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if e := processowner.Terminate(stopCtx, pane, m.Direct.Root); e != nil {
			return fmt.Errorf("herdr pane %d retirement: %w", panePID, e)
		}
		return nil
	}
	originalCancel := cmd.Cancel
	cmd.Cancel = func() error { return errors.Join(stopPane(), originalCancel()) }
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
				raw, err := query("workspace", "list")
				if err == nil {
					var listing struct {
						Result struct {
							Workspaces []json.RawMessage `json:"workspaces"`
						} `json:"result"`
					}
					if json.Unmarshal(raw, &listing) != nil || listing.Result.Workspaces == nil || len(listing.Result.Workspaces) != 0 {
						return errors.New("refusing foreign herdr workspace")
					}
					raw, err = query("workspace", "create", "--cwd", cmd.Dir, "--label", m.Instance, "--no-focus")
					if err != nil {
						return err
					}
					var created struct {
						Result struct {
							RootPane struct {
								ID string `json:"pane_id"`
							} `json:"root_pane"`
							Workspace struct {
								Label string `json:"label"`
							} `json:"workspace"`
						} `json:"result"`
					}
					if json.Unmarshal(raw, &created) != nil || created.Result.RootPane.ID == "" || created.Result.Workspace.Label != m.Instance {
						return errors.New("invalid herdr workspace identity")
					}
					raw, err = query("pane", "process-info", "--pane", created.Result.RootPane.ID)
					if err != nil {
						return err
					}
					var info struct {
						Result struct {
							Info struct {
								PID  int `json:"shell_pid"`
								PGID int `json:"foreground_process_group_id"`
							} `json:"process_info"`
						} `json:"result"`
					}
					if json.Unmarshal(raw, &info) != nil || info.Result.Info.PID <= 1 || info.Result.Info.PID != info.Result.Info.PGID {
						return errors.New("invalid herdr pane process identity")
					}
					panePID = info.Result.Info.PID
					paneMu.Lock()
					pane, err = processowner.RegisterLinked(panePID, filepath.Join(m.Direct.Root, ".processes"), m.Direct.Root, bridgeConfig.SessionID)
					if err == nil {
						err = processowner.Watch(pane)
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
					go func() {
						defer close(monitorDone)
						for {
							if _, e := os.Stat(status); e == nil {
								monitorError = errors.Join(stopPane(), stopServer())
								return
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
				return fmt.Errorf("herdr pane exit status %d", code)
			}
			return nil
		},
		Close: func() error {
			cancelMonitor()
			e := stopPane()
			closeStreams()
			return e
		},
	}, nil
}
