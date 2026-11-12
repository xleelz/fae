package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/processowner"
)

// IdleRegistration is an adapter capability, not a discovery/adoption API. The
// adapter must supply records created before allowing its own processes to run.
// Final and QA sessions must set Protected; only explicit pre-code globals shed.
// Reap waits for adapter-owned children after their groups have been terminated.
// OnRetired, when set, is called once the session's retirement is proven and
// its registration released; it runs under the registry lock and must not
// block. Context is a non-authoritative handoff, never evidence or a source of
// grants.
type IdleRegistration struct {
	ID, Socket, Role           string
	Global, PreCode, Protected bool
	Expire                     bool // Explicit TTL for task-bound resident transports.
	Records                    []*processowner.Record
	Context                    json.RawMessage
	Reap                       func() error
	OnRetired                  func()
}

var ErrIdleSessionActive = errors.New("session already active")

type idleEntry struct {
	IdleRegistration
	idleSince        time.Time
	active, retiring bool
	// discard removes the context file at retirement: a released task-bound
	// session keeps none (FAE-RT-011).
	discard bool
	// pending is closed when the retirement in progress ends; nil when none.
	pending chan struct{}
	// unproven marks a session refused for reuse whose retirement was tried
	// and failed; it is counted in OwnedIdleSessions.unproven until retired.
	unproven bool
}

// ErrIdleSessionRetiring refuses reuse of a released session whose retirement
// is not yet proven; the caller reconciles it before relaunching.
var ErrIdleSessionRetiring = errors.New("session retirement pending; relaunch required")

type IdleAction struct{ Kind, Session, Reason string }

// OwnedIdleSessions tracks only explicitly registered, current-daemon sessions.
// It never adopts sockets discovered by the host inventory. On restart the normal
// process owner recovery retires old processes; context files survive separately.
type OwnedIdleSessions struct {
	mu                 sync.Mutex
	root, owner, birth string
	sessions           map[string]*idleEntry
	rounds             int
	TTL                time.Duration
	// unproven counts the sessions marked unproven, read without the lock.
	unproven atomic.Int64
}

