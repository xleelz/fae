// Package daemon owns the instance socket, private control plane and durable drive.
package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/xleelz/fae/workspace"
)

type Layout struct{ Root, Run, Socket, Credential, Data, Binding, Log, Worktrees string }

var instanceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func Paths(root, instance string) (Layout, error) {
	if err := workspace.Validate(root); err != nil {
		return Layout{}, err
	}
	if !instanceName.MatchString(instance) {
		return Layout{}, errors.New("invalid instance name")
	}
	r := filepath.Join(root, instance)
	run := filepath.Join(r, "run")
	return Layout{r, run, filepath.Join(run, "rpc.sock"), filepath.Join(run, "credential"), filepath.Join(r, "plane"), filepath.Join(r, "workspace.json"), filepath.Join(r, "supervision.log"), filepath.Join(r, "worktrees")}, nil
}

type owner struct {
	PID      int       `json:"pid"`
	Start    string    `json:"start"`
	Launched time.Time `json:"launched"`
}
type instanceLock struct {
	file  *os.File
	owner owner
	stale bool
}

func processStart(pid int) (string, error) {
	b, err := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "lstart=").Output()
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", errors.New("process identity unavailable")
	}
	return s, nil
}
func acquire(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		var old owner
		_ = json.NewDecoder(f).Decode(&old)
		f.Close()
		return nil, fmt.Errorf("instance already owned by pid %d", old.PID)
	}
	var old owner
	_ = json.NewDecoder(f).Decode(&old)
	if old.PID != 0 {
		if start, err := processStart(old.PID); err == nil && start == old.Start {
			f.Close()
			return nil, fmt.Errorf("unlocked record still names live owner pid %d; refusing reclamation", old.PID)
		}
	}
	start, err := processStart(os.Getpid())
	if err != nil {
		f.Close()
		return nil, err
	}
	l := &instanceLock{f, owner{os.Getpid(), start, time.Now().UTC()}, old.PID != 0}
	if err = f.Truncate(0); err == nil {
		_, err = f.Seek(0, 0)
	}
	if err == nil {
		err = json.NewEncoder(f).Encode(l.owner)
	}
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return l, nil
}
func (l *instanceLock) close() {
	if l != nil && l.file != nil {
		_ = l.file.Truncate(0)
		_ = l.file.Sync()
		_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
		_ = l.file.Close()
	}
}
func checkLayout(c Config, l Layout) error {
	current, err := os.Getwd()
	if err != nil {
		return err
	}
	var binding struct {
		Workspace string `json:"workspace"`
	}
	b, err := os.ReadFile(l.Binding)
	if err == nil {
		if err = json.Unmarshal(b, &binding); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err = workspace.CheckBinding(c.Workspace, current, binding.Workspace); err != nil {
		return err
	}
	// Secret files and the control plane must live outside every repository.
	p := l.Root
	for {
		if _, e := os.Lstat(filepath.Join(p, ".git")); e == nil {
			return errors.New("instance root must be outside a repository")
		}
		next := filepath.Dir(p)
		if next == p {
			break
		}
		p = next
	}
	return nil
}
func mkdirPrivate(path string) error {
	// Refuse existing symlink ancestors before MkdirAll can follow them.
	for p := path; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("instance path contains symlink: %s", p)
		}
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("unsafe instance directory %s (requires 0700)", path)
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if physical != path {
		return fmt.Errorf("instance path contains symlink: %s", path)
	}
	return nil
}

func writePrivate(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fmt.Errorf("unsafe private file %s", path)
	}
	if err = f.Truncate(0); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	return f.Sync()
}
