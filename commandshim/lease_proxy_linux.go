//go:build linux

package commandshim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

type LeaseProxy struct {
	activeMu           sync.Mutex
	active             map[int]scheduler.Owner
	observe            func(context.Context) (hostguard.Sample, error)
	hostPolicy         hostguard.Policy
	allowHostNamespace atomic.Bool
	listener           *net.UnixListener
	cancel             context.CancelFunc
	root               chan struct{}
	pid                int
	once               sync.Once
	wg                 sync.WaitGroup
	policy             RoleShims
}
type proxyRequest struct {
	Argv []string
	Dir  string
}
type proxyReply struct {
	Granted bool
	Error   string
}

// StartLeaseProxy keeps host admission and host process identities outside the
// session PID namespace. Only descendants of the bound bridge may request it.
func StartLeaseProxy(ctx context.Context, path string, policy RoleShims) (*LeaseProxy, error) {
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &LeaseProxy{listener: listener, cancel: cancel, root: make(chan struct{}), policy: policy, active: map[int]scheduler.Owner{}}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			conn, e := listener.AcceptUnix()
			if e != nil {
				return
			}
			p.wg.Add(1)
			go func() { defer p.wg.Done(); defer conn.Close(); p.serve(ctx, conn) }()
		}
	}()
	go func() { <-ctx.Done(); listener.Close() }()
	return p, nil
}
func (p *LeaseProxy) BindProcess(pid int) { p.once.Do(func() { p.pid = pid; close(p.root) }) }
func (p *LeaseProxy) Close() error {
	p.cancel()
	err := p.listener.Close()
	p.wg.Wait()
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}
func peerPID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var inner error
	err = raw.Control(func(fd uintptr) {
		cred, inner = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	})
	if err != nil {
		return 0, err
	}
	if inner != nil {
		return 0, inner
	}
	if cred.Uid != uint32(os.Getuid()) {
		return 0, fmt.Errorf("foreign lease peer")
	}
	return int(cred.Pid), nil
}
func parentPID(pid int) (int, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return 0, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(raw[i+1:]))
	if len(fields) < 2 {
		return 0, fmt.Errorf("short process stat")
	}
	return strconv.Atoi(fields[1])
}
func descendant(pid, root int) bool {
	for n := 0; n < 1024 && pid > 1; n++ {
		if pid == root {
			return true
		}
		next, err := parentPID(pid)
		if err != nil || next == pid {
			return false
		}
		pid = next
	}
	return false
}
func descendants(pid int) []int {
	entries, _ := os.ReadDir("/proc")
	out := []int{}
	for _, entry := range entries {
		n, err := strconv.Atoi(entry.Name())
		if err == nil && n != pid && descendant(n, pid) {
			out = append(out, n)
		}
	}
	return out
}

