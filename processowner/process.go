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

// Record is one inventory entry (FAE-HOST-021): pid, kernel start identity,
// parent identity and the process's role in the instance (dolt, agent-session,
// agent-pane, multiplexer-server, host-command, ui; empty for legacy records).
type Record struct {
	PID, PGID, ParentPID                 int
	Birth, ParentBirth, Owner, Directory string
	Session                              string
	Role                                 string `json:",omitempty"`
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
			e = terminate(ctx, r, r.Owner, 0)
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

// recordReadLimit bounds an ownership record read back from the registry.
// New records keep a bounded command (recordCommand), but a record written
// before that holds its whole argument vector, up to the kernel's ARG_MAX;
// recovery and retirement must still read it to prove its processes.
const recordReadLimit = 4 << 20

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
	if !s.Mode().IsRegular() || s.Mode().Perm() != 0600 || s.Size() > recordReadLimit {
		return nil, errors.New("invalid ownership record")
	}
	var r Record
	e = json.NewDecoder(io.LimitReader(f, recordReadLimit+1)).Decode(&r)
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
	return RegisterRole(pid, dir, owner, session, "")
}

// RegisterRole is RegisterLinked recording the process's instance role.
func RegisterRole(pid int, dir, owner, session, role string) (*Record, error) {
	return registerRecord(pid, dir, owner, session, role, nil, "")
}

