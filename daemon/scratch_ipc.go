package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/xleelz/fae/processowner"
)

// IPC has no recoverable byte stream. Its exact filesystem identity is retained
// in the snapshot instead; an open endpoint never authorizes cleanup.
func scratchIPC(root, path string, info os.FileInfo) (string, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", err
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) != 3 || parts[0] != "sessions" {
		return "", errors.New("unknown scratch special file: " + rel)
	}
	fifo := strings.HasPrefix(parts[1], "session-") && (parts[2] == "start.pipe" || parts[2] == "output.pipe") && info.Mode()&os.ModeNamedPipe != 0
	socket := strings.HasPrefix(parts[1], "resident-") && parts[2] == "mux-client.sock" && info.Mode()&os.ModeSocket != 0
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (!fifo && !socket) || st.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("unowned scratch special file: " + rel)
	}
	for _, dir := range []string{filepath.Dir(path), filepath.Join(root, "sessions")} {
		d, e := os.Lstat(dir)
		if e != nil {
			return "", e
		}
		ds, ok := d.Sys().(*syscall.Stat_t)
		if !ok || !d.IsDir() || d.Mode().Perm() != 0700 || ds.Uid != uint32(os.Getuid()) {
			return "", errors.New("unsafe scratch IPC directory")
		}
	}
	return fmt.Sprintf("ipc:v1:%d:%d:%d:%d", st.Dev, st.Ino, uint32(info.Mode()), st.Uid), nil
}

func scratchIPCMarkers(root, path string) error {
	dir := filepath.Dir(path)
	marker := "config.json"
	if strings.HasPrefix(filepath.Base(dir), "resident-") {
		marker = "resident-owner.json"
	}
	p := filepath.Join(dir, marker)
	info, err := os.Lstat(p)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || st.Uid != uint32(os.Getuid()) || info.Size() > 4<<20 {
		return errors.New("unsafe scratch IPC marker")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if marker == "config.json" {
		var c struct {
			Protocol string `json:"protocol"`
			Role     string `json:"role"`
			Workflow string `json:"workflow_id"`
			Session  string `json:"session_id"`
			Task     string `json:"task_id"`
			Log      string `json:"log_directory"`
			Handoff  string `json:"handoff"`
			Worktree string `json:"worktree"`
		}
		if json.Unmarshal(raw, &c) != nil || c.Protocol != "fae-agent-v1" || c.Session == "" || c.Task == "" || c.Log != dir || c.Handoff != filepath.Join(dir, "handoff.json") || (filepath.Dir(c.Worktree) != filepath.Join(root, "worktrees") && !(c.Worktree == filepath.Join(root, "prophet-context") && c.Role == "prophet" && c.Workflow == "fae-prophet" && strings.HasPrefix(c.Session, "prophet-"))) {
			return errors.New("scratch session IPC binding mismatch")
		}
	} else {
		var m struct {
			Owner, Session, ParentBirth, Socket, Request string
			ParentPID                                    int
		}
		if json.Unmarshal(raw, &m) != nil || m.Owner != filepath.Join(root, "sessions") || !strings.HasPrefix(m.Session, "resident-") || m.ParentPID <= 1 || m.ParentBirth == "" || m.Socket != filepath.Join(dir, "mux.sock") || m.Request != filepath.Join(dir, "request.sock") {
			return errors.New("scratch resident IPC binding mismatch")
		}
		birth, e := processowner.Birth(m.ParentPID)
		if e == nil && birth == m.ParentBirth {
			return errors.New("scratch resident owner still alive")
		}
		if e != nil && syscall.Kill(m.ParentPID, 0) != syscall.ESRCH {
			return errors.New("scratch resident owner death unproven")
		}
	}
	return nil
}

// lsof is required only when retired IPC exists. Unknown diagnostics, permissions
// failures and missing tooling fail closed; a successful owner-wide enumeration
// must contain no matching pathname fields.
func scratchIPCUnused(ctx context.Context, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	for _, path := range paths {
		for _, c := range path {
			if c < 32 || c > 126 || c == '\\' {
				return errors.New("scratch IPC path cannot be safely compared with lsof")
			}
		}
	}
	// Darwin's pathname selection omits open FIFO descriptors. Enumerate the
	// owner's descriptors instead and compare explicit pathname fields.
	cmd := exec.CommandContext(ctx, "lsof", "-nP", "-a", "-u", strconv.Itoa(os.Getuid()), "-Fpn")
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	out, err := cmd.Output()
	if err != nil || diagnostic.Len() != 0 {
		return errors.New("scratch IPC descriptor enumeration failed")
	}
	wanted := map[string]bool{}
	for _, path := range paths {
		wanted[path] = true
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") && wanted[strings.TrimPrefix(line, "n")] {
			return errors.New("scratch IPC still has open handles")
		}
	}
	return nil
}
