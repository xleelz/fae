package daemon

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
)

// FAE-API-012: every mutating RPC is idempotent under its key. A repeated call
// with the same key returns the original result and writes nothing; the same
// key with another request is refused; a keyed admission replays while the
// instance drains.
func TestKeyedMutationsReplayWithoutWriting(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	audits := func(action string) int {
		t.Helper()
		var n int
		if e := d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "count keyed audits", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE action=?`, action).Scan(&n)
		}); e != nil {
			t.Fatal(e)
		}
		return n
	}
	admitted, err := call(t, d, "Admit", AdmitParams{ID: "keyed", Tree: "tree", AuthorizingMessage: "message"}, "keyed-admit")
	if err != nil {
		t.Fatal(err)
	}
	first, err := call(t, d, "Reprioritize", TaskControlParams{ID: "keyed", Priority: 40, AuthorizingMessage: "message"}, "keyed-priority")
	if err != nil {
		t.Fatal(err)
	}
	before, version := audits("Reprioritize"), d.jobs["keyed"].Version
	again, err := call(t, d, "Reprioritize", TaskControlParams{ID: "keyed", Priority: 40, AuthorizingMessage: "message"}, "keyed-priority")
	if err != nil || string(again.Result) != string(first.Result) {
		t.Fatalf("replay differs: %s / %s %v", first.Result, again.Result, err)
	}
	if audits("Reprioritize") != before || d.jobs["keyed"].Version != version {
		t.Fatal("a replayed keyed mutation wrote")
	}
	if _, err = call(t, d, "Reprioritize", TaskControlParams{ID: "keyed", Priority: 50, AuthorizingMessage: "message"}, "keyed-priority"); err == nil || !strings.Contains(err.Error(), "idempot") {
		t.Fatal("a different request reused a key", err)
	}
	note, err := call(t, d, "Message", map[string]string{"text": "keyed note"}, "keyed-note")
	if err != nil {
		t.Fatal(err)
	}
	if repeated, e := call(t, d, "Message", map[string]string{"text": "keyed note"}, "keyed-note"); e != nil || string(repeated.Result) != string(note.Result) {
		t.Fatal("message replay recorded a second message", string(note.Result), string(repeated.Result), e)
	}
	if _, err = call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); err != nil {
		t.Fatal(err)
	}
	replayed, err := call(t, d, "Admit", AdmitParams{ID: "keyed", Tree: "tree", AuthorizingMessage: "message"}, "keyed-admit")
	if err != nil || string(replayed.Result) != string(admitted.Result) {
		t.Fatal("keyed admission did not replay while draining", err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "fresh", Tree: "tree", AuthorizingMessage: "message"}, "fresh"); err == nil {
		t.Fatal("draining admitted new work")
	}
}
