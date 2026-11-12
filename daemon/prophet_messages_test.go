package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
)

func TestProphetContextOperatorMessagesAndDirectedRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	var mu sync.Mutex
	briefs := map[string]string{}
	cfg.ProphetRunner = func(ctx context.Context, task string, brief json.RawMessage) (json.RawMessage, error) {
		mu.Lock()
		briefs[task] = string(brief)
		mu.Unlock()
		a := ProphetAnswer{Reply: "ok", Proposals: []string{}}
		if strings.Contains(string(brief), `"message":"RESTART`) {
			a.Action, a.Task, a.Information = "RestartWithInformation", "alpha", "operator supplied fresh facts"
		}
		return json.Marshal(a)
	}
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	for _, id := range []string{"alpha", "beta"} {
		if _, e = call(t, d, "Admit", AdmitParams{ID: id, Content: id, Tree: "tree", AuthorizingMessage: "message"}, id); e != nil {
			t.Fatal(e)
		}
	}
	for _, p := range []MessageParams{{ID: "alpha-note", Text: "ALPHA-OPERATOR-NOTE", Task: "alpha"}, {Text: "ALPHA-REPLY", ReplyTo: "alpha-note"}, {Text: "BETA-OPERATOR-NOTE", Task: "beta"}, {Text: "UNSCOPED-NOTE"}} {
		if _, e = call(t, d, "Message", p, ""); e != nil {
			t.Fatal(e)
		}
	}
	// The per-task context holds that task's operator messages and nothing else.
	r, e := call(t, d, "ProphetSession", ProphetParams{Task: "alpha"}, "")
	if e != nil {
		t.Fatal(e)
	}
	session := string(r.Result)
	if !strings.Contains(session, "ALPHA-OPERATOR-NOTE") || !strings.Contains(session, "ALPHA-REPLY") || strings.Contains(session, "BETA-OPERATOR-NOTE") || strings.Contains(session, "UNSCOPED-NOTE") {
		t.Fatalf("session scope %s", session)
	}
	id := prophetSend(t, d, ProphetParams{Task: "alpha", Text: "what did I say?"})
	prophetAwait(t, d, "alpha", id)
	mu.Lock()
	brief := briefs["alpha"]
	mu.Unlock()
	if !strings.Contains(brief, "ALPHA-OPERATOR-NOTE") || !strings.Contains(brief, "ALPHA-REPLY") || strings.Contains(brief, "BETA-OPERATOR-NOTE") || strings.Contains(brief, "UNSCOPED-NOTE") {
		t.Fatalf("brief scope %s", brief)
	}

	// A Prophet restart without an explicit directing message is refused with no effect.
	if _, e = call(t, d, "Escalate", TaskControlParams{ID: "alpha", Reason: "needs restart", AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	undirected := prophetSend(t, d, ProphetParams{Task: "alpha", Text: "RESTART please"})
	if reply := prophetAwait(t, d, "alpha", undirected); !strings.Contains(reply.Text, "refused") {
		t.Fatal(reply)
	}
	if j, e := d.store.GetJob(ctx, "alpha"); e != nil || j.State != "Escalated" {
		t.Fatal(j.State, e)
	}
	// The operator's message explicitly directing restart produces an audited restart citing it.
	directed := prophetSend(t, d, ProphetParams{Task: "alpha", Text: "RESTART with facts", Direct: []string{"RestartWithInformation"}})
	if reply := prophetAwait(t, d, "alpha", directed); strings.Contains(reply.Text, "failed") {
		t.Fatal(reply)
	}
	j, e := d.store.GetJob(ctx, "alpha")
	if e != nil {
		t.Fatal(e)
	}
	var env envelope
	if e = decodeEnvelope(j.Payload, &env); e != nil {
		t.Fatal(e)
	}
	if j.State != defaults.Workflow().Start || len(env.Job.ContextEntries) != 1 || env.Job.ContextEntries[0].Kind != "information" || env.Job.ContextEntries[0].Text != "operator supplied fresh facts" {
		t.Fatal(j.State, env.Job.ContextEntries)
	}
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "check directed restart", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		for id, want := range map[string]int{directed: 1, undirected: 0} {
			var n int
			if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='prophet' AND action='RestartWithInformation' AND message_id=?`, id).Scan(&n); e != nil {
				return e
			}
			if n != want {
				return fmt.Errorf("restart audit for %s: %d want %d", id, n, want)
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
}
