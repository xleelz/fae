package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

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
	cmd := exec.CommandContext(ctx, "lsof", "-n", "-U", "-a", "-c", "herdr", "-Fpn")
	raw, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(raw) != 0 || len(exit.Stderr) != 0 {
			return nil, fmt.Errorf("Herdr socket census: %w", err)
		}
	}
	sockets, err := herdrSockets(raw)
	if err != nil {
		return nil, err
	}
	result := make(map[string]int)
	for _, socket := range sockets {
		q := exec.CommandContext(ctx, binary, "pane", "list")
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
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "HERDR_") {
			env = append(env, e)
		}
	}
	return append(env, "HERDR_SOCKET_PATH="+socket)
}
func herdrSockets(raw []byte) ([]string, error) {
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
		path := line[1:]
		// Herdr exposes a separate client attachment socket, not an API socket.
		if strings.HasSuffix(path, "-client.sock") {
			continue
		}
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
			return nil, errors.New("invalid Herdr socket path")
		}
		seen[path] = true
		processes[pid] = true
	}
	for _, found := range processes {
		if !found {
			return nil, errors.New("Herdr process has no discoverable API socket")
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	return paths, nil
}
func herdrPaneCount(raw []byte) (int, error) {
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
