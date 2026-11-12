package daemon

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	agentruntime "github.com/xleelz/fae/runtime"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func prophetAwait(t *testing.T, d *Daemon, task, id string) controlplane.ProphetEntry {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		page, e := d.store.ProphetPage(context.Background(), task, 0, 100)
		if e != nil {
			t.Fatal(e)
		}
		for _, v := range page {
			if v.ReplyTo == id {
				return v
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("Prophet reply timed out")
	return controlplane.ProphetEntry{}
}
func prophetSend(t *testing.T, d *Daemon, p ProphetParams) string {
	t.Helper()
	r, e := call(t, d, "ProphetChat", p, "")
	if e != nil {
		t.Fatal(e)
	}
	var v struct {
		ID string `json:"id"`
	}
	if e = json.Unmarshal(r.Result, &v); e != nil {
		t.Fatal(e)
	}
	return v.ID
}
func TestProphetNativeScopedChatDirectionPagingRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.ProphetRunner = func(ctx context.Context, task string, brief json.RawMessage) (json.RawMessage, error) {
		if task == "alpha" && strings.Contains(string(brief), "SECRET-BETA") {
			t.Error("other task leaked")
		}
		a := ProphetAnswer{Reply: "Task-only answer", Proposals: []string{"Consider restarting"}}
		if strings.Contains(string(brief), "DO_CONTEXT") {
			a.Action = "ResumeWithContext"
			a.Task = "alpha"
			a.Context = "additional scoped fact"
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
	for id, content := range map[string]string{"alpha": "ALPHA ONLY", "beta": "SECRET-BETA"} {
		if _, e = call(t, d, "Admit", AdmitParams{ID: id, Content: content, Tree: "tree", AuthorizingMessage: "message"}, id); e != nil {
			t.Fatal(e)
		}
	}
	id := prophetSend(t, d, ProphetParams{Task: "alpha", Text: "describe task"})
	reply := prophetAwait(t, d, "alpha", id)
	if reply.Text != "Task-only answer" {
		t.Fatal(reply)
	}
	page, e := d.store.ProphetPage(ctx, "alpha", 0, 1)
	if e != nil || len(page) != 1 || page[0].Author != "operator" || page[0].Time.IsZero() {
		t.Fatal(page, e)
	}
	page, e = d.store.ProphetPage(ctx, "alpha", page[0].Sequence, 1)
	if e != nil || len(page) != 1 || page[0].ReplyTo != id {
		t.Fatal(page, e)
	}
	denied := prophetSend(t, d, ProphetParams{Task: "alpha", Text: "DO_CONTEXT without permission"})
	reply = prophetAwait(t, d, "alpha", denied)
	if !strings.Contains(reply.Text, "refused") {
		t.Fatal(reply)
	}
	approved := prophetSend(t, d, ProphetParams{Task: "alpha", Text: "DO_CONTEXT explicitly", Direct: []string{"ResumeWithContext"}})
	reply = prophetAwait(t, d, "alpha", approved)
	if strings.Contains(reply.Text, "failed") {
		t.Fatal(reply)
	}
	j, e := d.store.GetJob(ctx, "alpha")
	if e != nil {
		t.Fatal(e)
	}
	var env envelope
	if e = decodeEnvelope(j.Payload, &env); e != nil || len(env.Job.ContextEntries) != 1 {
		t.Fatal(env.Job.ContextEntries, e)
	}
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "check Prophet citation", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='prophet' AND action='ResumeWithContext' AND message_id=?`, approved).Scan(&n); e != nil {
			return e
		}
		if n != 1 {
			return fmt.Errorf("expected one cited action, got %d", n)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	global := prophetSend(t, d, ProphetParams{Text: "summarize both tasks"})
	if reply = prophetAwait(t, d, "", global); reply.Text == "" {
		t.Fatal(reply)
	}
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	page, e = d.store.ProphetPage(ctx, "alpha", 0, 100)
	if e != nil || len(page) != 6 {
		t.Fatal(len(page), e)
	}
}
func TestProphetGuardExhaustive(t *testing.T) {
	for _, directed := range []bool{false, true} {
		for _, supported := range []bool{false, true} {
			if CanEnactProphet(directed, supported) != (directed && supported) {
				t.Fatal(directed, supported)
			}
		}
	}
}

func TestProphetInputRefusals(t *testing.T) {
	d := &Daemon{}
	for _, p := range []ProphetParams{{Text: ""}, {Text: strings.Repeat("x", 65537)}, {Text: "x", Direct: []string{"Cancel"}}, {Text: "x", Direct: []string{"ResumeWithContext"}}, {Task: "a", ActionTask: "b", Text: "x", Direct: []string{"ResumeWithContext"}}} {
		raw, _ := json.Marshal(p)
		if _, e := d.prophetRPC(context.Background(), api.Request{Method: "ProphetChat", Params: raw}); e == nil {
			t.Fatal(p)
		}
	}
}
func TestProphetTaskPrincipalIsolation(t *testing.T) {
	for _, tc := range []struct {
		method, task string
		ok           bool
	}{{"ProphetSession", "alpha", true}, {"ProphetChatRead", "alpha", true}, {"ProphetSession", "beta", false}, {"ProphetSession", "", false}, {"ProphetChat", "alpha", false}} {
		owner := make([]byte, 32)
		token := make([]byte, 32)
		token[0] = 1
		var got error
		server, _ := api.New(owner, func(ctx context.Context, r api.Request) (any, error) {
			_, _, _, got = authorizePrincipal(ctx, r)
			return nil, got
		})
		if e := server.AddPrincipal(token, api.Principal{Actor: "prophet", Task: "alpha"}); e != nil {
			t.Fatal(e)
		}
		params, _ := json.Marshal(ProphetParams{Task: tc.task})
		raw, _ := json.Marshal(api.Request{Version: 1, Method: tc.method, Params: params})
		req := httptest.NewRequest("POST", "/v1/rpc", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", hex.EncodeToString(token))
		server.ServeHTTP(httptest.NewRecorder(), req)
		if (got == nil) != tc.ok {
			t.Fatal(tc, got)
		}
	}
}

type prophetRuntimeFunc func(context.Context, agentruntime.Launch) (agentruntime.Collected, error)

func (f prophetRuntimeFunc) Run(ctx context.Context, l agentruntime.Launch) (agentruntime.Collected, error) {
	return f(ctx, l)
}
func TestProphetLiveGuardNoRoleSlotsAndPrivateContext(t *testing.T) {
	a := admissionFixture(t, 1)
	// The scheduled task has an explicit unbounded tree cap.
	a.view.Store(&scheduleView{treeRoot: map[string]string{"alpha": "alpha"}, treeLimit: map[string]int{"alpha": -1}})
	d := a.daemon
	db, mock, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	d.store = controlplane.New(db, controlplane.Options{})
	mock.ExpectQuery("SELECT").WillReturnRows(sqlmock.NewRows([]string{"provider", "model", "outcome", "score", "failures", "last_ok", "last_failure", "hold_until"}))
	d.live = &liveExecutor{guard: a, runner: prophetRuntimeFunc(func(ctx context.Context, l agentruntime.Launch) (agentruntime.Collected, error) {
		// A Prophet turn is an agent session: no role slot, no heavy-op unit.
		assertHostCounts(t, a, 0, 0)
		if !l.FreshContext || l.WorkflowID != "fae-prophet" || l.Role != "prophet" || l.SessionPolicy != "" || len(l.Profile.Allowed) != 1 || l.Profile.Allowed[0] != "handoff" {
			t.Fatal(l)
		}
		entries, e := os.ReadDir(l.Worktree)
		if e != nil || len(entries) != 0 {
			t.Fatal(entries, e)
		}
		return agentruntime.Collected{Output: validProphetReply, ProcessesRetired: true, NonExecution: true}, nil
	})}
	raw, e := d.runProphet(context.Background(), "alpha", "turn-test", json.RawMessage(`[]`))
	if e != nil || !strings.Contains(string(raw), "answer") {
		t.Fatal(string(raw), e)
	}
	assertHostCounts(t, a, 0, 0)
	// Only the health read is expected; any role reservation would fail SQL expectations.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestProphetNativeRestartDrainAndInterruptedRecovery(t *testing.T) {
	cfg, ctx := fixture(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int32
	cfg.ProphetRunner = func(ctx context.Context, task string, brief json.RawMessage) (json.RawMessage, error) {
		calls.Add(1)
		var blocks []struct {
			Message string `json:"message"`
		}
		if e := json.Unmarshal(brief, &blocks); e != nil {
			return nil, e
		}
		text := blocks[len(blocks)-1].Message
		a := ProphetAnswer{Reply: "done", Proposals: []string{}}
		switch text {
		case "restart":
			a.Action = "RestartWithInformation"
			a.Task = "alpha"
			a.Information = "new scoped instruction"
		case "wrong target":
			a.Action = "ResumeWithContext"
			a.Task = "beta"
			a.Context = "must not append"
		case "hold":
			entered <- struct{}{}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
			}
			a.Action = "ResumeWithContext"
			a.Task = "alpha"
			a.Context = "must not enact after drain"
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
	if _, e = call(t, d, "Escalate", TaskControlParams{ID: "alpha", Reason: "needs operator", AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	restart := prophetSend(t, d, ProphetParams{Task: "alpha", Text: "restart", Direct: []string{"RestartWithInformation"}})
	reply := prophetAwait(t, d, "alpha", restart)
	if strings.Contains(reply.Text, "failed") {
		t.Fatal(reply)
	}
	j, e := d.store.GetJob(ctx, "alpha")
	if e != nil || j.State == "Escalated" {
		t.Fatal(j.State, e)
	}
	if e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "inspect restart citation", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		var n int
		if e := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE actor='prophet' AND action='RestartWithInformation' AND message_id=?`, restart).Scan(&n); e != nil {
			return e
		}
		if n != 1 {
			return fmt.Errorf("restart citation count %d", n)
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	wrong := prophetSend(t, d, ProphetParams{Text: "wrong target", ActionTask: "alpha", Direct: []string{"ResumeWithContext"}})
	reply = prophetAwait(t, d, "", wrong)
	if !strings.Contains(reply.Text, "refused") {
		t.Fatal(reply)
	}
	hold := prophetSend(t, d, ProphetParams{Task: "alpha", Text: "hold", Direct: []string{"ResumeWithContext"}})
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if _, e = call(t, d, "ProphetChat", ProphetParams{Task: "alpha", Text: "busy"}, ""); e == nil {
		t.Fatal("overlapping same-scope turn")
	}
	if _, e = call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	health := d.captureControllerHealth(false)
	active := len(d.prophetActive)
	d.mu.Unlock()
	if active != 1 || health.InFlight < 1 {
		t.Fatal("active Prophet omitted", active, health)
	}
	close(release)
	reply = prophetAwait(t, d, "alpha", hold)
	if !strings.Contains(reply.Text, "draining") {
		t.Fatal(reply)
	}
	// Simulate the durable crash boundary after request persistence but before launch.
	queued, _ := json.Marshal(map[string]any{"status": "queued", "daemon_started": d.started, "task": "alpha"})
	audit := controlplane.Audit{Actor: "test", Action: "crash boundary fixture", Outcome: "ok"}
	if e = d.store.PutMarker(ctx, audit, "prophet-turn/interrupted", queued); e != nil {
		t.Fatal(e)
	}
	if _, e = d.store.AppendProphet(ctx, audit, controlplane.ProphetEntry{ID: "interrupted", Task: "alpha", Author: "operator", Text: "not replayed"}); e != nil {
		t.Fatal(e)
	}
	count := calls.Load()
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	result, e := d.prophetRead(ctx, ProphetParams{Task: "alpha", Limit: 100})
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(result)
	if !strings.Contains(string(raw), "interrupted; no automatic replay") || calls.Load() != count {
		t.Fatal(string(raw), calls.Load(), count)
	}
}