// A private PID namespace's init owns every process in that namespace: killing
// it makes the kernel kill all remaining members, including reparented work.
func namespaceOwner(pid int, allowHost bool) (scheduler.Owner, error) {
	ns, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
	if err != nil {
		return scheduler.Owner{}, err
	}
	host, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return scheduler.Owner{}, err
	}
	if ns == host {
		if allowHost {
			return scheduler.OwnerForPID(pid)
		}
		return scheduler.Owner{}, fmt.Errorf("heavy lease peer requires a private PID namespace")
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return scheduler.Owner{}, err
	}
	for _, entry := range entries {
		n, e := strconv.Atoi(entry.Name())
		if e != nil {
			continue
		}
		candidate, e := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", n))
		if e != nil || candidate != ns {
			continue
		}
		status, e := os.ReadFile(fmt.Sprintf("/proc/%d/status", n))
		if e != nil {
			continue
		}
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "NSpid:") {
				fields := strings.Fields(line)
				if len(fields) > 1 && fields[len(fields)-1] == "1" {
					return scheduler.OwnerForPID(n)
				}
			}
		}
	}
	return scheduler.Owner{}, fmt.Errorf("private PID namespace init not found")
}
func processStillRunning(owner scheduler.Owner) bool {
	switch scheduler.ProbeOwner(owner) {
	case scheduler.Dead:
		return false
	case scheduler.Unknown:
		return true
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", owner.PID))
	if err != nil {
		return true
	}
	i := strings.LastIndexByte(string(raw), ')')
	if i < 0 {
		return true
	}
	fields := strings.Fields(string(raw[i+1:]))
	return len(fields) == 0 || (fields[0] != "Z" && fields[0] != "X")
}
func stopProxyPeer(pid int, owner scheduler.Owner) {
	if !processStillRunning(owner) {
		return
	}
	// Production owner is the private PID namespace init. Host-namespace test
	// clients instead stop their descendant tree before stopping their owner.
	if owner.PID == pid {
		for _, child := range descendants(pid) {
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	}
	_ = syscall.Kill(owner.PID, syscall.SIGKILL)
	for processStillRunning(owner) {
		time.Sleep(10 * time.Millisecond)
	}
}
func (p *LeaseProxy) serve(ctx context.Context, conn *net.UnixConn) {
	reply := func(err error) { _ = json.NewEncoder(conn).Encode(proxyReply{Error: err.Error()}) }
	select {
	case <-p.root:
	case <-ctx.Done():
		return
	}
	pid, err := peerPID(conn)
	if err != nil {
		reply(err)
		return
	}
	if pid == p.pid || !descendant(pid, p.pid) {
		reply(fmt.Errorf("lease peer is not a bridge descendant"))
		return
	}
	owner, err := namespaceOwner(pid, p.allowHostNamespace.Load())
	if err != nil {
		reply(err)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	var request proxyRequest
	if err = json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&request); err != nil {
		reply(err)
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	if len(request.Argv) == 0 {
		reply(fmt.Errorf("empty proxy command"))
		return
	}
	name := filepath.Base(request.Argv[0])
	_, granted := shimCapability(p.policy, request.Argv)
	if !granted || (len(p.policy.Shims) > 0 && !slices.Contains(p.policy.Shims, name)) || p.policy.Tools[name] == "" || request.Argv[0] != p.policy.Tools[name] {
		reply(fmt.Errorf("role capability does not grant proxy command"))
		return
	}
	// Revalidate the command's path and resource clamp on the host as well as
	// inside the session. The actual command still runs only inside the view.
	policy := Policy{Worktree: p.policy.Worktree, CWD: request.Dir, CodeRole: true, LeaseHeld: true, Local: p.policy.Local, RAMRoots: p.policy.RAMRoots}
	argv, err := policy.Prepare(request.Argv)
	if err != nil {
		reply(err)
		return
	}
	peerOwner, err := scheduler.OwnerForPID(pid)
	if err != nil {
		reply(err)
		return
	}
	p.activeMu.Lock()
	for activePID, activeOwner := range p.active {
		if scheduler.ProbeOwner(activeOwner) != scheduler.Dead && descendant(pid, activePID) {
			p.activeMu.Unlock()
			reply(fmt.Errorf("nested heavy command inside a leased command is refused (FAE-HOST-014)"))
			return
		}
	}
	p.active[pid] = peerOwner
	p.activeMu.Unlock()
	defer func() { p.activeMu.Lock(); delete(p.active, pid); p.activeMu.Unlock() }()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-runCtx.Done():
			_ = conn.Close()
		case <-watchDone:
		}
	}()
	// Read completion while admission is queued as well as while running. EOF
	// cancels a queued request promptly instead of waiting for its eventual grant.
	completion := make(chan error, 1)
	go func() {
		var done struct{ Done bool }
		err := json.NewDecoder(io.LimitReader(conn, 1024)).Decode(&done)
		if err == nil && !done.Done {
			err = fmt.Errorf("invalid proxy completion")
		}
		completion <- err
		if err != nil {
			cancel()
		}
	}()
	h := HeavyOp{Ledger: p.policy.Ledger, Weight: ShimWeight(argv), Class: "role", Argv: argv, owner: &owner, AlertSpool: p.policy.AlertSpool, Observe: p.observe, Policy: p.hostPolicy}
	h.Admit = func(bool) ([]string, error) { return policy.Prepare(request.Argv) }
	h.execute = func(context.Context) (int, error) {
		if err := json.NewEncoder(conn).Encode(proxyReply{Granted: true}); err != nil {
			stopProxyPeer(pid, owner)
			return 0, err
		}
		err := <-completion
		// A completed client has waited its child. Descendants lingering after it
		// are stopped before the lease can become available to another command.
		children := descendants(pid)
		if err != nil || len(children) > 0 {
			stopProxyPeer(pid, owner)
			return 0, fmt.Errorf("proxy command disconnected or retained descendants")
		}
		return 0, nil
	}
	_, err = RunHeavyOp(runCtx, h)
	if err != nil {
		reply(err)
		return
	}
	_ = json.NewEncoder(conn).Encode(proxyReply{Granted: true})
}
func runProxyCommand(ctx context.Context, h HeavyOp) (int, error) {
	conn, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: h.proxySocket, Net: "unix"})
	if err != nil {
		return HeavyOpRefused, fmt.Errorf("host lease proxy: %w", err)
	}
	defer conn.Close()
	if err = json.NewEncoder(conn).Encode(proxyRequest{Argv: h.Argv, Dir: h.Dir}); err != nil {
		return HeavyOpRefused, err
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()
	var reply proxyReply
	if err = json.NewDecoder(conn).Decode(&reply); err != nil {
		return HeavyOpRefused, err
	}
	if !reply.Granted {
		return HeavyOpRefused, fmt.Errorf("host lease proxy refused: %s", reply.Error)
	}
	if h.Admit != nil {
		h.Argv, err = h.Admit(true)
		if err != nil {
			return Refused, err
		}
	}
	cmd := exec.CommandContext(ctx, h.Argv[0], h.Argv[1:]...)
	cmd.Env, cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = StripLeaseMarkers(h.Env), h.Dir, h.Stdin, h.Stdout, h.Stderr
	configureHeavyOpParentDeath(cmd)
	err = cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			code = 128 + int(status.Signal())
		}
		err = nil
	}
	if sendErr := json.NewEncoder(conn).Encode(struct{ Done bool }{true}); sendErr != nil {
		return HeavyOpRefused, sendErr
	}
	if recvErr := json.NewDecoder(conn).Decode(&reply); recvErr != nil {
		return HeavyOpRefused, recvErr
	}
	if !reply.Granted {
		return HeavyOpRefused, fmt.Errorf("host lease proxy: %s", reply.Error)
	}
	return code, err
}
