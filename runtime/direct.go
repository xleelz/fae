package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/commandshim"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/workflow"
)

// Direct runs a trusted fae-agent-v1 bridge, not an arbitrary agent CLI. The
// bridge MUST enforce Config.Profile using its provider's actual tool controls.
// Merely putting the profile into a prompt does not implement this protocol.
type Direct struct {
	Command           []string
	Root              string
	Environment       map[string]string
	Timeout           time.Duration
	StartTimeout      time.Duration
	StallTimeout      time.Duration
	StallPollInterval time.Duration
	MaxHandoffBytes   int64
	TailBytes         int
	mu                sync.Mutex
	live              map[string]int
	seen              map[string]bool
	prepareProcess    func(context.Context, *exec.Cmd, string) (*processHooks, error)
}
type Launch struct {
	WorkflowID       string           `json:"workflow_id,omitempty"`
	WorkflowRevision string           `json:"workflow_revision,omitempty"`
	SessionPolicy    string           `json:"session_policy,omitempty"`
	FreshContext     bool             `json:"fresh_context,omitempty"`
	DispatchID       string           `json:"dispatch_id"`
	SessionID        string           `json:"session_id"`
	TaskID           string           `json:"task_id"`
	Role             string           `json:"role"`
	Worktree         string           `json:"worktree"`
	Mode             string           `json:"mode"`
	Profile          workflow.Profile `json:"profile"`
	Binding          workflow.Binding `json:"binding"`
	Brief            json.RawMessage  `json:"brief"`
}
type BridgeConfig struct {
	Protocol string `json:"protocol"`
	Launch
	Handoff      string `json:"handoff"`
	LogDirectory string `json:"log_directory"`
}
type Handoff struct {
	DispatchID string          `json:"dispatch_id"`
	SessionID  string          `json:"session_id"`
	TaskID     string          `json:"task_id"`
	Output     json.RawMessage `json:"output"`
}
type Collected struct {
	// NonExecution is positive transport proof that no provider code was released.
	NonExecution       bool
	ProcessesRetired   bool
	Output             json.RawMessage
	Tail, LogDirectory string
	ProducedAt         time.Time
}
type TurnError struct {
	Class, Path, Role, DispatchID string
	Cause                         error
}

func (e *TurnError) Error() string {
	return fmt.Sprintf("%s: role=%s dispatch=%s handoff=%s: %v", e.Class, e.Role, e.DispatchID, e.Path, e.Cause)
}
func (e *TurnError) Unwrap() error { return e.Cause }

