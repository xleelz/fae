package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var lsofSocketSuffix = regexp.MustCompile(` type=[A-Z]+( \([A-Z_]+\))?$`)

// LsofName returns the pathname of an lsof -F n field value. Linux lsof
// appends the socket type (and newer releases its state) to UNIX socket
// names: "/run/x.sock type=STREAM (LISTEN)"; other names are unchanged.
func LsofName(field string) string { return lsofSocketSuffix.ReplaceAllString(field, "") }

// HerdrInventory counts actual panes, including workspaces owned by other
// instances and the operator. It never adopts or mutates those workspaces.
// lsof discovers private as well as default/named-session API sockets. A failed
// census is not an empty census: callers must withhold admission on any error.
func HerdrInventory(ctx context.Context, binary string) (map[string]int, error) {
	if !filepath.IsAbs(binary) {
		return nil, errors.New("absolute Herdr binary required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var result map[string]int
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		result, err = herdrInventoryOnce(ctx, binary)
		if err == nil {
			return result, nil
		}
		if attempt < 2 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	return nil, err
}

// Servers can retire between socket discovery and query. Retry the complete
// census, never discard a failing socket from an otherwise partial snapshot.
func herdrInventoryOnce(ctx context.Context, binary string) (map[string]int, error) {
	cmd := exec.CommandContext(ctx, "lsof", "-n", "-U", "-a", "-c", "herdr-core", "-Fpn")
	raw, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(raw) != 0 || len(exit.Stderr) != 0 {
			return nil, fmt.Errorf("Herdr socket census: %w", err)
		}
	}
	sockets, err := herdrSockets(raw, func(pid int) ([]string, error) { return herdrProcessArgs(ctx, pid) })
	if err != nil {
		return nil, err
	}
	result := make(map[string]int)
	for _, socket := range sockets {
		if !socketReady(socket) {
			return nil, fmt.Errorf("Herdr pane census %s: socket not listening", socket)
		}
		q := exec.CommandContext(ctx, binary, "ls", "--json")
		q.Env = herdrInventoryEnv(socket)
		out, err := q.Output()
		if err != nil {
			return nil, fmt.Errorf("Herdr pane census %s: %w", socket, err)
		}
		n, err := herdrPaneCount(out)
		if err != nil {
			return nil, fmt.Errorf("Herdr pane census %s: %w", socket, err)
		}
		result[socket] = n
	}
	return result, nil
}
func herdrInventoryEnv(socket string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "HERDR_") && !strings.HasPrefix(e, "HC_") {
			env = append(env, e)
		}
	}
	return append(env, "HC_SOCKET="+socket, "HERDR_SOCKET_PATH="+socket)
}

func isHerdrExecutable(exe string) bool {
	base := filepath.Base(strings.TrimSpace(exe))
	return base == "herdr-core" || base == "herdr" || strings.HasPrefix(base, "herdr-core.") || strings.HasPrefix(base, "herdr.")
}

