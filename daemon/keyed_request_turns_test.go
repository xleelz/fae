package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
)

// FAE-ROB-020 with FAE-API-012: a keyed mutation holds only its own key's
// turn. While a keyed edge edit's read before the writer lock is slowed, a
// keyed task control under another key returns in under 1 s, and the same
// edit retried under the same key waits for the original, then replays its
// result: the edge is added once.
func TestKeyedMutationsWaitOnlyForTheirOwnKey(t *testing.T) {
	d := restartedWithFinished(t, "r", "done-a")
	if _, err := call(t, d, "BacklogAdmit", BacklogParams{Items: []BacklogItemParams{{AdmitParams: AdmitParams{ID: "dependent", Tree: "tree", Priority: 10, Content: "accept dependent"}}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	edge := BacklogEdgeParams{BacklogEdge: controlplane.BacklogEdge{From: "done-a", To: "dependent", Kind: "blocking"}, AuthorizingMessage: "message"}
	started := slowPlaneRead(d, "stored/done-a", 3*time.Second)
	slowed := slowedCall(t, d, started, "BacklogEdge", edge, "edge")
	begun := time.Now()
	_, err := callOnce(t, d, "Reprioritize", TaskControlParams{ID: "dependent", Priority: 20, AuthorizingMessage: "message"}, "priority")
	if elapsed := time.Since(begun); err != nil || elapsed >= time.Second {
		t.Fatalf("a keyed mutation waited for another key's plane read: %s: %v", elapsed, err)
	}
	select {
	case <-slowed:
		t.Fatal("the slowed call finished before the retry was sent")
	default:
	}
	raw, _ := json.Marshal(edge)
	token, err := os.ReadFile(d.layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	retried := make(chan rpcOutcome, 1)
	go func() {
		r, err := api.Call(context.Background(), d.layout.Socket, token, api.Request{Version: 1, Method: "BacklogEdge", Params: raw, Key: "edge"})
		retried <- rpcOutcome{r, err}
	}()
	first := slowedResult(t, slowed)
	select {
	case r := <-retried:
		if r.err != nil || string(r.response.Result) != string(first) {
			t.Fatalf("the retry under the same key did not replay: %s / %s: %v", first, r.response.Result, r.err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the retry under the same key never returned")
	}
}

// FAE-ROB-020 with FAE-API-012: a key's turn excludes only requests under
// the same key; a waiter whose context ends leaves without it, and released
// turns are forgotten.
func TestKeyedRequestTurnsExcludeOnlyTheSameKey(t *testing.T) {
	var k keyedRequestTurns
	ctx := context.Background()
	releaseA, err := k.acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	other, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	releaseB, err := k.acquire(other, "b")
	if err != nil {
		t.Fatal("another key waited for a held turn", err)
	}
	got := make(chan func(), 1)
	go func() {
		release, err := k.acquire(ctx, "a")
		if err != nil {
			t.Error(err)
			return
		}
		got <- release
	}()
	select {
	case <-got:
		t.Fatal("the same key did not wait for the held turn")
	case <-time.After(200 * time.Millisecond):
	}
	releaseA()
	select {
	case release := <-got:
		release()
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never took the released turn")
	}
	waiting, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if _, err = k.acquire(waiting, "b"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("a waiter whose context ended took or kept waiting for the turn", err)
	}
	releaseB()
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.turns) != 0 {
		t.Fatal("released turns kept", len(k.turns))
	}
}
