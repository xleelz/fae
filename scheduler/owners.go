package scheduler

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// OwnerCache observes the owners of a host heavy-op pool for Advance, so every
// access that advances the shared ledger reconciles dead owners (FAE-HOST-031).
// Self is Alive; a foreign owner is Dead only when its pid is absent or its
// start time differs. Proven deaths are remembered; other owners are probed
// again at most once a second. Ambiguous owners stay Unknown and keep their
// capacity. The zero value is not usable; use NewOwnerCache.
type OwnerCache struct {
	self  Owner
	probe func(Owner) Liveness
	mu    sync.Mutex
	seen  map[Owner]ownerSeen
}

type ownerSeen struct {
	state Liveness
	at    time.Time
}

// NewOwnerCache returns the liveness cache of one ledger client; a nil probe
// selects ProbeOwner.
func NewOwnerCache(self Owner, probe func(Owner) Liveness) *OwnerCache {
	if probe == nil {
		probe = ProbeOwner
	}
	return &OwnerCache{self: self, probe: probe, seen: map[Owner]ownerSeen{}}
}

// Observe returns the liveness of every holder and waiter of p.
func (c *OwnerCache) Observe(p Pool) map[Owner]Liveness {
	c.mu.Lock()
	defer c.mu.Unlock()
	observed := map[Owner]Liveness{c.self: Alive}
	now := time.Now()
	observe := func(o Owner) {
		if _, done := observed[o]; done {
			return
		}
		if v, ok := c.seen[o]; ok && (v.state == Dead || now.Sub(v.at) < time.Second) {
			observed[o] = v.state
			return
		}
		state := c.probe(o)
		c.seen[o] = ownerSeen{state, now}
		observed[o] = state
	}
	for _, h := range p.Holders {
		observe(h.Owner)
	}
	for _, w := range p.Waiting {
		observe(w.Owner)
	}
	return observed
}

// DefaultHostLedger is the stable host-scoped heavy-op ledger every instance
// of this user shares unless one is configured (FAE-HOST-031): it does not
// depend on the instance home, so instances with different homes still share
// one pool. The directory is private (0700), so the scope is one user's host.
func DefaultHostLedger() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", errors.Join(err, errors.New("host ledger default requires an absolute home directory"))
	}
	return filepath.Join(home, ".fae", "host-ledger"), nil
}
