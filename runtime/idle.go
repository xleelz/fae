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
	"time"

	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/processowner"
)

// IdleRegistration is an adapter capability, not a discovery/adoption API. The
// adapter must supply records created before allowing its own processes to run.
// Final and QA sessions must set Protected; only explicit pre-code globals shed.
// Reap waits for adapter-owned children after their groups have been terminated.
// Context is a non-authoritative handoff, never evidence or a source of grants.
type IdleRegistration struct {
	ID, Socket, Role           string
	Global, PreCode, Protected bool
	Expire                     bool // Explicit TTL for task-bound resident transports.
	Records                    []*processowner.Record
	Context                    json.RawMessage
	Reap                       func() error
}

var ErrIdleSessionActive = errors.New("session already active")

type idleEntry struct {
	IdleRegistration
	idleSince time.Time
	active    bool
}
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
func (p *OwnedIdleSessions) retire(ctx context.Context, s *idleEntry) error {
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
	delete(p.sessions, s.ID)
	return nil
}

// Round uses a fresh host-wide socket census. Partial or unknown inventories fail
// closed. A foreign-held limit never triggers ownership-based escalation.
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
	ids := make([]string, 0, len(p.sessions))
	for id, s := range p.sessions {
		if _, ok := counts[s.Socket]; !ok {
			p.rounds = 0
			return nil, errors.New("registered idle socket missing from live census")
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
				return nil, e
			}
			p.rounds = 0
			return []IdleAction{{Kind: "idle-session-retired", Session: id, Reason: "idle TTL; durable handoff retained"}}, nil
		}
	}
	if total < limit || ownedIdle < limit || inflight != 0 {
		p.rounds = 0
		return nil, nil
	}
	p.rounds++
	if p.rounds < 3 {
		return nil, nil
	}
	p.rounds = 0
	for _, id := range ids {
		s := p.sessions[id]
		if kernel.CanShedIdle(s.Global, s.PreCode, s.Protected, s.active, inflight) {
			if e := p.retire(ctx, s); e != nil {
				return nil, e
			}
			return []IdleAction{{Kind: "idle-session-shed", Session: id, Reason: "three own-idle saturated rounds; durable handoff retained"}}, nil
		}
	}
	return []IdleAction{{Kind: "PaneCapStarvation", Reason: fmt.Sprintf("%d own idle panes saturate limit %d; no sheddable pre-code global session", ownedIdle, limit)}}, nil
}
func (p *OwnedIdleSessions) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, s := range p.sessions {
		if s.active {
			return errors.New("cannot retire active idle-pool session")
		}
		if e := p.retire(ctx, s); e != nil {
			return e
		}
	}
	return nil
}

// Count lets the daemon avoid host inventory work when no adapter uses pooling.
func (p *OwnedIdleSessions) Count() int { p.mu.Lock(); defer p.mu.Unlock(); return len(p.sessions) }
