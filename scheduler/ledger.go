package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Ledger is a host-shared, private durable heavy-operation pool. All cooperating
// instances must use the same directory. The stable lock is never renamed; pool
// and audit history are published together using fsync+rename under flock.
type Ledger struct {
	path     string
	capacity int
	dynamic  bool
}
type LeaseAudit struct {
	At                 time.Time
	Action, ID, Reason string
	Owner              Owner
	Events             []Event
}
type ledgerState struct {
	Readers             map[string]LedgerReader `json:",omitempty"`
	WorkflowSlots       *WorkflowSlotState      `json:",omitempty"`
	Version             int
	Pool                Pool
	ProviderPools       map[string]Pool               `json:",omitempty"`
	ProviderPoolConfigs map[string]ProviderPoolConfig `json:",omitempty"`
	ProviderPoolQuotas  map[string]ProviderPoolQuota  `json:",omitempty"`
	ProviderPoolPauses  map[string]ProviderPoolPause  `json:",omitempty"`
	Audit               []LeaseAudit
	Panes               []PaneReservation          `json:",omitempty"`
	PaneLimits          map[string]PaneLimitRecord `json:",omitempty"`
	HostLimits          map[string]HostLimitRecord `json:",omitempty"`
	Launches            []LaunchReservation        `json:",omitempty"`
}

func OpenLedger(path string, capacity int) (*Ledger, error) { return openLedger(path, capacity, false) }
func openLedger(path string, capacity int, dynamic bool) (*Ledger, error) {
	return openLedgerContext(context.Background(), path, capacity, dynamic)
}
func openLedgerContext(initialContext context.Context, path string, capacity int, dynamic bool) (*Ledger, error) {
	if !filepath.IsAbs(path) || capacity <= 0 {
		return nil, errors.New("absolute host ledger path and positive capacity required")
	}
	path = filepath.Clean(path)
	for p := path; ; p = filepath.Dir(p) {
		info, e := os.Lstat(p)
		if e == nil && info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("ledger symlink forbidden")
		}
		if e != nil && !os.IsNotExist(e) {
			return nil, e
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	if e := os.MkdirAll(path, 0700); e != nil {
		return nil, e
	}
	info, e := os.Stat(path)
	if e != nil {
		return nil, e
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("ledger directory must have mode 0700")
	}
	l := &Ledger{path: path, capacity: capacity, dynamic: dynamic}
	e = l.transact(initialContext, true, func(s *ledgerState) error { return nil })
	return l, e
}
func (l *Ledger) transact(ctx context.Context, write bool, fn func(*ledgerState) error) error {
	lock, fresh, e := openLedgerLock(l.path)
	if e != nil {
		return e
	}
	fd := int(lock.Fd())
	defer lock.Close()
	if info, e := lock.Stat(); e != nil {
		return e
	} else if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("ledger lock must be regular and private")
	}
	for {
		e = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			break
		}
		if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
			return e
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	state := ledgerState{Version: 1}
	state.Pool, _ = func() (Pool, error) {
		p, e := NewPool(l.capacity)
		if e != nil {
			return Pool{}, e
		}
		return *p, nil
	}()
	statePath := filepath.Join(l.path, "state.json")
	fd, e = syscall.Open(statePath, syscall.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e == nil {
		f := os.NewFile(uintptr(fd), "ledger state")
		b, readErr := io.ReadAll(io.LimitReader(f, 64<<20))
		info, statErr := f.Stat()
		f.Close()
		if readErr != nil {
			return readErr
		}
		if statErr != nil {
			return statErr
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return errors.New("ledger state must be regular and private")
		}
		if info.Size() >= 64<<20 {
			return errors.New("lease ledger exceeds size bound")
		}
		if e = json.Unmarshal(b, &state); e != nil {
			return fmt.Errorf("unreadable lease ledger: %w", e)
		}
	} else if !os.IsNotExist(e) {
		return e
	} else if !write || !fresh {
		return errors.New("lease ledger missing")
	}
	if (state.Version != 1 && state.Version != 2) || (state.Version == 2 && state.WorkflowSlots == nil) || (!l.dynamic && state.Pool.Capacity != l.capacity) {
		return errors.New("lease ledger version/capacity conflict")
	}
	if len(state.HostLimits) > 0 {
		if _, _, err := reconcileHost(&state); err != nil {
			return err
		}
	}
	if e = reconcileWorkflowSlots(&state); e != nil {
		return e
	}
	if e = state.Pool.Validate(); e != nil {
		return e
	}
	for _, pp := range state.ProviderPools {
		if e = pp.Validate(); e != nil {
			return e
		}
	}
	if write {
		recordLedgerReader(&state)
	}
	if e = fn(&state); e != nil {
		return e
	}
	if !write {
		return nil
	}
	if state.Version == 1 && state.WorkflowSlots != nil {
		if e := upgradeWorkflowSchema(&state); e != nil {
			return e
		}
	}
	b, e := json.Marshal(state)
	if e != nil {
		return e
	}
	if len(b) >= 64<<20 {
		return errors.New("lease audit history requires archival; ledger full")
	}
	temp, e := os.CreateTemp(l.path, ".state-*")
	if e != nil {
		return e
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, e = temp.Write(b); e != nil {
		temp.Close()
		return e
	}
	if e = temp.Sync(); e != nil {
		temp.Close()
		return e
	}
	if e = temp.Close(); e != nil {
		return e
	}
	if e = os.Rename(name, statePath); e != nil {
		return e
	}
	dir, e := os.Open(l.path)
	if e != nil {
		return e
	}
	defer dir.Close()
	return dir.Sync()
}
func (l *Ledger) Enqueue(ctx context.Context, r Request) error {
	return l.transact(ctx, true, func(s *ledgerState) error {
		for _, w := range s.Pool.Waiting {
			if w.ID == r.ID {
				if w.Owner == r.Owner && w.Weight == r.Weight && w.Class == r.Class && w.Priority == r.Priority && w.Queued.Equal(r.Queued) {
					return nil
				}
				return errors.New("request identity conflict")
			}
		}
		for _, h := range s.Pool.Holders {
			if h.ID == r.ID {
				if h.Owner == r.Owner && h.Weight == r.Weight && h.Class == r.Class && h.Priority == r.Priority && h.Queued.Equal(r.Queued) {
					return nil
				}
				return errors.New("request identity conflict")
			}
		}
		if e := s.Pool.Enqueue(r); e != nil {
			return e
		}
		s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "queued", ID: r.ID, Owner: r.Owner})
		return nil
	})
}
func (l *Ledger) Advance(ctx context.Context, now time.Time, observed map[Owner]Liveness) ([]Lease, []Event, error) {
	var granted []Lease
	var events []Event
	e := l.transact(ctx, true, func(s *ledgerState) error {
		var e error
		granted, events, e = s.Pool.AdvanceObserved(now, observed)
		if e != nil {
			return e
		}
		for _, g := range granted {
			s.Audit = append(s.Audit, LeaseAudit{At: now, Action: "granted", ID: g.ID, Owner: g.Owner})
		}
		if len(events) > 0 {
			s.Audit = append(s.Audit, LeaseAudit{At: now, Action: "advance", Events: events})
		}
		return nil
	})
	if e != nil {
		return nil, nil, e
	}
	return granted, events, nil
}
func (l *Ledger) Release(ctx context.Context, id string, owner Owner, reason string) error {
	return l.transact(ctx, true, func(s *ledgerState) error {
		if reason == "" {
			return errors.New("release reason required")
		}
		if !s.Pool.Release(id, owner) {
			return errors.New("lease not held by owner")
		}
		s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "released", ID: id, Owner: owner, Reason: reason})
		return nil
	})
}
func (l *Ledger) Snapshot(ctx context.Context) (Pool, error) {
	var p Pool
	e := l.transact(ctx, false, func(s *ledgerState) error { p = s.Pool; return nil })
	return p, e
}
func (l *Ledger) Audit(ctx context.Context) ([]LeaseAudit, error) {
	var a []LeaseAudit
	e := l.transact(ctx, false, func(s *ledgerState) error { a = s.Audit; return nil })
	return a, e
}