func NewOwnedIdleSessions(root, owner string) (*OwnedIdleSessions, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(owner) {
		return nil, errors.New("absolute idle context root and process owner required")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	for p := root; ; p = filepath.Dir(p) {
		info, e := os.Lstat(p)
		if e != nil {
			return nil, e
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("idle context path contains symlink")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	info, e := os.Stat(root)
	if e != nil {
		return nil, e
	}
	if info.Mode().Perm() != 0700 {
		return nil, errors.New("private idle context directory required")
	}
	birth, e := processowner.Birth(os.Getpid())
	if e != nil {
		return nil, e
	}
	return &OwnedIdleSessions{root: root, owner: owner, birth: birth, sessions: map[string]*idleEntry{}, TTL: 30 * time.Minute}, nil
}
func idleID(id string) bool { return id != "" && id != "." && id != ".." && filepath.Base(id) == id }
func (p *OwnedIdleSessions) Register(r IdleRegistration, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !idleID(r.ID) || r.Role == "" || !filepath.IsAbs(r.Socket) || len(r.Records) == 0 || r.Reap == nil || now.IsZero() || !json.Valid(r.Context) {
		return errors.New("invalid idle session registration")
	}
	rel, e := filepath.Rel(p.owner, r.Socket)
	if e != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return errors.New("idle socket outside owned runtime root")
	}
	if _, ok := p.sessions[r.ID]; ok {
		return errors.New("duplicate idle session")
	}
	for _, v := range p.sessions {
		if v.Socket == r.Socket {
			return errors.New("idle socket already registered")
		}
	}
	seen := map[int]bool{}
	records := make([]*processowner.Record, 0, len(r.Records))
	for _, record := range r.Records {
		if record == nil || record.Owner != p.owner || record.ParentPID != os.Getpid() || record.ParentBirth != p.birth || record.Session != r.ID || record.Path == "" || seen[record.PID] {
			return errors.New("idle process lacks exact current owner linkage")
		}

		registry := filepath.Join(p.owner, ".processes")
		if filepath.Dir(record.Path) != registry {
			return errors.New("idle process record outside owned registry")
		}
		stat, e := os.Lstat(record.Path)
		if e != nil || !stat.Mode().IsRegular() || stat.Mode().Perm() != 0600 || stat.Size() > 1<<20 {
			return errors.New("invalid private idle process record")
		}
		raw, e := os.ReadFile(record.Path)
		if e != nil {
			return e
		}
		var stored processowner.Record
		if json.Unmarshal(raw, &stored) != nil {
			return errors.New("malformed idle process record")
		}
		expected, _ := json.Marshal(record)
		actual, _ := json.Marshal(stored)
		if !bytes.Equal(expected, actual) {
			return errors.New("idle process record differs from durable ownership")
		}
		birth, e := processowner.Birth(record.PID)
		if e != nil || birth != record.Birth {
			return errors.New("idle process identity not live")
		}
		seen[record.PID] = true
		copy := *record
		records = append(records, &copy)
	}
	if err := p.save(r.ID, r.Context); err != nil {
		return err
	}
	// Role names reserved by the stock workflow are protected even when an
	// adapter forgets the explicit flag. Custom final roles must also set it.
	switch strings.ToLower(r.Role) {
	case "oracle", "qa", "final-judgement", "final_judgement", "final-judgment":
		r.Protected = true
	}
	r.Context = append(json.RawMessage(nil), r.Context...)
	r.Records = records
	p.sessions[r.ID] = &idleEntry{IdleRegistration: r, idleSince: now}
	return nil
}

// Begin must precede dispatch. It atomically excludes the session from shedding;
// the caller still revalidates its worktree and applies the current tool profile.
func (p *OwnedIdleSessions) Begin(id string) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	if !ok {
		return nil, errors.New("idle session missing; relaunch required")
	}
	if s.active {
		return nil, ErrIdleSessionActive
	}
	if s.retiring || s.pending != nil {
		return nil, ErrIdleSessionRetiring
	}
	s.active = true
	p.rounds = 0
	return append(json.RawMessage(nil), s.Context...), nil
}

// End persists a context handoff before the process becomes eligible for retirement.
func (p *OwnedIdleSessions) End(id string, handoff json.RawMessage, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	if !ok || !s.active || !json.Valid(handoff) || now.IsZero() {
		return errors.New("invalid idle completion")
	}
	if e := p.save(id, handoff); e != nil {
		return e
	}
	s.Context = append(json.RawMessage(nil), handoff...)
	s.active = false
	s.idleSince = now
	return nil
}

// Release ends a turn whose session must not outlive it: a task-bound session
// retires through the retirement path when its turn is collected and keeps no
// context (FAE-RT-011). An unproven retirement keeps the session registered and
// refused for reuse; the next maintenance round or relaunch retries it.
func (p *OwnedIdleSessions) Release(ctx context.Context, id string) error {
	return p.endForRetirement(ctx, id, true)
}

// Abandon ends a turn whose session can no longer be trusted for reuse: the
// turn's own retirement is unproven, or its handoff could not be recorded. The
// session is never left active: it is refused for reuse and retired through
// the retirement path now or, if that is unproven too, by the next maintenance
// round or relaunch. Its last recorded context is kept for the relaunch
// (FAE-SCH-014).
func (p *OwnedIdleSessions) Abandon(ctx context.Context, id string) error {
	return p.endForRetirement(ctx, id, false)
}

func (p *OwnedIdleSessions) endForRetirement(ctx context.Context, id string, discard bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	if !ok || !s.active {
		return errors.New("invalid idle release")
	}
	s.active, s.retiring, s.discard = false, true, discard
	return p.retire(ctx, s)
}

// Retire reconciles one inactive session through the retirement path, which
// releases its registration only after exact process retirement (FAE-RT-012).
// It is idempotent: an absent session is already retired. A session that owns
// an in-flight turn is never closed. Once retirement is asked for, the session
// is refused for reuse: if it stays unproven, every later maintenance round
// retries it, like a released session's.
func (p *OwnedIdleSessions) Retire(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	s, ok := p.sessions[id]
	if !ok {
		return nil
	}
	if s.active {
		return ErrIdleSessionActive
	}
	s.retiring = true
	return p.retire(ctx, s)
}
func (p *OwnedIdleSessions) save(id string, data json.RawMessage) error {
	if len(data) > 1<<20 {
		return errors.New("idle handoff exceeds bound")
	}
	f, e := os.CreateTemp(p.root, ".handoff-")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(data); e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if e = os.Rename(f.Name(), filepath.Join(p.root, id+".json")); e != nil {
		return e
	}
	d, e := os.Open(p.root)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (p *OwnedIdleSessions) Handoff(id string) (json.RawMessage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !idleID(id) {
		return nil, errors.New("invalid session ID")
	}
	path := filepath.Join(p.root, id+".json")
	info, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return nil, errors.New("invalid idle handoff file")
	}
	b, e := os.ReadFile(path)
	if e != nil {
		return nil, e
	}
	if !json.Valid(b) {
		return nil, errors.New("corrupt idle handoff")
	}
	return b, nil
}

// retire runs the retirement path for s. It is entered and left with p.mu
// held, but releases the lock while processes are terminated, reaped and
// acknowledged, so one session's retirement never delays another session's
// Begin, End or Register. Meanwhile the entry is refused for reuse; it is
// removed only once retirement is proven. A retirement of s already in
// progress is awaited, and retried if it failed.
func (p *OwnedIdleSessions) retire(ctx context.Context, s *idleEntry) error {
	for s.pending != nil {
		done := s.pending
		p.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
		}
		p.mu.Lock()
		if e := ctx.Err(); e != nil {
			return e
		}
	}
	if p.sessions[s.ID] != s {
		return nil // retired meanwhile
	}
	done := make(chan struct{})
	s.pending = done
	discard := s.discard
	p.mu.Unlock()
	e := p.retireProcesses(ctx, s, discard)
	p.mu.Lock()
	s.pending = nil
	close(done)
	if e != nil {
		if s.retiring && !s.unproven {
			s.unproven = true
			p.unproven.Add(1)
		}
		return e
	}
	if s.unproven {
		p.unproven.Add(-1)
	}
	delete(p.sessions, s.ID)
	if s.OnRetired != nil {
		s.OnRetired()
	}
	return nil
}

