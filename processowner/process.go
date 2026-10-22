// Package processowner records exact OS process birth identities before allowing
// owned workloads to run. Records survive the daemon; a pipe watchdog survives
// parent SIGKILL. Unprovable identities block recovery rather than authorize kills.
package processowner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrRetirementUnproven prevents releasing a lease after ambiguous cleanup.
var ErrRetirementUnproven = errors.New("workload retirement unproven")

type Record struct {
	PID, PGID, ParentPID                 int
	Birth, ParentBirth, Owner, Directory string
	Session                              string
	Command                              []string
	Path                                 string `json:"-"`
	watch                                *os.File
}

func init() {
	if len(os.Args) == 3 && os.Args[1] == "--fae-internal-process-watch" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		ctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		r, e := read(os.Args[2])
		if e == nil {
			e = terminate(ctx, r, r.Owner)
		}
		if e != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
}
func privateDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("absolute registry required")
	}
	for p := dir; ; p = filepath.Dir(p) {
		s, e := os.Lstat(p)
		if e == nil && s.Mode()&os.ModeSymlink != 0 {
			return errors.New("registry symlink")
		}
		if e != nil && !os.IsNotExist(e) {
			return e
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	s, e := os.Stat(dir)
	if e != nil {
		return e
	}
	if s.Mode().Perm() != 0700 {
		return errors.New("registry must be private")
	}
	return nil
}
func read(path string) (*Record, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !s.Mode().IsRegular() || s.Mode().Perm() != 0600 || s.Size() > 65536 {
		return nil, errors.New("invalid ownership record")
	}
	var r Record
	e = json.NewDecoder(io.LimitReader(f, 65537)).Decode(&r)
	if e != nil {
		return nil, e
	}
	if r.PID <= 1 || r.PGID != r.PID || r.Birth == "" || r.Owner == "" || r.ParentPID <= 1 || r.ParentBirth == "" {
		return nil, errors.New("invalid process identity")
	}
	r.Path = path
	return &r, nil
}

// Register may only register a caller-owned group leader which is stopped before
// workload execution. Start provides that gate. Multiplexers must provide their own.
func Register(pid int, dir, owner string) (*Record, error) {
	return RegisterLinked(pid, dir, owner, "")
}
func RegisterLinked(pid int, dir, owner, session string) (*Record, error) {
	if owner == "" {
		return nil, errors.New("owner required")
	}
	if e := privateDir(dir); e != nil {
		return nil, e
	}
	pg, e := syscall.Getpgid(pid)
	if e != nil {
		return nil, e
	}
	if pg != pid || pid <= 1 {
		return nil, errors.New("dedicated group leader required")
	}
	b, e := birth(pid)
	if e != nil {
		return nil, e
	}
	pb, e := birth(os.Getpid())
	if e != nil {
		return nil, e
	}
	r := &Record{PID: pid, PGID: pg, Birth: b, ParentPID: os.Getpid(), ParentBirth: pb, Owner: owner, Session: session}
	f, e := os.CreateTemp(dir, "process-*.json")
	if e != nil {
		return nil, e
	}
	r.Path = f.Name()
	raw, _ := json.Marshal(r)
	_, e = f.Write(raw)
	if e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return nil, e
	}
	d, e := os.Open(dir)
	if e != nil {
		return nil, e
	}
	e = d.Sync()
	d.Close()
	if e != nil {
		return nil, e
	}
	return r, nil
}

