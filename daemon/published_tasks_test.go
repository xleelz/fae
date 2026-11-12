package daemon

import (
	"testing"
	"time"
)

// FAE-ROB-020: the Census and ReviewTargetHealth pool reads find the drive's
// tasks in the published snapshot, never under the writer lock. While the
// test holds d.mu, as a writer stuck in plane, git or ledger I/O does, each
// answers within its 5 s deadline, and with the answer it gave with the lock
// free: a census with a supersession proposal, and the review health of an
// admitted task and of an unknown one.
func TestPoolReadsDoNotWaitForTheWriterLock(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	stopDrive(t, d)
	for _, id := range []string{"held", "successor"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Content: "accept " + id, AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = call(t, d, "Escalate", TaskControlParams{ID: "held", Reason: "review", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	reads := []struct {
		method string
		params any
	}{
		{"Census", CensusParams{}},
		{"Census", CensusParams{Proposals: []CensusProposal{{ID: "held", Reason: "superseded-by successor", Evidence: "operator ruling"}}}},
		{"ReviewTargetHealth", map[string]string{"task": "held"}},
		{"ReviewTargetHealth", map[string]string{"task": "unknown"}},
	}
	answer := func(method string, params any) string {
		r, err := callOnce(t, d, method, params, "")
		if err != nil {
			return "error: " + err.Error()
		}
		return string(r.Result)
	}
	free := []string{}
	for _, read := range reads {
		free = append(free, answer(read.method, read.params))
	}
	if len(free[1]) < 200 || free[1] == free[0] {
		t.Fatalf("census with a proposal: %s", free[1])
	}
	d.mu.Lock()
	for i, read := range reads {
		begun := time.Now()
		got := answer(read.method, read.params)
		if elapsed := time.Since(begun); elapsed >= 5*time.Second || got != free[i] {
			d.mu.Unlock()
			t.Fatalf("%s waited for the writer lock: %s: %s, want %s", read.method, elapsed, got, free[i])
		}
	}
	d.mu.Unlock()
}
