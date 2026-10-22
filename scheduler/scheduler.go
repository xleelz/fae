// Package scheduler contains scheduling decisions and the host-shared durable
// lease ledger. The pure Pool makes no process-launch or persistence effects.
package scheduler

import (
	"fmt"
	"github.com/xleelz/fae/domain"
	"sort"
	"time"
)

type Item struct {
	ID                      string
	Priority                int
	Admitted                time.Time
	State                   domain.State
	RetireReason, Successor string
	Deferred                bool
}
type Edge struct {
	Prerequisite, Dependent string
	Blocking                bool
}
type Withheld struct{ Task, Reason, Prerequisite string }

func Ready(items []Item, edges []Edge) ([]Item, []Withheld) {
	byID := map[string]Item{}
	for _, i := range items {
		byID[i.ID] = i
	}
	var ready []Item
	var held []Withheld
	for _, i := range items {
		if i.State != domain.Pending && i.State != domain.Parked && i.State != domain.Blocked {
			continue
		}
		if i.Deferred {
			held = append(held, Withheld{i.ID, "Deferred", ""})
			continue
		}
		blocked := false
		for _, e := range edges {
			if !e.Blocking || e.Dependent != i.ID {
				continue
			}
			id := e.Prerequisite
			seen := map[string]bool{}
			satisfied := false
			for {
				p, ok := byID[id]
				if !ok || seen[id] {
					break
				}
				seen[id] = true
				if p.State == domain.Done || (p.State == domain.Retired && p.RetireReason == "contained") {
					satisfied = true
					break
				}
				if p.State == domain.Retired && p.RetireReason == "superseded-by" && p.Successor != "" {
					id = p.Successor
					continue
				}
				break
			}
			if !satisfied {
				blocked = true
				held = append(held, Withheld{i.ID, "Dependency", id})
			}
		}
		if !blocked {
			ready = append(ready, i)
		}
	}
	sort.SliceStable(ready, func(a, b int) bool {
		if ready[a].Priority != ready[b].Priority {
			return ready[a].Priority > ready[b].Priority
		}
		if !ready[a].Admitted.Equal(ready[b].Admitted) {
			return ready[a].Admitted.Before(ready[b].Admitted)
		}
		return ready[a].ID < ready[b].ID
	})
	return ready, held
}

type Owner struct {
	PID     int
	Started int64
}
type Request struct {
	ID            string
	Owner         Owner
	Weight, Class int
	Queued        time.Time
	Warned        bool
}
type Lease struct {
	Request
	Expires time.Time
}
type Pool struct {
	// Capacity is the historical validation ceiling. AdmissionLimit, when set,
	// is the current ceiling for new grants; lowering it never invalidates a holder.
	Capacity                int
	AdmissionLimit          int `json:",omitempty"`
	TTL, WarnAfter, MaxWait time.Duration
	Holders                 []Lease
	Waiting                 []Request
}
type Event struct {
	RequestID, Kind string
	Wait            time.Duration
	Holders         []string
}

func NewPool(capacity int) (*Pool, error) {
	if capacity <= 0 {
		return nil, fmt.Errorf("capacity must be positive")
	}
	return &Pool{Capacity: capacity, TTL: 6 * time.Hour, WarnAfter: 300 * time.Second, MaxWait: 1800 * time.Second}, nil
}
func (p *Pool) Enqueue(r Request) error {
	if r.ID == "" || r.Owner.PID <= 0 || r.Owner.Started <= 0 || r.Weight <= 0 || r.Weight > p.Capacity || r.Class < 0 || r.Class > 2 {
		return fmt.Errorf("invalid lease request")
	}
	for _, h := range p.Holders {
		if h.ID == r.ID {
			return fmt.Errorf("duplicate request")
		}
	}
	for _, w := range p.Waiting {
		if w.ID == r.ID {
			return fmt.Errorf("duplicate request")
		}
	}
	p.Waiting = append(p.Waiting, r)
	return nil
}