// Start uses a fixed shell only as a stop-before-exec gate. All command arguments
// are positional; no caller text is evaluated as shell source.
func Start(cmd *exec.Cmd, dir, owner string) (*Record, error) {
	return StartLinked(cmd, dir, owner, "")
}
func StartLinked(cmd *exec.Cmd, dir, owner, session string) (*Record, error) {
	original := append([]string(nil), cmd.Args...)
	path := cmd.Path
	cmd.Path = "/bin/sh"
	cmd.Args = append([]string{"sh", "-c", "kill -STOP $$; exec \"$@\"", "fae-owned", path}, original[1:]...)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if e := cmd.Start(); e != nil {
		return nil, e
	}
	failed := true
	defer func() {
		if failed {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
		}
	}()
	deadline := time.Now().Add(3 * time.Second)
	for {
		out, e := exec.Command("ps", "-p", strconv.Itoa(cmd.Process.Pid), "-o", "stat=").Output()
		if e != nil {
			return nil, e
		}
		if strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return nil, errors.New("launch gate exited before registration")
		}
		if strings.Contains(string(out), "T") {
			break
		}
		if time.Now().After(deadline) {
			return nil, errors.New("launch gate timeout")
		}
		time.Sleep(time.Millisecond)
	}
	r, e := RegisterLinked(cmd.Process.Pid, dir, owner, session)
	if e != nil {
		return nil, e
	}
	r.Command = original
	r.Directory = cmd.Dir
	raw, _ := json.Marshal(r)
	f, e := os.OpenFile(r.Path, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	_, e = f.Write(raw)
	if e == nil {
		e = f.Sync()
	}
	f.Close()
	if e != nil {
		return nil, e
	}
	if e = Watch(r); e != nil {
		return nil, e
	}
	if e = syscall.Kill(r.PID, syscall.SIGCONT); e != nil {
		r.watch.Close()
		return nil, e
	}
	failed = false
	return r, nil
}
func groupPresent(pgid int) (bool, error) {
	out, e := exec.Command("ps", "-axo", "pgid=,stat=").Output()
	if e != nil {
		return false, e
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if len(f) < 2 {
			return false, errors.New("unreadable process census")
		}
		p, e := strconv.Atoi(f[0])
		if e != nil {
			return false, e
		}
		if p == pgid && !strings.HasPrefix(f[1], "Z") {
			return true, nil
		}
	}
	return false, nil
}
func terminate(ctx context.Context, r *Record, owner string) error {
	alive, e := groupPresent(r.PGID)
	if e != nil {
		return e
	}
	if !alive {
		e = retireProof(r)
		if os.IsNotExist(e) {
			return nil
		}
		return e
	}
	b, e := birth(r.PID)
	if e != nil {
		return fmt.Errorf("ambiguous group %d without leader: %w", r.PGID, e)
	}
	pg, e := syscall.Getpgid(r.PID)
	if e == syscall.ESRCH { // Darwin does not expose pgid for an unreaped zombie.
		dead, de := exited(r.PID)
		if de == nil && dead {
			pg = r.PGID
			e = nil
		}
	}
	if e != nil {
		return e
	}
	if !CanTerminate(r.Owner == owner, b == r.Birth, pg == r.PGID, true) {
		return errors.New("ownership mismatch; refusing signal")
	}
	if e = syscall.Kill(-r.PGID, syscall.SIGKILL); e != nil && e != syscall.ESRCH {
		// Darwin may report EPERM when the last live group member exits between
		// census and signal. Only a fresh empty census authorizes retirement.
		present, probeErr := groupPresent(r.PGID)
		if probeErr == nil && !present {
			return retireProof(r)
		}
		return fmt.Errorf("signal owned group %d: %w", r.PGID, e)
	}
	for {
		alive, e = groupPresent(r.PGID)
		if e != nil {
			return e
		}
		if !alive {
			e = retireProof(r)
			if os.IsNotExist(e) {
				return nil
			}
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Recover must run under the daemon's instance lock, before any new launches.
// A live original parent is never disturbed. A single ambiguous record blocks it.
func Recover(ctx context.Context, dir, owner string) ([]Record, error) {
	if _, e := os.Stat(dir); os.IsNotExist(e) {
		return nil, nil
	}
	if e := privateDir(dir); e != nil {
		return nil, e
	}
	entries, e := filepath.Glob(filepath.Join(dir, "process-*.json"))
	if e != nil {
		return nil, e
	}
	retired, _ := filepath.Glob(filepath.Join(dir, "process-*.json.retired"))
	entries = append(entries, retired...)
	var recovered []Record
	for _, p := range entries {
		r, e := read(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return recovered, e
		}
		if r.Owner != owner {
			return recovered, errors.New("registry owner mismatch")
		}
		if strings.HasSuffix(p, ".retired") {
			recovered = append(recovered, *r)
			continue
		}
		pb, e := birth(r.ParentPID)
		if e != nil && syscall.Kill(r.ParentPID, 0) != syscall.ESRCH {
			return recovered, errors.New("cannot establish original owner death")
		}
		if e == nil && pb == r.ParentBirth {
			return recovered, errors.New("original process owner still alive")
		}
		if !strings.HasSuffix(p, ".retired") {
			if e = terminate(ctx, r, owner); e != nil {
				return recovered, e
			}
		}
		recovered = append(recovered, *r)
	}
	return recovered, nil
}

// Retire closes the watcher only after group death has been observed. An escaped
// session or an ambiguous surviving group does not authorize lease reclamation.
func Retire(r *Record) error {
	if r == nil {
		return nil
	}
	if r.watch != nil {
		defer r.watch.Close()
	}
	alive, e := groupPresent(r.PGID)
	if e != nil {
		return errors.Join(ErrRetirementUnproven, e)
	}
	if alive {
		return errors.Join(ErrRetirementUnproven, errors.New("owned group still alive"))
	}
	if r.Session != "" {
		if _, e := os.Stat(r.Path); os.IsNotExist(e) {
			if _, e = os.Stat(r.Path + ".retired"); os.IsNotExist(e) {
				return nil
			}
		}
		return retireProof(r)
	}
	e = os.Remove(r.Path)
	if e != nil && !os.IsNotExist(e) {
		return errors.Join(ErrRetirementUnproven, e)
	}
	e = os.Remove(r.Path + ".retired")
	if os.IsNotExist(e) {
		return nil
	}
	return e
}

// Terminate retires a registered group only while its exact leader identity
// remains provable. It never signals a PID-reused or leaderless ambiguous group.
func Terminate(ctx context.Context, r *Record, owner string) error {
	if r == nil {
		return errors.New("record required")
	}
	if r.watch != nil {
		defer r.watch.Close()
	}
	return terminate(ctx, r, owner)
}

// Keep an immutable retirement receipt so a watchdog cleanup can be audited
// after the database is reopened. Recovery is idempotent over these receipts.
func retireProof(r *Record) error {
	if strings.HasSuffix(r.Path, ".retired") {
		return nil
	}
	if e := os.Rename(r.Path, r.Path+".retired"); e != nil {
		if !os.IsNotExist(e) {
			return e
		}
		existing, readErr := read(r.Path + ".retired")
		if readErr != nil {
			return readErr
		}
		if existing.PID != r.PID || existing.Birth != r.Birth || existing.Owner != r.Owner || existing.Session != r.Session || existing.ParentPID != r.ParentPID || existing.ParentBirth != r.ParentBirth {
			return errors.New("retirement receipt identity mismatch")
		}
	}
	d, e := os.Open(filepath.Dir(r.Path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}

// Watch arms parent-death cleanup for an independently gated registered group.
func Watch(r *Record) error {
	if r == nil || r.watch != nil {
		return errors.New("record missing or already watched")
	}
	rd, wr, e := os.Pipe()
	if e != nil {
		return e
	}
	exe, e := os.Executable()
	if e != nil {
		rd.Close()
		wr.Close()
		return e
	}
	w := exec.Command(exe, "--fae-internal-process-watch", r.Path)
	w.Stdin = rd
	w.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e = w.Start(); e != nil {
		rd.Close()
		wr.Close()
		return e
	}
	rd.Close()
	go func() { _ = w.Wait() }()
	r.watch = wr
	return nil
}

// Wait preserves the unreaped direct-child identity while retiring descendants.
// Unlike post-Wait kill(-pid), this cannot target a group whose leader PID has
// been recycled: no caller may concurrently reap cmd. A zombie leader remains
// available to the OS identity probe until this function finally calls cmd.Wait.
func Wait(cmd *exec.Cmd, r *Record) error {
	if cmd == nil || cmd.Process == nil || r == nil || cmd.Process.Pid != r.PID {
		return errors.New("owned child required")
	}
	for {
		dead, e := exited(r.PID)
		if e != nil { // Keep fail-closed cleanup failures visible to lease admission.
			_ = cmd.Process.Kill()
			waitErr := cmd.Wait()
			return errors.Join(fmt.Errorf("owned child status unavailable: %w", e), waitErr, Retire(r))
		}
		if dead {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			cleanupErr := Terminate(ctx, r, r.Owner)
			cancel()
			waitErr := cmd.Wait()
			if cleanupErr == nil {
				if re := Retire(r); re != nil {
					return errors.Join(waitErr, re)
				}
				return waitErr
			}
			return errors.Join(waitErr, fmt.Errorf("retire owned group %d: %w", r.PGID, cleanupErr))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Birth identifies a process using the host's exact kernel start identity.
func Birth(pid int) (string, error) { return birth(pid) }

// ProvedDead distinguishes unavailable observation from an absent/replaced owner.
func ProvedDead(pid int, recorded string) bool {
	if pid <= 1 || recorded == "" {
		return false
	}
	actual, e := birth(pid)
	if e == nil {
		return actual != recorded
	}
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

// LinkedRetirement requires a complete exact set of durable terminal receipts.
// It assumes the documented trusted-workload no-detach contract; a process group
// is not a hostile-code containment boundary. Missing/extra/foreign proofs fail.
func LinkedRetirement(dir, owner, session string, parent int, parentBirth string, expected int) (bool, error) {
	if expected <= 0 || session == "" {
		return false, nil
	}
	if _, e := os.Stat(dir); os.IsNotExist(e) {
		return false, nil
	}
	if e := privateDir(dir); e != nil {
		return false, e
	}
	paths, e := filepath.Glob(filepath.Join(dir, "process-*.json*"))
	if e != nil {
		return false, e
	}
	count := 0
	seen := map[string]bool{}
	for _, path := range paths {
		r, e := read(path)
		if e != nil {
			return false, e
		}
		if r.Session != session {
			continue
		}
		if !strings.HasSuffix(path, ".retired") || r.Owner != owner || r.ParentPID != parent || r.ParentBirth != parentBirth {
			return false, nil
		}
		key := fmt.Sprintf("%d/%s", r.PID, r.Birth)
		if seen[key] {
			return false, nil
		}
		seen[key] = true
		count++
	}
	return count == expected, nil
}

// AcknowledgeLinkedRetirement removes terminal receipts only after the caller
// has durably checkpointed their retirement in the owning lease journal. It is
// idempotent after partial cleanup. Active or foreign same-session records fail
// closed; no signaling or inference from absent records occurs here.
func AcknowledgeLinkedRetirement(dir, owner, session string, parent int, parentBirth string) error {
	if session == "" || parent <= 1 || parentBirth == "" {
		return errors.New("exact linked retirement identity required")
	}
	if _, e := os.Stat(dir); os.IsNotExist(e) {
		return nil
	}
	if e := privateDir(dir); e != nil {
		return e
	}
	paths, e := filepath.Glob(filepath.Join(dir, "process-*.json*"))
	if e != nil {
		return e
	}
	var receipts []string
	for _, path := range paths {
		r, e := read(path)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if r.Session != session {
			continue
		}
		if r.ParentPID != parent || r.ParentBirth != parentBirth {
			continue
		}
		if r.Owner != owner || !strings.HasSuffix(path, ".retired") {
			return errors.New("unacknowledged active or foreign linked process")
		}
		receipts = append(receipts, path)
	}
	for _, path := range receipts {
		if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