// Cancel removes only a queued request. A granted lease must be released after
// its process has exited; cancellation alone is not proof that work stopped.
func (l *Ledger) Cancel(ctx context.Context, id string, owner Owner, reason string) error {
	return l.transact(ctx, true, func(s *ledgerState) error {
		if reason == "" {
			return errors.New("cancellation reason required")
		}
		for i, w := range s.Pool.Waiting {
			if w.ID == id && w.Owner == owner {
				s.Pool.Waiting = append(s.Pool.Waiting[:i], s.Pool.Waiting[i+1:]...)
				s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "cancelled", ID: id, Owner: owner, Reason: reason})
				return nil
			}
		}
		return errors.New("request not waiting for owner")
	})
}

func (l *Ledger) Recover(ctx context.Context, proof OperationProof, ownerDead bool) error {
	return l.transact(ctx, true, func(s *ledgerState) error {
		changed, e := s.Pool.RecoverProof(proof, ownerDead)
		if e != nil {
			return e
		}
		for poolID, pp := range s.ProviderPools {
			if proof.ProviderPool != "" && proof.ProviderPool != poolID {
				continue
			}
			pChanged, pErr := pp.RecoverProof(proof, ownerDead)
			if pErr != nil {
				return pErr
			}
			if pChanged {
				s.ProviderPools[poolID] = pp
				changed = true
			}
		}
		if changed {
			s.Audit = append(s.Audit, LeaseAudit{At: time.Now().UTC(), Action: "recovered", ID: proof.ID, Owner: proof.Owner, Reason: "durable " + proof.State + " proof and exact owner death"})
		}
		return nil
	})
}

// Publish a newly created lock only after locking its private inode. Publishing
// an unlocked O_EXCL file lets a second opener win flock before the creator
// writes state.json, mistaking concurrent bootstrap for lost durable state.
// Established lock files are never replaced or renamed.
func openLedgerLock(directory string) (*os.File, bool, error) {
	path := filepath.Join(directory, "lock")
	openExisting := func() (*os.File, bool, error) {
		fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return nil, false, err
		}
		return os.NewFile(uintptr(fd), "ledger lock"), false, nil
	}
	if file, fresh, err := openExisting(); err == nil || !errors.Is(err, syscall.ENOENT) {
		return file, fresh, err
	}
	file, err := os.CreateTemp(directory, ".ledger-lock-")
	if err != nil {
		return nil, false, err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, false, err
	}
	if err = os.Link(temporary, path); err != nil {
		file.Close()
		if errors.Is(err, os.ErrExist) {
			return openExisting()
		}
		return nil, false, err
	}
	return file, true, nil
}