// Sessions returns a copy of only processes launched by this adapter.
func (d *Direct) Sessions() map[string]int {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]int{}
	for k, v := range d.live {
		out[k] = v
	}
	return out
}
func canonicalDirectory(p string) error {
	if !filepath.IsAbs(p) {
		return errors.New("directory must be absolute")
	}
	actual, e := filepath.EvalSymlinks(p)
	if e != nil {
		return e
	}
	if actual != filepath.Clean(p) {
		return errors.New("directory must be canonical, without symlinks")
	}
	st, e := os.Stat(p)
	if e != nil {
		return e
	}
	if !st.IsDir() {
		return errors.New("not a directory")
	}
	return nil
}
func (d *Direct) Run(ctx context.Context, l Launch) (result Collected, err error) {
	groupRetired, hooksRetired := true, true
	defer func() { result.ProcessesRetired = groupRetired && hooksRetired }()
	fail := func(class, path string, e error) (Collected, error) {
		return result, &TurnError{class, path, l.Role, l.DispatchID, e}
	}
	if l.DispatchID == "" || l.SessionID == "" || l.TaskID == "" || l.Role == "" || (l.Mode != "local" && l.Mode != "normal") {
		return fail("launch-refused", "", errors.New("complete identity and mode required"))
	}
	if e := canonicalDirectory(l.Worktree); e != nil {
		return fail("launch-refused", "", e)
	}
	if e := canonicalDirectory(d.Root); e != nil {
		return fail("launch-refused", "", e)
	}
	if len(d.Command) == 0 || !filepath.IsAbs(d.Command[0]) {
		return fail("launch-refused", "", errors.New("bridge executable must be absolute"))
	}
	found := false
	for _, a := range d.Command {
		if a == "{config}" {
			found = true
		}
		if strings.ContainsRune(a, 0) {
			return fail("launch-refused", "", errors.New("NUL argument"))
		}
	}
	if !found {
		return fail("launch-refused", "", errors.New("fae-agent-v1 requires exact {config} argument"))
	}
	if !json.Valid(l.Brief) {
		return fail("launch-refused", "", errors.New("brief must be JSON"))
	}
	d.mu.Lock()
	if d.seen == nil {
		d.seen = map[string]bool{}
		d.live = map[string]int{}
	}
	if d.seen[l.SessionID] {
		d.mu.Unlock()
		return fail("uncertain-dispatch", "", errors.New("session already launched"))
	}
	d.seen[l.SessionID] = true
	d.mu.Unlock()
	startedAt := time.Now()
	dir, e := os.MkdirTemp(d.Root, "session-")
	if e != nil {
		return fail("launch-refused", "", e)
	}
	result.LogDirectory = dir
	handoff := filepath.Join(dir, "handoff.json")
	config := filepath.Join(dir, "config.json")
	b, e := json.Marshal(BridgeConfig{Protocol: "fae-agent-v1", Launch: l, Handoff: handoff, LogDirectory: dir})
	if e != nil {
		return fail("launch-refused", handoff, e)
	}
	if e = os.WriteFile(config, b, 0600); e != nil {
		return fail("launch-refused", handoff, e)
	}
	// Start from an allowlist, never inherit daemon credentials, Git overrides,
	// dynamic-loader injection, or shell startup configuration.
	env := []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir}
	for k, v := range d.Environment {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) || strings.HasPrefix(k, "FAE_") || strings.HasPrefix(k, "DOLT_") || strings.HasPrefix(k, "GIT_") || strings.HasPrefix(k, "DYLD_") || strings.HasPrefix(k, "LD_") || k == "HOME" || k == "TMPDIR" || k == "BASH_ENV" || k == "ENV" {
			return fail("launch-refused", handoff, fmt.Errorf("forbidden environment key %q", k))
		}
		env = append(env, k+"="+v)
	}
	shims, e := commandshim.InstallDenials(dir)
	if e != nil {
		return fail("launch-refused", handoff, e)
	}
	env = commandshim.PrependPath(env, shims)
	timeout := d.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args := append([]string(nil), d.Command...)
	for i, a := range args {
		if a == "{config}" {
			args[i] = config
		}
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = l.Worktree
	cmd.Env = env
	briefPath := filepath.Join(dir, "brief.json")
	if e = os.WriteFile(briefPath, l.Brief, 0600); e != nil {
		return fail("launch-refused", handoff, e)
	}
	briefFile, e := os.Open(briefPath)
	if e != nil {
		return fail("launch-refused", handoff, e)
	}
	defer briefFile.Close()
	cmd.Stdin = briefFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cap := d.TailBytes
	if cap <= 0 {
		cap = 64 << 10
	}
	tail := &tailBuffer{limit: cap}
	pipeRead, pipeWrite, e := os.Pipe()
	if e != nil {
		return fail("launch-refused", handoff, e)
	}
	defer pipeRead.Close()
	defer pipeWrite.Close()
	cmd.Stdout = pipeWrite
	cmd.Stderr = pipeWrite
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = time.Second
	// Bound transport startup independently from the full turn. The timer
	// cancels the same context so no abandoned startup goroutine can launch later.
	startLimit := d.StartTimeout
	if startLimit <= 0 {
		startLimit = 180 * time.Second
	}
	startupTimer := time.AfterFunc(startLimit, cancel)
	defer startupTimer.Stop()
	// A missing/non-executable bridge is detected before the gated launcher.
	if _, e := exec.LookPath(args[0]); e != nil {
		result.NonExecution = true
		return fail("startup-not-executed", handoff, e)
	}
	var hooks *processHooks
	if d.prepareProcess != nil {
		hooks, e = d.prepareProcess(ctx, cmd, dir)
		if e != nil {
			result.NonExecution = true
			return fail("startup-not-executed", handoff, e)
		}
		hooksRetired = false
		defer func() { e := hooks.Close(); hooksRetired = e == nil; err = errors.Join(err, e) }()
	}
	record, e := processowner.StartLinked(cmd, filepath.Join(d.Root, ".processes"), d.Root, l.SessionID)
	if e != nil {
		result.NonExecution = cmd.Process == nil
		groupRetired = cmd.Process == nil
		return fail("startup-not-executed", handoff, e)
	}
	defer func() { e := processowner.Retire(record); groupRetired = e == nil; err = errors.Join(err, e) }()
	pid := cmd.Process.Pid
	d.mu.Lock()
	d.live[l.SessionID] = pid
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.live, l.SessionID)
		d.mu.Unlock()
	}()
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(tail, pipeRead); close(drained) }()
	if hooks != nil {
		if e = hooks.Started(); e != nil {
			if hooks.NonExecution != nil {
				result.NonExecution = hooks.NonExecution()
			}
			_ = cmd.Cancel()
			_ = processowner.Wait(cmd, record)
			return fail("startup-not-executed", handoff, e)
		}
	}
	startupTimer.Stop()
	stallLimit := d.StallTimeout
	if stallLimit <= 0 {
		stallLimit = 5 * time.Minute
	}
	interval := d.StallPollInterval
	if interval <= 0 {
		interval = time.Second
	}
	monitorCtx, stopMonitor := context.WithCancel(ctx)
	stallDone := make(chan bool, 1)
	activityPID := func() int {
		if hooks != nil && hooks.ActivityPID != nil {
			return hooks.ActivityPID()
		}
		return pid
	}
	go monitorStall(monitorCtx, activityPID, dir, handoff, stallLimit, interval, cancel, stallDone)
	e = processowner.Wait(cmd, record)
	stopMonitor()
	stalled := <-stallDone
	if hooks != nil {
		e = hooks.Finished(e)
	}
	_ = pipeWrite.Close()
	select {
	case <-drained:
	case <-time.After(time.Second):
		_ = pipeRead.Close()
		<-drained
	}
	result.Tail = tail.String()
	if stalled {
		return fail("stalled", handoff, errors.New("positive liveness signals unchanged beyond stall threshold"))
	}
	if ctx.Err() != nil {
		return fail("interrupted", handoff, ctx.Err())
	}
	if e != nil {
		return fail("process-failed", handoff, e)
	}
	f, e := os.OpenFile(handoff, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		if cause := providerhealth.SessionLogs(result.LogDirectory); cause != providerhealth.OK {
			e = fmt.Errorf("provider cause %s: %w", cause, e)
		}
		return fail("missing-handoff", handoff, e)
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return fail("malformed-output", handoff, e)
	}
	max := d.MaxHandoffBytes
	if max <= 0 {
		max = 1 << 20
	}
	if st.ModTime().Before(startedAt) {
		return fail("stale-handoff", handoff, errors.New("handoff predates launch"))
	}
	if !st.Mode().IsRegular() || st.Size() > max {
		return fail("malformed-output", handoff, errors.New("handoff must be bounded regular file"))
	}
	raw, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil || int64(len(raw)) > max {
		return fail("malformed-output", handoff, errors.New("handoff exceeds bound"))
	}
	raw, e = domain.CanonicalJSON(raw)
	if e != nil {
		return fail("malformed-output", handoff, e)
	}
	var h Handoff
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if e = dec.Decode(&h); e != nil {
		return fail("malformed-output", handoff, e)
	}
	if h.DispatchID == "" {
		return fail("id-less-handoff", handoff, errors.New("dispatch_id required"))
	}
	if h.DispatchID != l.DispatchID || h.SessionID != l.SessionID || h.TaskID != l.TaskID {
		return fail("stale-handoff", handoff, errors.New("identity mismatch"))
	}
	if len(h.Output) == 0 {
		return fail("malformed-output", handoff, errors.New("output required"))
	}
	result.Output = h.Output
	result.ProducedAt = time.Now().UTC()
	return result, nil
}

type tailBuffer struct {
	mu    sync.Mutex
	data  []byte
	limit int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	if n >= t.limit {
		t.data = append(t.data[:0], p[n-t.limit:]...)
	} else {
		t.data = append(t.data, p...)
		if len(t.data) > t.limit {
			t.data = append([]byte(nil), t.data[len(t.data)-t.limit:]...)
		}
	}
	return n, nil
}
func (t *tailBuffer) String() string { t.mu.Lock(); defer t.mu.Unlock(); return string(t.data) }

// processHooks changes only transport; Direct retains handoff and identity validation.
type processHooks struct {
	NonExecution func() bool
	ActivityPID  func() int
	Started      func() error
	Finished     func(error) error
	Close        func() error
}