// retireProcesses runs without p.mu; it reads only fields fixed at Register.
func (p *OwnedIdleSessions) retireProcesses(ctx context.Context, s *idleEntry, discard bool) error {
	for _, record := range s.Records {
		if e := processowner.Terminate(ctx, record, p.owner); e != nil {
			return e
		}
	}
	if e := s.Reap(); e != nil {
		return e
	}
	for _, record := range s.Records {
		if e := processowner.Retire(record); e != nil {
			return e
		}
	}
	// Reap has completed adapter-owned release, and every registered group is
	// terminal. Idle lifetime records are no longer needed for turn recovery.
	for _, record := range s.Records {
		if record.Session != "" {
			if e := processowner.AcknowledgeLinkedRetirement(filepath.Dir(record.Path), record.Owner, record.Session, record.ParentPID, record.ParentBirth); e != nil {
				return e
			}
		}
	}
	// No save can race this removal: the entry is neither active nor
	// replaceable by Register until it is deleted.
	if !discard {
		return nil
	}
	if e := os.Remove(filepath.Join(p.root, s.ID+".json")); e != nil && !os.IsNotExist(e) {
		return e
	}
	return nil
}

// Round uses a fresh host-wide socket census. Partial or unknown inventories fail
// closed. A foreign-held limit never triggers ownership-based escalation. A
// reconciliation whose retirement stays unproven is returned as an error with
// the round's actions; it never stops the rest of the round.
func (p *OwnedIdleSessions) Round(ctx context.Context, counts map[string]int, limit, inflight int, now time.Time) ([]IdleAction, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if limit < 1 || inflight < 0 || now.IsZero() {
		return nil, errors.New("invalid idle maintenance round")
	}
	total, ownedIdle := 0, 0
	for _, n := range counts {
		if n < 0 || n > 1000000 || total > 1000000-n {
			return nil, errors.New("invalid pane census")
		}
		total += n
	}
	// FAE-RT-012: an idle slot whose pane was removed (its socket reports no
	// pane or is gone) and whose registered group is proven gone, or a released
	// session whose retirement failed, is reconciled through the retirement
	// path instead of failing every later round closed.
	var groups map[int]bool
	var reconciled []IdleAction
	var unproven error
	for _, id := range sortedIdleIDs(p.sessions) {
		s := p.sessions[id]
		if s == nil || s.active || s.pending != nil || !s.retiring && counts[s.Socket] > 0 {
			continue
		}
		reason := "session refused for reuse retired; durable handoff retained"
		if s.discard {
			reason = "released task-bound session retired"
		}
		if !s.retiring {
			if groups == nil {
				var e error
				if groups, e = liveGroups(); e != nil {
					break // An unreadable census proves nothing gone.
				}
			}
			if !anyGroupGone(s.Records, groups) {
				continue
			}
			reason = "owned pane removed under the idle slot; retired for relaunch, durable handoff retained"
		}
		if e := p.retire(ctx, s); e != nil {
			unproven = errors.Join(unproven, fmt.Errorf("idle session %s retirement unproven: %w", id, e))
			continue
		}
		reconciled = append(reconciled, IdleAction{Kind: "idle-session-reconciled", Session: id, Reason: reason})
	}
	if len(reconciled) > 0 {
		p.rounds = 0
		return reconciled, unproven
	}
	ids := make([]string, 0, len(p.sessions))
	for id, s := range p.sessions {
		if s.retiring || s.pending != nil {
			// Refused for reuse and awaiting (or in) retirement: not in
			// service, but an own idle pane while it lasts.
			ownedIdle += counts[s.Socket]
			continue
		}
		if _, ok := counts[s.Socket]; !ok {
			p.rounds = 0
			return nil, errors.Join(unproven, errors.New("registered idle socket missing from live census"))
		}
		ids = append(ids, id)
		if !s.active {
			ownedIdle += counts[s.Socket]
		}
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := p.sessions[ids[i]], p.sessions[ids[j]]
		if a.idleSince.Equal(b.idleSince) {
			return ids[i] < ids[j]
		}
		return a.idleSince.Before(b.idleSince)
	})
	ttl := p.TTL
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	for _, id := range ids {
		s := p.sessions[id]
		if (s.Global || s.Expire) && !s.active && now.Sub(s.idleSince) >= ttl {
			if e := p.retire(ctx, s); e != nil {
				return nil, errors.Join(unproven, e)
			}
			p.rounds = 0
			return []IdleAction{{Kind: "idle-session-retired", Session: id, Reason: "idle TTL; durable handoff retained"}}, unproven
		}
	}
	if total < limit || ownedIdle < limit || inflight != 0 {
		p.rounds = 0
		return nil, unproven
	}
	p.rounds++
	if p.rounds < 3 {
		return nil, unproven
	}
	p.rounds = 0
	for _, id := range ids {
		s := p.sessions[id]
		if kernel.CanShedIdle(s.Global, s.PreCode, s.Protected, s.active, inflight) {
			if e := p.retire(ctx, s); e != nil {
				return nil, errors.Join(unproven, e)
			}
			return []IdleAction{{Kind: "idle-session-shed", Session: id, Reason: "three own-idle saturated rounds; durable handoff retained"}}, unproven
		}
	}
	return []IdleAction{{Kind: "PaneCapStarvation", Reason: fmt.Sprintf("%d own idle panes saturate limit %d; no sheddable pre-code global session", ownedIdle, limit)}}, unproven
}
func (p *OwnedIdleSessions) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, id := range sortedIdleIDs(p.sessions) {
		s := p.sessions[id]
		if s == nil {
			continue // retired meanwhile
		}
		if s.active {
			return errors.New("cannot retire active idle-pool session")
		}
		if e := p.retire(ctx, s); e != nil {
			return e
		}
	}
	return nil
}

func sortedIdleIDs(sessions map[string]*idleEntry) []string {
	ids := make([]string, 0, len(sessions))
	for id := range sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// liveGroups lists the process groups with a live (non-defunct) member.
func liveGroups() (map[int]bool, error) {
	all, e := processowner.Census()
	if e != nil {
		return nil, e
	}
	groups := map[int]bool{}
	for _, p := range all {
		if !p.Defunct() {
			groups[p.PGID] = true
		}
	}
	return groups, nil
}

// anyGroupGone is positive proof from one census that a registered group has
// no live member: its leader exited, or was retired, reaped or replaced.
func anyGroupGone(records []*processowner.Record, groups map[int]bool) bool {
	for _, r := range records {
		if !groups[r.PGID] {
			return true
		}
	}
	return false
}

// Count lets the daemon avoid host inventory work when no adapter uses pooling.
func (p *OwnedIdleSessions) Count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.sessions) }

// Unretired reports the sessions refused for reuse whose retirement was tried
// and is still unproven. Each keeps its pane, and its pane reservation, until
// a later maintenance round or relaunch proves it retired. It takes no lock,
// so a status read never waits for a census or a retirement.
func (p *OwnedIdleSessions) Unretired() int { return int(p.unproven.Load()) }