// Advance reconciles owner identity and wait bounds before granting. Expired
// holders retain capacity until their owner is confirmed dead or explicitly released.
// alive must compare both PID and start time. A nil liveness probe fails closed.
func (p *Pool) Advance(now time.Time, alive func(Owner) bool) ([]Lease, []Event, error) {
	if p.AdmissionLimit < 0 || p.AdmissionLimit > p.Capacity {
		return nil, nil, fmt.Errorf("invalid admission limit")
	}
	if alive == nil || p.Capacity <= 0 || p.TTL <= 0 || p.MaxWait <= 0 || p.WarnAfter < 0 {
		return nil, nil, fmt.Errorf("unreadable or invalid lease ledger")
	}
	// Treat a malformed snapshot as unreadable before mutating any state.
	seen := map[string]bool{}
	validate := func(r Request) bool {
		if r.ID == "" || seen[r.ID] || r.Weight <= 0 || r.Weight > p.Capacity || r.Owner.PID <= 0 || r.Owner.Started <= 0 || r.Class < 0 || r.Class > 2 {
			return false
		}
		seen[r.ID] = true
		return true
	}
	for _, h := range p.Holders {
		if !validate(h.Request) {
			return nil, nil, fmt.Errorf("corrupt lease holder")
		}
	}
	for _, w := range p.Waiting {
		if !validate(w) {
			return nil, nil, fmt.Errorf("corrupt lease waiter")
		}
	}
	var events []Event
	holders := make([]Lease, 0, len(p.Holders))
	used := 0
	for _, h := range p.Holders {
		if !alive(h.Owner) {
			events = append(events, Event{RequestID: h.ID, Kind: "Reclaimed"})
			continue
		}
		if !CanAcquire(p.Capacity, used, h.Weight) {
			return nil, nil, fmt.Errorf("corrupt lease capacity")
		}
		used += h.Weight
		holders = append(holders, h)
	}
	p.Holders = holders
	waiters := p.Waiting[:0]
	for _, w := range p.Waiting {
		wait := now.Sub(w.Queued)
		if !alive(w.Owner) {
			events = append(events, Event{RequestID: w.ID, Kind: "OwnerDead"})
			continue
		}
		if wait >= p.MaxWait {
			events = append(events, Event{RequestID: w.ID, Kind: "Timeout", Wait: wait})
			continue
		}
		if wait >= p.WarnAfter && !w.Warned {
			ids := []string{}
			for _, h := range p.Holders {
				ids = append(ids, h.ID)
			}
			events = append(events, Event{w.ID, "Waiting", wait, ids})
			w.Warned = true
		}
		waiters = append(waiters, w)
	}
	p.Waiting = waiters
	sort.SliceStable(p.Waiting, func(i, j int) bool {
		a, b := p.Waiting[i], p.Waiting[j]
		if !a.Queued.Equal(b.Queued) {
			return a.Queued.Before(b.Queued)
		}
		return a.Class > b.Class
	})
	var granted []Lease
	for len(p.Waiting) > 0 {
		w := p.Waiting[0]
		limit := p.Capacity
		if p.AdmissionLimit > 0 {
			limit = p.AdmissionLimit
		}
		if !CanAcquireDynamic(limit, used, w.Weight) {
			break
		}
		l := Lease{w, now.Add(p.TTL)}
		p.Holders = append(p.Holders, l)
		granted = append(granted, l)
		used += w.Weight
		p.Waiting = p.Waiting[1:]
	}
	return granted, events, nil
}
func (p *Pool) Release(id string, owner Owner) bool {
	for i, h := range p.Holders {
		if h.ID == id && CanRelease(owner.PID, int(owner.Started), h.Owner.PID, int(h.Owner.Started)) {
			p.Holders = append(p.Holders[:i], p.Holders[i+1:]...)
			return true
		}
	}
	return false
}

type RoleRevision struct {
	Workflow, Molecule, Role string
	Revision, Cap            int
	Global, Live             bool
}

func RolePool(r RoleRevision, live []RoleRevision) (string, int, error) {
	if r.Workflow == "" || r.Role == "" || r.Molecule == "" || r.Cap <= 0 {
		return "", 0, fmt.Errorf("invalid role")
	}
	cap := r.Cap
	rev := r.Revision
	if r.Global {
		for _, x := range live {
			if x.Workflow == r.Workflow && x.Role == r.Role && x.Global && x.Live && x.Revision > rev {
				if x.Cap <= 0 {
					return "", 0, fmt.Errorf("invalid live cap")
				}
				cap = x.Cap
				rev = x.Revision
			}
		}
		return r.Workflow + "/" + r.Role, cap, nil
	}
	return r.Workflow + "/" + r.Molecule + "/" + r.Role, cap, nil
}
func WithinLimit(current, limit int) bool {
	return current >= 0 && (limit == -1 || (limit > 0 && current < limit))
}
