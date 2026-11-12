package daemon

import (
	"context"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/domain"
	"sync"
	"time"
)

type readFlight struct {
	done    chan struct{}
	value   any
	err     error
	cancel  context.CancelFunc
	waiters int
}
type readPool struct {
	mu      sync.Mutex
	flights map[string]*readFlight
	slots   chan struct{}
}

// run bounds outstanding keys as well as running queries. Requests only wait on
// completion channels: SQL runs in the pool and never under the daemon mutex.
// Cancellation belongs to each waiter; the last departing waiter cancels SQL.
func (p *readPool) run(ctx context.Context, key string, query func(context.Context) (any, error)) (any, error) {
	p.mu.Lock()
	if p.flights == nil {
		p.flights = map[string]*readFlight{}
		p.slots = make(chan struct{}, 8)
	}
	f := p.flights[key]
	if f == nil {
		if len(p.flights) >= 64 {
			p.mu.Unlock()
			return nil, &api.RPCError{Code: "read-busy", Message: "bounded read queue is full"}
		}
		work, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		f = &readFlight{done: make(chan struct{}), cancel: cancel}
		p.flights[key] = f
		go func() {
			defer cancel()
			select {
			case p.slots <- struct{}{}:
				f.value, f.err = query(work)
				<-p.slots
			case <-work.Done():
				f.err = work.Err()
			}
			p.mu.Lock()
			if p.flights[key] == f {
				delete(p.flights, key)
			}
			close(f.done)
			p.mu.Unlock()
		}()
	}
	f.waiters++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		f.waiters--
		if f.waiters == 0 {
			f.cancel()
		}
		p.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return f.value, f.err
	}
}

func (d *Daemon) handleReads(ctx context.Context, r api.Request) (any, error) {
	principal, permission, _, err := authorizePrincipal(ctx, r)
	if err != nil {
		return nil, err
	}
	if !permission.Read && r.Key != "" && r.Method != "Admit" {
		return d.keyedMutation(ctx, principal, r)
	}
	// Cached reads answer from their snapshots; any plane read they need runs
	// in this pool or in their keeper (READS.md).
	if !permission.Read || r.Method == "Status" || r.Method == "Board" || r.Method == "Topology" {
		return d.handleDirect(ctx, r)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw := r.Params
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	canonical, err := domain.CanonicalJSON(raw)
	if err != nil {
		return nil, err
	}
	key := principal.Actor + ":" + principal.Task + ":" + r.Method + ":" + string(canonical)
	return d.readPool.run(ctx, key, func(work context.Context) (any, error) { return d.handleDirect(work, r) })
}

func (d *Daemon) handle(ctx context.Context, r api.Request) (any, error) {
	if _, _, _, err := authorizePrincipal(ctx, r); err != nil {
		return nil, err
	}
	return d.handleReads(ctx, r)
}