// herdrSockets returns the API sockets of every Herdr process lsof listed. A
// process with no API socket is tolerated only when its argument vector
// positively identifies a client role, which hosts no panes; a server, an
// unrecognised role or an unreadable command line fails the census closed.
func herdrSockets(raw []byte, args func(pid int) ([]string, error)) ([]string, error) {
	seen := map[string]bool{}
	processes := map[int]bool{}
	pid := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "p") {
			var err error
			pid, err = strconv.Atoi(line[1:])
			if err != nil || pid <= 0 {
				return nil, errors.New("invalid Herdr census PID")
			}
			processes[pid] = false
		}
		if !strings.HasPrefix(line, "n/") {
			continue
		}
		path := LsofName(line[1:])
		// Herdr exposes a separate client attachment socket, not an API socket.
		if strings.HasSuffix(path, "-client.sock") {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return nil, errors.New("invalid Herdr socket path")
		}
		// herdr-core spawns a per-session holder process listening on
		// *.holder.*.sock; its terminals are counted via the server's API socket.
		if strings.Contains(filepath.Base(path), ".holder.") {
			processes[pid] = true
			continue
		}
		seen[path] = true
		processes[pid] = true
	}
	for pid, found := range processes {
		if found {
			continue
		}
		if argv, err := args(pid); err != nil || !herdrClient(argv) {
			return nil, fmt.Errorf("Herdr process %d has no discoverable API socket", pid)
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	return paths, nil
}

// Herdr clients: the attach launcher (no subcommand), the SSH remote
// bridge and API subcommands connect to a server that runs as a separate
// "herdr-core serve" (or legacy "herdr server") process; "server" with a
// further client subcommand is a client too.
var herdrClientCommands = map[string]bool{
	"run": true, "ls": true, "get": true, "attach": true, "read": true,
	"kill": true, "rm": true, "label": true, "keep": true, "send": true,
	"keys": true, "interrupt": true, "wait": true, "put": true, "exec": true,
	"file": true, "templates": true, "events": true, "update": true,
	"machine": true, "connect": true, "schema": true, "version": true, "help": true,
	"remote-client-bridge": true, "api": true, "workspace": true, "worktree": true,
	"tab": true, "notification": true, "agent": true, "pane": true, "session": true,
	"integration": true, "status": true, "completion": true, "config": true,
	"channel": true, "terminal": true, "plugin": true, "layout": true,
}
var herdrServerClientCommands = map[string]bool{
	"start": true, "stop": true, "status": true, "restart": true, "help": true,
	"reload-config": true, "live-handoff": true, "agent-manifests": true,
	"reload-agent-manifests": true, "update-agent-manifests": true,
}

func herdrClient(argv []string) bool {
	if len(argv) == 0 || !isHerdrExecutable(argv[0]) {
		return false
	}
	var words []string
	for i := 1; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--session" || a == "--machine" || a == "--remote" || a == "--remote-keybindings" || a == "--socket":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			words = append(words, a)
		}
	}
	switch {
	case len(words) == 0:
		return true
	case words[0] == "serve" || words[0] == "hold":
		return false
	case words[0] == "server":
		return len(words) > 1 && herdrServerClientCommands[words[1]]
	}
	return herdrClientCommands[words[0]]
}

// herdrProcessArgs reads a process's exact argument vector from /proc, or from
// ps where there is no /proc (macOS). A vanished process has no arguments.
func herdrProcessArgs(ctx context.Context, pid int) ([]string, error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err == nil {
		if len(raw) == 0 {
			return nil, errors.New("empty Herdr command line")
		}
		return strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00"), nil
	}
	if _, e := os.Stat("/proc/self/cmdline"); e == nil || !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	out, err := exec.CommandContext(ctx, "ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil, err
	}
	if argv := strings.Fields(string(out)); len(argv) > 0 {
		return argv, nil
	}
	return nil, errors.New("empty Herdr command line")
}
func herdrPaneCount(raw []byte) (int, error) {
	var coreEnvelope struct {
		Terminals *[]struct {
			Address string `json:"address"`
			State   string `json:"state"`
		} `json:"terminals"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &coreEnvelope) == nil && coreEnvelope.Terminals != nil && len(coreEnvelope.Error) == 0 {
		ids := map[string]bool{}
		live := 0
		for _, t := range *coreEnvelope.Terminals {
			if t.Address == "" || ids[t.Address] {
				return 0, errors.New("duplicate or missing Herdr pane identity")
			}
			ids[t.Address] = true
			if t.State == "" || t.State == "live" {
				live++
			}
		}
		return live, nil
	}
	var envelope struct {
		Result struct {
			Type  string `json:"type"`
			Panes []struct {
				ID string `json:"pane_id"`
			} `json:"panes"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil || envelope.Result.Type != "pane_list" || envelope.Result.Panes == nil || len(envelope.Error) > 0 {
		return 0, errors.New("invalid Herdr pane inventory")
	}
	ids := map[string]bool{}
	for _, p := range envelope.Result.Panes {
		if p.ID == "" || ids[p.ID] {
			return 0, errors.New("duplicate or missing Herdr pane identity")
		}
		ids[p.ID] = true
	}
	return len(ids), nil
}
