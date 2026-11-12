package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/domain"
)

// keyedMutation makes every mutating RPC idempotent under its request key
// (FAE-API-012): a repeated call with the same key and request returns the
// recorded result and writes nothing; the same key with another request is
// refused. Admit records its key inside the admission transaction instead.
// Keys are scoped by method and principal. Only the key's own turn is held
// across the lookup, the mutation and the record (FAE-ROB-020).
func (d *Daemon) keyedMutation(ctx context.Context, principal api.Principal, r api.Request) (any, error) {
	if len(r.Key) > 128 {
		return nil, errors.New("idempotence key exceeds 128 bytes")
	}
	params := r.Params
	if len(params) == 0 {
		params = json.RawMessage(`{}`)
	}
	canonical, err := domain.CanonicalJSON(params)
	if err != nil {
		return nil, err
	}
	request, _ := json.Marshal(struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{r.Method, canonical})
	key := "rpc:" + r.Method + ":" + principal.Actor + ":" + principal.Task + ":" + r.Key
	release, err := d.keyedRequests.acquire(ctx, key)
	if err != nil {
		return nil, err
	}
	defer release()
	if recorded, found, err := d.store.LookupRequest(ctx, key, request); err != nil {
		return nil, err
	} else if found {
		return recorded, nil
	}
	result, err := d.handleDirect(ctx, r)
	if err != nil {
		return nil, err
	}
	response, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	// The mutation committed; an unrecorded key only loses replay, never the result.
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err = d.store.RecordRequest(record, key, request, response); err != nil {
		d.taskLog("idempotence key %s not recorded: %v\n", r.Key, err)
	}
	return json.RawMessage(response), nil
}

// keyedRequestTurns gives each request key its own exclusion. A keyed
// mutation holds its key's turn across its lookup, mutation and record, so a
// retry under the same key waits for the original and then replays its
// recorded result (FAE-API-012), while mutations under different keys never
// wait on each other's plane I/O (FAE-ROB-020). mu guards only the map.
type keyedRequestTurns struct {
	mu    sync.Mutex
	turns map[string]*keyedRequestTurn
}

type keyedRequestTurn struct {
	held  chan struct{} // holds a token while a request under the key runs
	users int           // requests holding or awaiting the turn; guarded by mu
}

// acquire waits for key's turn, or until ctx ends. release ends the turn.
func (k *keyedRequestTurns) acquire(ctx context.Context, key string) (release func(), err error) {
	k.mu.Lock()
	if k.turns == nil {
		k.turns = map[string]*keyedRequestTurn{}
	}
	t := k.turns[key]
	if t == nil {
		t = &keyedRequestTurn{held: make(chan struct{}, 1)}
		k.turns[key] = t
	}
	t.users++
	k.mu.Unlock()
	leave := func() {
		k.mu.Lock()
		if t.users--; t.users == 0 {
			delete(k.turns, key)
		}
		k.mu.Unlock()
	}
	select {
	case t.held <- struct{}{}:
		return func() { <-t.held; leave() }, nil
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
}
