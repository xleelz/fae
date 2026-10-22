package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/xleelz/fae/kernel"
	agentruntime "github.com/xleelz/fae/runtime"
)

// SessionAudit links a disposable Fae turn to its provider context. The context
// is a cache, never a source of authoritative workflow or artifact state.
type SessionAudit struct {
	Scope      string `json:"scope"`
	ThreadID   string `json:"thread_id"`
	Resumed    bool   `json:"resumed"`
	TaskID     string `json:"task_id"`
	DispatchID string `json:"dispatch_id"`
}
type sessionState struct {
	Scope    string `json:"scope"`
	ThreadID string `json:"thread_id"`
	Pending  bool   `json:"pending"`
}
type providerSession struct {
	dir     string
	lock    *os.File
	state   sessionState
	resumed bool
}

var threadIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func privateDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("absolute session cache path required")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || real != filepath.Clean(path) {
		return errors.New("canonical session cache directory required")
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st.Mode().Perm()&0077 != 0 {
		return errors.New("private session cache directory required")
	}
	return nil
}
func openProviderSession(ctx context.Context, root string, l agentruntime.BridgeConfig) (*providerSession, error) {
	scopedPolicy := l.SessionPolicy == "workflow-global" || l.SessionPolicy == "task-bound" && l.TaskID != ""
	if root != "" && !scopedPolicy {
		return nil, errors.New("persistent provider requires a declared session scope and task identity")
	}
	if !kernel.CanResumeProvider(root != "", scopedPolicy, l.FreshContext) {
		return nil, nil
	}
	if l.WorkflowID == "" || l.WorkflowRevision == "" || l.Role == "" {
		return nil, errors.New("persistent provider requires workflow revision and role")
	}
	if err := privateDirectory(root); err != nil {
		return nil, err
	}
	// Revision includes role instructions, briefs and allowed output schemas.
	scope := []any{l.WorkflowID, l.WorkflowRevision, l.Role, l.Binding, l.Profile, l.Mode}
	// Preserve the workflow-global scope across tasks; task-bound context
	// resumes only within its exact task and is fresh for the next task.
	if l.SessionPolicy == "task-bound" {
		scope = append(scope, "task-bound", l.TaskID)
	}
	scopeRaw, _ := json.Marshal(scope)
	sum := sha256.Sum256(scopeRaw)
	key := hex.EncodeToString(sum[:])
	dir := filepath.Join(root, key)
	if err := privateDirectory(dir); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if err != syscall.EWOULDBLOCK && err != syscall.EAGAIN {
			lock.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			lock.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	s := &providerSession{dir: dir, lock: lock, state: sessionState{Scope: key}}
	fail := func(err error) (*providerSession, error) { s.close(); return nil, err }
	path := filepath.Join(dir, "state.json")
	raw, err := regularRead(path, true)
	if err != nil {
		return fail(err)
	}
	if len(raw) > 0 {
		if err = json.Unmarshal(raw, &s.state); err != nil {
			return fail(err)
		}
		if s.state.Scope != key || s.state.ThreadID != "" && !threadIDPattern.MatchString(s.state.ThreadID) {
			return fail(errors.New("invalid provider session cache"))
		}
		// A process death or failed validation may leave provider-side effects in
		// context. Rotate instead of replaying an uncertain turn into that context.
		if s.state.Pending {
			s.state.ThreadID = ""
		}
	}
	s.resumed = s.state.ThreadID != ""
	s.state.Pending = true
	if err = s.save(); err != nil {
		return fail(err)
	}
	if err = privateDirectory(filepath.Join(dir, "cwd")); err != nil {
		return fail(err)
	}
	return s, nil
}
func (s *providerSession) close() {
	if s != nil && s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}
func (s *providerSession) save() error {
	raw, err := json.Marshal(s.state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, "state-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(s.dir, "state.json")); err != nil {
		return err
	}
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (s *providerSession) complete(thread string) error {
	if !threadIDPattern.MatchString(thread) {
		return errors.New("provider did not announce a valid thread")
	}
	if s.resumed && thread != s.state.ThreadID {
		return errors.New("provider resumed a different thread")
	}
	s.state.ThreadID = thread
	s.state.Pending = false
	return s.save()
}

// announcedThread consumes identity events only. Provider error classification
// remains exclusively in providerhealth's shared session-log reader.
func announcedThread(path string) (string, error) {
	raw, err := regularRead(path, false)
	if err != nil {
		return "", err
	}
	var thread string
	for _, line := range bytesLines(raw) {
		var v struct {
			Type   string `json:"type"`
			Thread string `json:"thread_id"`
		}
		if json.Unmarshal(line, &v) == nil && v.Type == "thread.started" {
			if !threadIDPattern.MatchString(v.Thread) || thread != "" && thread != v.Thread {
				return "", errors.New("invalid provider thread identity")
			}
			thread = v.Thread
		}
	}
	if thread == "" {
		return "", fmt.Errorf("provider thread identity missing in %s", path)
	}
	return thread, nil
}
func bytesLines(raw []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			lines = append(lines, raw[start:i])
			start = i + 1
		}
	}
	if start < len(raw) {
		lines = append(lines, raw[start:])
	}
	return lines
}
