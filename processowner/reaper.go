package processowner

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// ReaperGrace bounds how long an unclaimed defunct child may stay unreaped.
// The daemon's own unowned forks are waited by exec.Cmd, which reaps at exit
// (Wait collects the process before draining output); a zombie unclaimed this
// long was adopted.
const ReaperGrace = 5 * time.Second

// FAE-HOST-021/-023: the daemon is the child subreaper of its descendants, so
// an orphan stays in the instance tree and is re-parented to the daemon, which
// must reap it. Exit statuses of the daemon's own forks belong to their waiters:
// children started by StartRole are explicitly owned (processowner.Wait) and
// never collected here. Fae never forks a child into a foreign process group or
// a new session, so a defunct non-leader of a foreign group is adopted and
// reaped at once; any other unowned defunct child (an orphan in the daemon's
// own group or a daemonized session leader, indistinguishable from an own fork
// at a glance) is reaped once it has stayed unclaimed for ReaperGrace.
var reaper = struct {
	sync.Mutex
	owned   map[int]string
	defunct map[string]time.Time
	started bool
	grace   time.Duration
	reaped  int
}{owned: map[int]string{}, defunct: map[string]time.Time{}}

func own(pid int, birth string) {
	reaper.Lock()
	reaper.owned[pid] = birth
	reaper.Unlock()
}
func disown(pid int) {
	reaper.Lock()
	delete(reaper.owned, pid)
	reaper.Unlock()
}

func getsid() int {
	sid, _, _ := syscall.RawSyscall(syscall.SYS_GETSID, 0, 0, 0)
	return int(sid)
}

func censusChildren() ([]Process, error) {
	all, err := Census()
	if err != nil {
		return nil, err
	}
	var out []Process
	for _, p := range all {
		if p.PPID == os.Getpid() {
			out = append(out, p)
		}
	}
	return out, nil
}

// childRule classifies a direct child: adopted (a foreign group's non-leader)
// or leader (a session leader of another session). Neither is an own fork with
// an unchanged group and session.
func childRule(c Process, selfPGID, selfSID int) (adopted, leader bool) {
	adopted = c.PGID != c.PID && c.PGID != selfPGID
	leader = c.SID == c.PID && c.SID != selfSID
	return adopted, leader
}

// StartReaper makes this process the child subreaper and reaps adopted defunct
// children on every SIGCHLD and at grace deadlines; it never polls. It is
// process-wide and idempotent; it fails where subreapers are unsupported.
func StartReaper(grace time.Duration) error {
	reaper.Lock()
	defer reaper.Unlock()
	if reaper.started {
		return nil
	}
	if err := setSubreaper(); err != nil {
		return err
	}
	reaper.started, reaper.grace = true, grace
	signals := make(chan os.Signal, 16)
	signal.Notify(signals, syscall.SIGCHLD)
	go func() {
		timer := time.NewTimer(time.Hour)
		timer.Stop()
		for {
			if next, _ := ReapAdopted(time.Now()); !next.IsZero() {
				timer.Reset(time.Until(next))
			}
			select {
			case <-signals:
			case <-timer.C:
			}
		}
	}()
	return nil
}

// ReapAdopted collects adopted defunct children and returns the next grace
// deadline of a pending unclaimed child (zero when none is pending).
func ReapAdopted(now time.Time) (time.Time, error) {
	kids, err := directChildren()
	if err != nil {
		return time.Time{}, err
	}
	selfPGID, selfSID := syscall.Getpgrp(), getsid()
	reaper.Lock()
	defer reaper.Unlock()
	seen := map[string]bool{}
	var next time.Time
	for _, c := range kids {
		if !c.Defunct() || reaper.owned[c.PID] == c.Start {
			continue
		}
		adopted, _ := childRule(c, selfPGID, selfSID)
		key := strconv.Itoa(c.PID) + "/" + c.Start
		if !adopted {
			seen[key] = true
			first, ok := reaper.defunct[key]
			if !ok {
				first = now
				reaper.defunct[key] = now
			}
			if deadline := first.Add(reaper.grace); now.Before(deadline) {
				if next.IsZero() || deadline.Before(next) {
					next = deadline
				}
				continue
			}
		}
		var status syscall.WaitStatus
		if pid, e := syscall.Wait4(c.PID, &status, syscall.WNOHANG, nil); e == nil && pid == c.PID {
			reaper.reaped++
		}
	}
	for key := range reaper.defunct {
		if !seen[key] {
			delete(reaper.defunct, key)
		}
	}
	return next, nil
}

// KillAdopted is shutdown's last step: it kills and reaps every adopted child
// (re-parented descendants of the instance tree), so no inventory process
// survives the daemon. Own forks and explicitly owned children are untouched;
// signalling an unreaped child cannot hit a recycled PID.
func KillAdopted(ctx context.Context) (int, error) {
	kids, err := directChildren()
	if err != nil {
		return 0, err
	}
	selfPGID, selfSID := syscall.Getpgrp(), getsid()
	killed := 0
	var errs []error
	for _, c := range kids {
		reaper.Lock()
		owned := reaper.owned[c.PID] == c.Start
		reaper.Unlock()
		if adopted, leader := childRule(c, selfPGID, selfSID); owned || !adopted && !leader {
			continue
		}
		if !c.Defunct() {
			if e := syscall.Kill(c.PID, syscall.SIGKILL); e != nil && e != syscall.ESRCH {
				errs = append(errs, e)
				continue
			}
			killed++
		}
		for {
			var status syscall.WaitStatus
			pid, e := syscall.Wait4(c.PID, &status, syscall.WNOHANG, nil)
			if pid == c.PID || e != nil {
				break
			}
			select {
			case <-ctx.Done():
				return killed, ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	return killed, errors.Join(errs...)
}