// registerRecord publishes a group leader's record once, whole: StartRole
// passes the command and directory here rather than rewriting the record
// after it is published, so no registry scan can see it empty or partial.
func registerRecord(pid int, dir, owner, session, role string, command []string, directory string) (*Record, error) {
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
	r := &Record{PID: pid, PGID: pg, Birth: b, ParentPID: os.Getpid(), ParentBirth: pb, Owner: owner, Session: session, Role: role, Command: command, Directory: directory}
	if r.Path, e = publish(dir, r); e != nil {
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

// publish writes a record under a pending name that no registry scan
// matches, then gives it its final name, so a concurrent scan (another
// session's retirement proof, recovery, the inventory) reads it whole or not
// at all. Every record reaches its final name only here, already written and
// synced; nothing rewrites it in place afterwards (retirement renames it).
// The pending name carries the publisher's pid, so the sweep in Recover can
// tell a crashed publisher's leftover from a live publisher's file. The final
// name never replaces an existing record: see placeRecord.
func publish(dir string, r *Record) (string, error) {
	raw, _ := json.Marshal(r)
	for {
		f, e := os.CreateTemp(dir, fmt.Sprintf("%s%d-*.json", pendingRecordPrefix, os.Getpid()))
		if e != nil {
			return "", e
		}
		pending := f.Name()
		if _, e = f.Write(raw); e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			_ = os.Remove(pending)
			return "", e
		}
		path := filepath.Join(dir, strings.TrimPrefix(filepath.Base(pending), ".pending-"))
		if e = placeRecord(pending, path); errors.Is(e, os.ErrExist) {
			continue
		}
		if e != nil {
			return "", e
		}
		return path, nil
	}
}

// pendingRecordPrefix names a record that publish has not yet placed.
const pendingRecordPrefix = ".pending-process-"

// linkRecord is os.Link; tests replace it to stand in for a registry
// filesystem without hard links.
var linkRecord = os.Link

// placeRecord gives a written pending record its final name without ever
// replacing an existing record. It hard-links the pending file into place;
// where the registry's filesystem has no hard links (FAT and exFAT, some FUSE
// and SMB mounts) it falls back to a rename that refuses an existing target
// (Linux renameat2 RENAME_NOREPLACE, Darwin renamex_np RENAME_EXCL). Either
// way an existing name is reported as os.ErrExist, and the pending file is
// gone once the record is placed. A filesystem with neither fails the
// launch with an error that says so.
func placeRecord(pending, path string) error {
	linkErr := linkRecord(pending, path)
	if linkErr == nil {
		_ = os.Remove(pending)
		return nil
	}
	if errors.Is(linkErr, os.ErrExist) {
		_ = os.Remove(pending)
		return linkErr
	}
	renameErr := renameNoReplace(pending, path)
	if renameErr == nil {
		return nil
	}
	_ = os.Remove(pending)
	if errors.Is(renameErr, os.ErrExist) {
		return renameErr
	}
	return fmt.Errorf("publish process record %s: neither a hard link nor a no-replace rename placed it (the process registry's filesystem must support one): %w", path, errors.Join(linkErr, renameErr))
}

// GateError reports a started launch whose stop-before-exec gate never
// released the workload. Retired is positive proof that the gate group was
// killed, reaped and is absent, with no active record left behind.
type GateError struct {
	Cause   error
	Retired bool
}

func (e *GateError) Error() string { return e.Cause.Error() }
func (e *GateError) Unwrap() error { return e.Cause }

// Start uses a fixed shell only as a stop-before-exec gate. All command arguments
// are positional; no caller text is evaluated as shell source.
func Start(cmd *exec.Cmd, dir, owner string) (*Record, error) {
	return StartLinked(cmd, dir, owner, "")
}
func StartLinked(cmd *exec.Cmd, dir, owner, session string) (*Record, error) {
	return StartRole(cmd, dir, owner, session, "")
}

// StartRole is StartLinked recording the process's instance role. The child's
// exit status is explicitly owned by its waiter: the reaper never collects it.
func StartRole(cmd *exec.Cmd, dir, owner, session, role string) (_ *Record, err error) {
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
	var registered *Record
	defer func() {
		if err == nil {
			return
		}
		// SIGCONT was never delivered, so the gate never exec'd the workload.
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
		alive, probe := groupPresent(cmd.Process.Pid)
		retired := probe == nil && !alive
		if retired && registered != nil {
			retired = Retire(registered) == nil
		}
		err = &GateError{Cause: err, Retired: retired}
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
	// The record is published once, with its command and directory: a
	// rewrite under the final name would let a concurrent scan read it empty.
	r, e := registerRecord(cmd.Process.Pid, dir, owner, session, role, recordCommand(original), cmd.Dir)
	if e != nil {
		return nil, e
	}
	own(r.PID, r.Birth)
	registered = r
	if e = Watch(r); e != nil {
		return nil, e
	}
	if e = syscall.Kill(r.PID, syscall.SIGCONT); e != nil {
		return nil, e
	}
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

// terminate signals an owned group: with a grace, SIGTERM first and SIGKILL
// only if the group outlives it (FAE-CP-002/-003's terminate-then-kill); a
// group cannot be reused while any member remains, so the second signal
// needs no new identity proof.
func terminate(ctx context.Context, r *Record, owner string, grace time.Duration) error {
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
	signal := syscall.SIGKILL
	if grace > 0 {
		signal = syscall.SIGTERM
	}
	forceAt := time.Now().Add(grace)
	if e = syscall.Kill(-r.PGID, signal); e != nil && e != syscall.ESRCH {
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
		if signal == syscall.SIGTERM && !time.Now().Before(forceAt) {
			signal = syscall.SIGKILL
			if e = syscall.Kill(-r.PGID, signal); e != nil && e != syscall.ESRCH {
				return fmt.Errorf("force owned group %d: %w", r.PGID, e)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Recover must run under the daemon's instance lock, before any new launches.
// A live original parent is never disturbed. A single ambiguous record blocks
// it, and the refusal names the record's file. An empty record an earlier
// process left is removed first (clearEmptyRecord); RecoverClearing reports
// each one so the caller can log and audit it. An optional grace terminates
// each orphan with SIGTERM before SIGKILL. It then sweeps the pending files
// of crashed publishers (sweepPending).
func Recover(ctx context.Context, dir, owner string, grace ...time.Duration) ([]Record, error) {
	recovered, _, e := RecoverClearing(ctx, dir, owner, grace...)
	return recovered, e
}

// RecoverClearing is Recover that also returns the path of every empty
// record it removed, even when it then refuses another record, so that no
// removal goes unreported (FAE-ROB-014): the daemon writes a log line and an
// audit row for each.
func RecoverClearing(ctx context.Context, dir, owner string, grace ...time.Duration) (recovered []Record, cleared []string, err error) {
	recovered, err = recoverRegistry(ctx, dir, owner, &cleared, grace...)
	return recovered, cleared, err
}

func recoverRegistry(ctx context.Context, dir, owner string, cleared *[]string, grace ...time.Duration) ([]Record, error) {
	wait := time.Duration(0)
	if len(grace) > 0 {
		wait = grace[0]
	}
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
			removed, clearErr := clearEmptyRecord(p)
			if clearErr != nil {
				return recovered, fmt.Errorf("process record %s is empty and could not be removed: %w", p, clearErr)
			}
			if removed {
				*cleared = append(*cleared, p)
				continue
			}
			return recovered, unreadableRecord(p, e)
		}
		if r.Owner != owner {
			return recovered, fmt.Errorf("process record %s: registry owner mismatch", p)
		}
		if strings.HasSuffix(p, ".retired") {
			recovered = append(recovered, *r)
			continue
		}
		pb, e := birth(r.ParentPID)
		if e != nil && syscall.Kill(r.ParentPID, 0) != syscall.ESRCH {
			return recovered, fmt.Errorf("process record %s: cannot establish original owner death", p)
		}
		if e == nil && pb == r.ParentBirth {
			return recovered, fmt.Errorf("process record %s: original process owner still alive", p)
		}
		if !strings.HasSuffix(p, ".retired") {
			if e = terminate(ctx, r, owner, wait); e != nil {
				return recovered, e
			}
		}
		recovered = append(recovered, *r)
	}
	return recovered, sweepPending(ctx, dir, owner, wait)
}

// recoverProcessStarted is about when this process started: package
// variables are initialized before main runs. clearEmptyRecord removes only
// an empty record last written at least emptyRecordClockSlack before it.
var recoverProcessStarted = time.Now()

// emptyRecordClockSlack covers the coarse clock file times come from: Linux
// stamps a file from a clock that advances once per scheduler tick (1 to 10
// ms), so a file this process wrote just after recoverProcessStarted can carry
// an earlier time (re-audit 9 measured up to 1.09 ms). A record written in
// the last 50 ms before this process started is refused like a newer one; an
// earlier process can only have left it in the moment before it released
// the instance lock, so a later start removes it.
const emptyRecordClockSlack = 50 * time.Millisecond

// clearEmptyRecord removes an empty record, active or retired, that an
// earlier process left, and reports whether it did. Before publish placed
// records whole, StartRole rewrote a published record in place (truncate,
// then write), so a crash between the two left an empty process-*.json, and a
// failed rewrite that was then retired left an empty receipt. Such a file
// names no process: nothing can be proved alive or dead from it, and keeping
// it only blocks every start and every retirement scan of the registry.
// Removing it signals nothing, and it releases no lease, since a lease needs
// a complete set of matching receipts (LinkedRetirement) and an empty file is
// never one. Recover runs under the instance lock, and every record is now
// placed whole, so the file is removed only when it is a private regular
// file (mode 0600, not a symlink), empty, and last written before this
// process started, by more than the file clock's slack
// (emptyRecordClockSlack); anything else is refused with its path.
func clearEmptyRecord(path string) (bool, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return false, nil
	}
	st, e := f.Stat()
	f.Close()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 || st.Size() != 0 || !st.ModTime().Before(recoverProcessStarted.Add(-emptyRecordClockSlack)) {
		return false, nil
	}
	if e = os.Remove(path); e != nil && !os.IsNotExist(e) {
		return false, e
	}
	return true, nil
}

// unreadableRecord is Recover's refusal of a record it cannot read: it names
// the file and what the operator can do about it.
func unreadableRecord(path string, cause error) error {
	return fmt.Errorf("process record %s cannot be read (%v): it may name a process this instance started, so recovery will not guess; check that no process it names (pid and group, when the file decodes) is still running, then move the file out of the registry and start again", path, cause)
}

// sweepPending removes the pending files that publishers left when they
// crashed before placing their record (publish). Registries are shared by
// the sessions of one instance, so a pending file is left alone until its
// publisher is proved dead: a whole pending record names its publisher's
// exact identity (the record's parent), and a partial one only the
// publisher's pid, in its file name, so it goes once no process has that pid.
// A whole pending record of this owner names a launch gate that was stopped
// before its workload could run and was never placed in the inventory; while
// that exact gate exists it is terminated under the same identity proof as
// an active record. Pending files of other owners, of live or unprovable
// publishers, and partial ones without a pid in the name (written before
// pending names carried one) are kept; they are never read as records.
// Swept gates are not returned as recovered records: no lease or receipt
// ever counted them.
func sweepPending(ctx context.Context, dir, owner string, grace time.Duration) error {
	paths, e := filepath.Glob(filepath.Join(dir, pendingRecordPrefix+"*.json"))
	if e != nil {
		return e
	}
	for _, p := range paths {
		r, e := read(p)
		switch {
		case os.IsNotExist(e):
			continue
		case e == nil:
			if r.Owner != owner || !ProvedDead(r.ParentPID, r.ParentBirth) {
				continue
			}
			if !ProvedDead(r.PID, r.Birth) {
				if e = terminate(ctx, r, owner, grace); e != nil {
					return fmt.Errorf("stopped launch gate of %s: %w", p, e)
				}
				// terminate keeps a retirement receipt under the pending name.
				if e = os.Remove(p + ".retired"); e != nil && !os.IsNotExist(e) {
					return e
				}
			}
		default:
			pid, named := pendingPublisher(p)
			if !named || syscall.Kill(pid, 0) != syscall.ESRCH {
				continue
			}
		}
		if e = os.Remove(p); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	return nil
}

// pendingPublisher is the publisher pid in a pending file's name.
func pendingPublisher(path string) (int, bool) {
	name := strings.TrimPrefix(filepath.Base(path), pendingRecordPrefix)
	pid, _, found := strings.Cut(name, "-")
	n, e := strconv.Atoi(pid)
	return n, found && e == nil && n > 1
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
	return terminate(ctx, r, owner, 0)
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
// recordCommandLimit bounds the JSON-encoded command an ownership record
// keeps. The command is only informational; a long vector, such as gofmt -l
// over a large tree, keeps its leading arguments and a count of the rest.
const recordCommandLimit = 16 << 10

func recordCommand(argv []string) []string {
	out, used := []string{}, 0
	for i, arg := range argv {
		encoded, _ := json.Marshal(arg)
		if used+len(encoded) > recordCommandLimit {
			return append(out, fmt.Sprintf("... %d more arguments", len(argv)-i))
		}
		out, used = append(out, arg), used+len(encoded)
	}
	return out
}

func Wait(cmd *exec.Cmd, r *Record) error {
	if cmd == nil || cmd.Process == nil || r == nil || cmd.Process.Pid != r.PID {
		return errors.New("owned child required")
	}
	defer disown(r.PID)
	blocking := true // fall back to polling where waitid is unavailable
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
		if blocking {
			blocking = awaitExit(r.PID) == nil
		} else {
			time.Sleep(10 * time.Millisecond)
		}
	}
}

var errUnsupportedWait = errors.New("blocking unreaped wait unsupported")

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
		if os.IsNotExist(e) {
			// Retired or acknowledged since the listing: not read at all.
			continue
		}
		if e != nil {
			if otherSession(path, session, 0, "") {
				continue
			}
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
			if otherSession(path, session, parent, parentBirth) {
				continue
			}
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

// otherSession reports whether a record that failed validation (wrong mode,
// invalid identity) still names, in JSON that decodes, another session, or
// with a parent given, another parent than the caller's. Such a record cannot
// be one of the caller's linked processes, so it neither blocks nor counts
// toward the caller's retirement proof. A record that does not decode, such
// as an empty or truncated file, might be the caller's own active record, so
// the scans fail closed on it. Records are only ever placed whole (publish),
// so a malformed record means damage, not a write in progress.
func otherSession(path, session string, parent int, parentBirth string) bool {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return false
	}
	defer f.Close()
	s, e := f.Stat()
	if e != nil || !s.Mode().IsRegular() || s.Size() > recordReadLimit {
		return false
	}
	var names struct {
		Session     *string
		ParentPID   int
		ParentBirth string
	}
	if json.NewDecoder(io.LimitReader(f, recordReadLimit+1)).Decode(&names) != nil || names.Session == nil {
		return false
	}
	if *names.Session != session {
		return true
	}
	return parent > 0 && (names.ParentPID != parent || names.ParentBirth != parentBirth)
}

// Inventory lists the live (unretired) records of a registry: the instance's
// owned processes with their identities and roles (FAE-HOST-021).
func Inventory(dir string) ([]Record, error) {
	if _, e := os.Stat(dir); os.IsNotExist(e) {
		return nil, nil
	}
	if e := privateDir(dir); e != nil {
		return nil, e
	}
	paths, e := filepath.Glob(filepath.Join(dir, "process-*.json"))
	if e != nil {
		return nil, e
	}
	var out []Record
	for _, p := range paths {
		r, e := read(p)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return out, e
		}
		out = append(out, *r)
	}
	return out, nil
}
