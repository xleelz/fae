package client

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
)

// instance makes a short instance home under /var/tmp (Unix socket paths
// are limited to 108 bytes) with an owner-only run directory and credential.
func instance(t *testing.T) (home string, credential []byte) {
	t.Helper()
	home, err := os.MkdirTemp("/var/tmp", "ftui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	if err = os.MkdirAll(filepath.Join(home, "dev", "run"), 0700); err != nil {
		t.Fatal(err)
	}
	credential = make([]byte, 32)
	if _, err = rand.Read(credential); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "dev", "run", "credential"), credential, 0600); err != nil {
		t.Fatal(err)
	}
	return home, credential
}

// serve runs a real api.Server on the instance socket with handler.
func serve(t *testing.T, home string, credential []byte, handler api.Handler) {
	t.Helper()
	server, err := api.New(credential, handler)
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(filepath.Join(home, "dev", "run", "rpc.sock")); err != nil {
		t.Fatal(err)
	}
	go server.Serve()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	})
}

func TestPathsResolveTheInstanceSocketAndCredential(t *testing.T) {
	socket, credential, err := Paths("/home/u/.fae/instances", "dev")
	if err != nil || socket != "/home/u/.fae/instances/dev/run/rpc.sock" || credential != "/home/u/.fae/instances/dev/run/credential" {
		t.Fatal(socket, credential, err)
	}
	for _, bad := range [][2]string{{"relative/home", "dev"}, {"/home/u/../x", "dev"}, {"/home/u", "-dash"}, {"/home/u", "a/b"}, {"/home/u", ""}} {
		if _, _, err := Paths(bad[0], bad[1]); err == nil {
			t.Fatalf("Paths(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

func TestSocketFramesRequestsAsTheCLIDoes(t *testing.T) {
	home, credential := instance(t)
	var got api.Request
	serve(t, home, credential, func(_ context.Context, r api.Request) (any, error) {
		got = r
		return map[string]any{"items": []map[string]any{{"id": "a", "state": "Pending", "priority": 10}}, "snapshot_age_ms": 1200, "snapshot_at": "2026-10-30T12:00:00Z"}, nil
	})
	socket, err := NewSocket(home, "dev")
	if err != nil {
		t.Fatal(err)
	}
	board, err := New(socket).Board(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != api.Version || got.Method != "Board" || string(got.Params) != `{"after":"","include_retired":false,"limit":500}` {
		t.Fatalf("request %+v params %s", got, got.Params)
	}
	if len(board.Tasks) != 1 || board.Tasks[0].ID != "a" || board.SnapshotAge != 1200*time.Millisecond {
		t.Fatalf("board %+v", board)
	}
}

func TestSocketReportsTypedRefusals(t *testing.T) {
	home, credential := instance(t)
	serve(t, home, credential, func(context.Context, api.Request) (any, error) {
		return nil, &api.RPCError{Code: "snapshot-stale", Message: "board snapshot exceeds 10 seconds"}
	})
	socket, _ := NewSocket(home, "dev")
	_, err := New(socket).Board(context.Background(), false)
	if Code(err) != "snapshot-stale" || errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "board snapshot exceeds 10 seconds") {
		t.Fatalf("refusal %v (code %q)", err, Code(err))
	}
}

func TestSocketReportsAnUnreachableDaemon(t *testing.T) {
	empty, err := os.MkdirTemp("/var/tmp", "ftui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(empty) })
	socket, _ := NewSocket(empty, "dev")
	// No credential: the daemon never started.
	_, err = socket.Call(context.Background(), "Status", map[string]any{})
	if !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "is the daemon running?") {
		t.Fatalf("missing credential: %v", err)
	}
	if _, statErr := os.Stat(socket.Credential); !os.IsNotExist(statErr) {
		t.Fatal("the client created a credential")
	}
	// A credential but no listener: the daemon stopped.
	home, _ := instance(t)
	socket, _ = NewSocket(home, "dev")
	if _, err = socket.Call(context.Background(), "Status", map[string]any{}); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("no listener: %v", err)
	}
}

func TestSocketRefusesAnUnsafeOrWrongCredential(t *testing.T) {
	home, credential := instance(t)
	serve(t, home, credential, func(context.Context, api.Request) (any, error) { return map[string]any{}, nil })
	socket, _ := NewSocket(home, "dev")
	if err := os.Chmod(socket.Credential, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := socket.Call(context.Background(), "Status", map[string]any{}); err == nil || errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "unsafe credential") {
		t.Fatalf("a world-readable credential: %v", err)
	}
	wrong := make([]byte, 32)
	if err := os.Remove(socket.Credential); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socket.Credential, wrong, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := socket.Call(context.Background(), "Status", map[string]any{}); err == nil || errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("a wrong credential: %v", err)
	}
}

// callerFunc is an in-memory Caller.
type callerFunc func(method string, params json.RawMessage) (any, error)

func (f callerFunc) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	out, err := f(method, raw)
	if err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

func TestPagedReadsFollowTheCursor(t *testing.T) {
	cursors := []string{}
	c := New(callerFunc(func(method string, params json.RawMessage) (any, error) {
		var p struct {
			After string `json:"after"`
		}
		_ = json.Unmarshal(params, &p)
		cursors = append(cursors, method+":"+p.After)
		switch {
		case method == "Board" && p.After == "":
			return map[string]any{"items": []map[string]any{{"id": "a"}, {"id": "b"}}, "next_cursor": "b", "snapshot_age_ms": 10}, nil
		case method == "Board":
			return map[string]any{"items": []map[string]any{{"id": "c"}}, "snapshot_age_ms": 30}, nil
		case method == "Topology" && p.After == "":
			return map[string]any{"molecules": []map[string]any{{"molecule_id": "m1", "stages": []map[string]any{{"stage": "spec", "normal_path": true, "tasks": []string{"a"}}}}}, "next_cursor": "m1"}, nil
		case method == "Topology":
			return map[string]any{"molecules": []map[string]any{{"molecule_id": "m2"}}}, nil
		}
		return nil, errors.New("unexpected " + method)
	}))
	board, err := c.Board(context.Background(), true)
	if err != nil || len(board.Tasks) != 3 || board.Tasks[2].ID != "c" || board.SnapshotAge != 30*time.Millisecond {
		t.Fatalf("board %+v %v", board, err)
	}
	topology, err := c.Topology(context.Background())
	if err != nil || len(topology.Molecules) != 2 || topology.Molecules[0].Stages[0].Tasks[0] != "a" {
		t.Fatalf("topology %+v %v", topology, err)
	}
	if want := []string{"Board:", "Board:b", "Topology:", "Topology:m1"}; !reflect.DeepEqual(cursors, want) {
		t.Fatalf("cursors %v", cursors)
	}
}

func TestPagedReadsStopAtTheirBound(t *testing.T) {
	n := 0
	c := New(callerFunc(func(string, json.RawMessage) (any, error) {
		n++
		return map[string]any{"items": []map[string]any{{"id": "x"}}, "next_cursor": strings.Repeat("z", n)}, nil
	}))
	board, err := c.Board(context.Background(), false)
	if err != nil || !board.Truncated || n != maxPages {
		t.Fatalf("a never-ending board read %d pages: %+v %v", n, board.Truncated, err)
	}
}

func legacyCaller(calls *[]string) Caller {
	at := func(m int) string { return time.Date(2026, 10, 30, 11, m, 0, 0, time.UTC).Format(time.RFC3339) }
	return callerFunc(func(method string, params json.RawMessage) (any, error) {
		*calls = append(*calls, method)
		switch method {
		case "TaskGet":
			return nil, errors.New("actor is not permitted to perform action")
		case "Show":
			return map[string]any{"tasks": []map[string]any{{"id": "api", "state": "implement", "stage": "implement", "in_flight": true, "priority": 20, "workflow_id": "default", "workflow_revision": 3, "target_id": "local", "target_revision": 1}}}, nil
		case "TaskHistory":
			return map[string]any{"task": map[string]any{"state": "implement"}, "molecule_id": "m1",
				"transitions": []map[string]any{
					{"id": 1, "from": "Pending", "to": "spec", "attempt": 1, "note": "released", "created_at": at(0)},
					{"id": 2, "from": "spec", "to": "implement", "attempt": 1, "note": "completed d-1", "created_at": at(20)},
				},
				"messages": []map[string]any{{"id": "msg-1", "author": "operator", "text": "go", "created_at": at(5)}},
			}, nil
		}
		return nil, errors.New("unexpected " + method)
	})
}

func TestDetailFallsBackToShowAndHistoryWithoutTaskGet(t *testing.T) {
	calls := []string{}
	have := Snapshot{
		Tasks: map[string]Task{"api": {ID: "api", State: "implement", MoleculeID: "m1"}, "lib": {ID: "lib", State: "Done"}, "old": {ID: "old", State: "Cancelled"}, "app": {ID: "app", State: "Pending"}},
		Backlog: Backlog{
			Items:     []BacklogItem{{ID: "api", SourceRef: "ISSUE-1", AcceptanceDigest: "abc"}},
			Edges:     []Edge{{From: "lib", To: "api", Kind: "blocking"}, {From: "old", To: "api", Kind: "blocking"}, {From: "api", To: "app", Kind: "blocking"}},
			DeadEdges: []DeadEdge{{Edge: &Edge{From: "old", To: "api", Kind: "blocking"}, Outcome: "Cancelled"}},
		},
	}
	d, err := New(legacyCaller(&calls)).Detail(context.Background(), "api", have, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(calls, []string{"TaskGet", "Show", "TaskHistory"}) {
		t.Fatalf("calls %v", calls)
	}
	// The refusal is remembered for a minute, so polling does not repeat it.
	c := New(legacyCaller(&calls))
	clock := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return clock }
	calls = nil
	for range 2 {
		if _, err = c.Detail(context.Background(), "api", have, false); err != nil {
			t.Fatal(err)
		}
	}
	clock = clock.Add(2 * time.Minute)
	if _, err = c.Detail(context.Background(), "api", have, false); err != nil {
		t.Fatal(err)
	}
	if want := []string{"TaskGet", "Show", "TaskHistory", "Show", "TaskHistory", "TaskGet", "Show", "TaskHistory"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls %v", calls)
	}
	if d.Source != "TaskHistory" || d.State != "implement" || !d.InFlight || d.SourceRef != "ISSUE-1" || d.AcceptanceDigest != "abc" || d.Target != "local" || d.Molecule != "m1" {
		t.Fatalf("detail %+v", d)
	}
	stages := []string{}
	for _, e := range d.Trace {
		stages = append(stages, e.Stage+"/"+e.Outcome+"/"+e.To)
	}
	if want := []string{"Pending/released/spec", "spec/completed d-1/implement", "implement//"}; !reflect.DeepEqual(stages, want) {
		t.Fatalf("trace %v", stages)
	}
	if d.Trace[1].LeftAt == nil || d.Trace[2].LeftAt != nil || d.Trace[1].EnteredAt.Minute() != 0 {
		t.Fatalf("trace times %+v", d.Trace)
	}
	if want := []EdgeRef{{Other: "lib", Kind: "blocking", OtherState: "Done"}, {Other: "old", Kind: "blocking", OtherState: "Cancelled", Dead: true}}; !reflect.DeepEqual(d.EdgesIn, want) {
		t.Fatalf("edges in %+v", d.EdgesIn)
	}
	if len(d.EdgesOut) != 1 || d.EdgesOut[0].Other != "app" || len(d.Messages) != 1 || !strings.Contains(strings.Join(d.Missing, ","), "description") {
		t.Fatalf("detail %+v", d)
	}
}

func TestDetailReportsOtherTaskGetErrors(t *testing.T) {
	calls := []string{}
	c := New(callerFunc(func(method string, _ json.RawMessage) (any, error) {
		calls = append(calls, method)
		return nil, &api.RPCError{Code: "not-found", Message: "no task nope"}
	}))
	if _, err := c.Detail(context.Background(), "nope", Snapshot{}, false); Code(err) != "not-found" || !reflect.DeepEqual(calls, []string{"TaskGet"}) {
		t.Fatalf("%v %v", err, calls)
	}
	if Unsupported(Unreachable(errors.New("actor is not permitted to perform action"))) {
		t.Fatal("an unreachable daemon is not an older one")
	}
}

func TestDecodeTaskGetReadsTheContractShape(t *testing.T) {
	raw := json.RawMessage(`{
		"id":"api","title":"API layer","content":"Expose it.","priority":20,"source_ref":"ISSUE-1","write_class":"code",
		"declared_files":["a","b"],"test_filter":"^api$","test_shape":"unit",
		"workflow_id":"default","workflow_revision":3,"target_id":"local","target_revision":1,
		"state":"Escalated","stage":"review","molecule_id":"m1","escalation_kind":"AttemptCap","escalation_reason":"limit 3",
		"edges":{"in":[{"from":"lib","kind":"blocking","state":"Done"}],"out":[{"to":"app","kind":"relates","state":"Pending","dead":false}]},
		"trace":[
			{"stage":"Pending","entered_at":"2026-10-30T10:00:00Z","left_at":"2026-10-30T10:05:00Z","outcome":"released","attempt":0},
			{"kind":"task-edited","at":"2026-10-30T10:06:00Z","fields":["title"],"authorizing_message":"msg-9"},
			{"stage":"review","entered_at":"2026-10-30T10:05:00Z","attempt":2}
		]}`)
	d, err := DecodeTaskGet(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "api" || d.Title != "API layer" || d.Content != "Expose it." || d.Priority != 20 || d.SourceRef != "ISSUE-1" || d.WriteClass != "code" ||
		!reflect.DeepEqual(d.DeclaredFiles, []string{"a", "b"}) || d.TestFilter != "^api$" || d.TestShape != "unit" || d.Workflow != "default" ||
		d.WorkflowRevision != 3 || d.Target != "local" || d.TargetRevision != 1 || d.State != "Escalated" || d.Stage != "review" || d.Molecule != "m1" ||
		d.EscalationKind != "AttemptCap" || d.EscalationReason != "limit 3" || d.Source != "TaskGet" {
		t.Fatalf("detail %+v", d)
	}
	if !reflect.DeepEqual(d.EdgesIn, []EdgeRef{{Other: "lib", Kind: "blocking", OtherState: "Done"}}) || !reflect.DeepEqual(d.EdgesOut, []EdgeRef{{Other: "app", Kind: "relates", OtherState: "Pending"}}) {
		t.Fatalf("edges %+v %+v", d.EdgesIn, d.EdgesOut)
	}
	if len(d.Trace) != 2 || d.Trace[1].Attempt != 2 || d.Trace[0].LeftAt == nil || len(d.Edits) != 1 || d.Edits[0].Message != "msg-9" || d.Edits[0].Fields[0] != "title" {
		t.Fatalf("trace %+v edits %+v", d.Trace, d.Edits)
	}
}

func TestDecodeTaskGetReadsEditsDeletesAndEdgeRelease(t *testing.T) {
	raw := json.RawMessage(`{
		"id":"api","version":12,"title":"API","content":"","content_omitted":true,"content_bytes":2000000,
		"state":"Escalated","stage":"Escalated","attempts":2,"attempt_limit":3,"escalation_kind":"OperatorEscalate",
		"escalated_at":"2026-10-30T09:00:00Z","deferred":true,
		"pending_delete":{"action":"retired","retirement":"obsolete gone","requested_at":"2026-10-30T09:00:00Z","dispatch":"d-7"},
		"readiness":{"ready":false,"withheld_by":["old"]},
		"edges":{"in":[{"from":"old","to":"api","kind":"blocking","state":"Cancelled","satisfied":false},{"from":"lib","to":"api","kind":"blocking","state":"Done","satisfied":true}],"out":[]},
		"trace":[{"stage":"spec","entered_at":"2026-10-30T08:00:00Z","left_at":"2026-10-30T09:00:00Z","outcome":"TaskDelete: gone","to":"Escalated","attempt":2,
			"edits":[{"sequence":40,"at":"2026-10-30T08:30:00Z","kind":"task-edited","detail":{"fields":["title"]}}]},
			{"stage":"Escalated","entered_at":"2026-10-30T09:00:00Z","left_at":null,"outcome":"","attempt":2}],
		"edits":[
			{"sequence":40,"at":"2026-10-30T08:30:00Z","kind":"task-edited","detail":{"fields":["title"],"via":"TaskUpdate","effect":"from-next-stage","actor":"operator","message_id":"msg-4"}},
			{"sequence":41,"at":"2026-10-30T08:40:00Z","kind":"backlog_edge_changed","detail":{}},
			{"sequence":42,"at":"2026-10-30T09:00:00Z","kind":"task-deleted","detail":{"action":"retired"}}
		]}`)
	d, err := DecodeTaskGet(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.Version != 12 || !d.ContentOmitted || d.ContentBytes != 2000000 || d.Attempts != 2 || d.AttemptLimit != 3 || !d.Deferred ||
		d.EscalatedAt.Hour() != 9 || d.Readiness != "withheld by old" {
		t.Fatalf("detail %+v", d)
	}
	// An older daemon's pending_delete (action, retirement, dispatch) yields
	// the contract's fields.
	if p := d.PendingDelete; p == nil || p.Reason != "gone" || p.SupersededBy != "" || p.Status != "waiting-for-turn" || p.RequestedAt.Hour() != 9 {
		t.Fatalf("pending delete %+v", d.PendingDelete)
	}
	if len(d.EdgesIn) != 2 || !d.EdgesIn[0].Dead || d.EdgesIn[1].Dead || d.EdgesIn[1].Satisfied == nil || !*d.EdgesIn[1].Satisfied {
		t.Fatalf("edges %+v", d.EdgesIn)
	}
	if len(d.Trace) != 2 || len(d.Edits) != 3 {
		t.Fatalf("the nested edits were counted twice: %d stages, %d edits", len(d.Trace), len(d.Edits))
	}
	if e := d.Edits[0]; e.Kind != "task-edited" || e.Fields[0] != "title" || e.Message != "msg-4" || e.Effect != "from-next-stage" || e.Actor != "operator" {
		t.Fatalf("edit %+v", e)
	}
	if d.Edits[1].Kind != "backlog_edge_changed" || d.Edits[2].Kind != "task-deleted" {
		t.Fatalf("edits %+v", d.Edits)
	}
}

// The contract's pending_delete object (reason, superseded_by, requested_at,
// status) decodes as is; null and {} are no pending delete.
func TestDecodePendingDeleteReadsTheContractObject(t *testing.T) {
	for _, c := range []struct {
		raw  string
		want *PendingDelete
	}{
		{`{"reason":"replaced","superseded_by":"v2","requested_at":"2026-10-30T09:00:00Z","status":"waiting-for-turn"}`, &PendingDelete{Reason: "replaced", SupersededBy: "v2", RequestedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC), Status: "waiting-for-turn"}},
		{`{"reason":"restarted","status":"uncertain"}`, &PendingDelete{Reason: "restarted", Status: "uncertain", UncertainDispatch: true}},
		{`{"reason":"restarted","status":"waiting-for-turn","uncertain_dispatch":true}`, &PendingDelete{Reason: "restarted", Status: "waiting-for-turn", UncertainDispatch: true}},
		{`{"reason":"dropped","superseded_by":"","requested_at":"2026-10-30T09:00:00Z","status":"failed"}`, &PendingDelete{Reason: "dropped", RequestedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC), Status: "failed"}},
		{`{"reason":"old","status":"retiring"}`, &PendingDelete{Reason: "old", Status: "retiring"}},
		{`{"action":"retired","retirement":"superseded-by v2","requested_at":"2026-10-30T09:00:00Z"}`, &PendingDelete{SupersededBy: "v2", RequestedAt: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC), Status: "retiring"}},
		{`null`, nil},
		{`{}`, nil},
	} {
		d, err := DecodeTaskGet(json.RawMessage(`{"id":"a","pending_delete":` + c.raw + `}`))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(d.PendingDelete, c.want) {
			t.Fatalf("%s: %+v, want %+v", c.raw, d.PendingDelete, c.want)
		}
	}
	d, err := DecodeTaskGet(json.RawMessage(`{"id":"a","edits_truncated":true,"superseded_fields":{"declared_files":["src/old.x"],"test_filter":"^TestOld$","test_shape":"go-unit"}}`))
	if err != nil || !d.EditsTruncated || !reflect.DeepEqual(d.SupersededFields, &StructuredFields{DeclaredFiles: []string{"src/old.x"}, TestFilter: "^TestOld$", TestShape: "go-unit"}) {
		t.Fatalf("edits_truncated / superseded_fields: %+v %v", d, err)
	}
	if d, _ = DecodeTaskGet(json.RawMessage(`{"id":"a","superseded_fields":null}`)); d.PendingDelete != nil || d.EditsTruncated || d.SupersededFields != nil {
		t.Fatalf("absent fields: %+v", d)
	}
}

// The Board row's pending_delete flag decodes, and is false when absent.
func TestBoardRowCarriesThePendingDeleteFlag(t *testing.T) {
	var rows []Task
	if err := json.Unmarshal([]byte(`[{"id":"a","state":"Escalated","pending_delete":true},{"id":"b","state":"Escalated"}]`), &rows); err != nil {
		t.Fatal(err)
	}
	if !rows[0].PendingDelete || rows[1].PendingDelete {
		t.Fatalf("rows %+v", rows)
	}
}

// StaleVersion recognises the daemon's expected_version refusal and nothing
// else; ModeComplete is the daemon's submission rule.
func TestStaleVersionAndModeComplete(t *testing.T) {
	if !StaleVersion(errors.New("invalid-params: task a is at version 6, not the expected 5: read it again (TaskGet) and retry")) {
		t.Fatal("the stale-version refusal was not recognised")
	}
	for _, err := range []error{nil, errors.New("task a is Done"), errors.New("expected_version must be positive")} {
		if StaleVersion(err) {
			t.Fatalf("%v taken for a stale version", err)
		}
	}
	for _, c := range []struct {
		normal, dry, commit, want bool
	}{{false, false, false, true}, {true, true, false, true}, {true, false, true, true}, {true, false, false, false}, {true, true, true, false}, {false, true, false, false}, {false, false, true, false}} {
		if got := ModeComplete(c.normal, c.dry, c.commit); got != c.want {
			t.Fatalf("ModeComplete(%v, %v, %v) = %v", c.normal, c.dry, c.commit, got)
		}
	}
}

func TestDecodeTaskGetToleratesNearShapes(t *testing.T) {
	raw := json.RawMessage(`{
		"admission":{"id":"api","title":"T","description":"D","source":"S"},
		"workflow":{"id":"wf","revision":4},
		"escalation":{"kind":"SameGate","detail":"same failure twice"},
		"current_stage":"spec",
		"edges":[{"from":"lib","to":"api","kind":"blocking","prerequisite_state":"Done"},{"from":"api","to":"app","kind":"blocking","dependent_state":"Pending"}],
		"trace":{"stages":[{"stage":"spec","entered_at":"2026-10-30T10:00:00Z"}],"edits":[{"edited_at":"2026-10-30T10:01:00Z","changed":["priority"]}]}
	}`)
	d, err := DecodeTaskGet(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "api" || d.Title != "T" || d.Content != "D" || d.SourceRef != "S" || d.Workflow != "wf" || d.WorkflowRevision != 4 || d.Stage != "spec" ||
		d.EscalationKind != "SameGate" || d.EscalationReason != "same failure twice" {
		t.Fatalf("detail %+v", d)
	}
	if len(d.EdgesIn) != 1 || d.EdgesIn[0].OtherState != "Done" || len(d.EdgesOut) != 1 || d.EdgesOut[0].Other != "app" || d.EdgesOut[0].OtherState != "Pending" {
		t.Fatalf("edges %+v %+v", d.EdgesIn, d.EdgesOut)
	}
	if len(d.Trace) != 1 || len(d.Edits) != 1 || d.Edits[0].Fields[0] != "priority" || d.Edits[0].At.Minute() != 1 {
		t.Fatalf("trace %+v edits %+v", d.Trace, d.Edits)
	}
	if _, err := DecodeTaskGet(json.RawMessage(`null`)); err == nil {
		t.Fatal("an empty TaskGet result decoded")
	}
}

func TestWriteParamsCarryOnlyWhatTheContractNames(t *testing.T) {
	title := "New"
	raw, _ := json.Marshal(TaskUpdateParams{ID: "a", Title: &title, AuthorizingMessage: "m"})
	if string(raw) != `{"id":"a","title":"New","authorizing_message":"m"}` {
		t.Fatalf("update %s", raw)
	}
	empty := ""
	raw, _ = json.Marshal(TaskUpdateParams{ID: "a", SourceRef: &empty, AuthorizingMessage: "m"})
	if string(raw) != `{"id":"a","source_ref":"","authorizing_message":"m"}` {
		t.Fatalf("an emptied field must be sent: %s", raw)
	}
	raw, _ = json.Marshal(TaskCreateParams{TaskFields: TaskFields{ID: "a", Content: "c", Priority: 10}, Edges: []Edge{{From: "b", To: "a", Kind: "blocking"}}, AuthorizingMessage: "m"})
	if string(raw) != `{"id":"a","content":"c","priority":10,"edges":[{"from":"b","to":"a","kind":"blocking"}],"authorizing_message":"m"}` {
		t.Fatalf("create %s", raw)
	}
	// The mode, disposition, class and pins use the admission contract's names.
	raw, _ = json.Marshal(TaskCreateParams{TaskFields: TaskFields{ID: "a", Content: "c", Priority: 10, WorkflowID: "wf", Revision: 3, TargetConfig: "tg", TargetRevision: 2, TaskClass: "heavy", Mode: "Normal", DryRun: true}, AuthorizingMessage: "m"})
	if string(raw) != `{"id":"a","content":"c","priority":10,"workflow_id":"wf","revision":3,"target_config":"tg","target_revision":2,"task_class":"heavy","mode":"Normal","dry_run":true,"authorizing_message":"m"}` {
		t.Fatalf("normal-mode create %s", raw)
	}
	raw, _ = json.Marshal(TaskUpdateParams{ID: "a", Title: &title, ExpectedVersion: 5, AuthorizingMessage: "m"})
	if string(raw) != `{"id":"a","title":"New","expected_version":5,"authorizing_message":"m"}` {
		t.Fatalf("update with a version %s", raw)
	}
	raw, _ = json.Marshal(TaskDeleteParams{ID: "a", Reason: "gone", ExpectedVersion: 5, AuthorizingMessage: "m"})
	if string(raw) != `{"id":"a","reason":"gone","expected_version":5,"authorizing_message":"m"}` {
		t.Fatalf("delete with a version %s", raw)
	}
	raw, _ = json.Marshal(EdgeParams{Edge: Edge{From: "a", To: "b", Kind: "relates"}, AuthorizingMessage: "m"})
	if string(raw) != `{"from":"a","to":"b","kind":"relates","authorizing_message":"m"}` {
		t.Fatalf("edge %s", raw)
	}
	raw, _ = json.Marshal(TaskDeleteParams{ID: "a", Reason: "gone", AuthorizingMessage: "m"})
	if string(raw) != `{"id":"a","reason":"gone","authorizing_message":"m"}` {
		t.Fatalf("delete %s", raw)
	}
	raw, _ = json.Marshal(TaskDeleteParams{ID: "a", Reason: "replaced", SupersededBy: "b", AuthorizingMessage: "m"})
	if string(raw) != `{"id":"a","reason":"replaced","superseded_by":"b","authorizing_message":"m"}` {
		t.Fatalf("delete with a successor %s", raw)
	}
	var direction struct {
		Text      string `json:"text"`
		Authorize struct {
			Classes []string `json:"classes"`
		} `json:"authorize"`
	}
	if err := json.Unmarshal([]byte(Authorization("Edit a.", "task")), &direction); err != nil || direction.Text != "Edit a." || !reflect.DeepEqual(direction.Authorize.Classes, []string{"task"}) {
		t.Fatalf("authorization %+v %v", direction, err)
	}
}

func TestMessageReturnsTheRecordedID(t *testing.T) {
	var sent json.RawMessage
	c := New(callerFunc(func(method string, params json.RawMessage) (any, error) {
		sent = params
		return map[string]any{"id": "msg-1", "thread_root": "msg-1"}, nil
	}))
	id, err := c.Message(context.Background(), "hello", "")
	if err != nil || id != "msg-1" || string(sent) != `{"text":"hello"}` {
		t.Fatalf("%q %v %s", id, err, sent)
	}
}

func TestReadinessInWords(t *testing.T) {
	for raw, want := range map[string]string{
		`{"state":"Pending","ready":true,"withheld_by":[]}`:           "ready",
		`{"state":"Pending","ready":false,"withheld_by":["a: spec"]}`: "withheld by a: spec",
		`{"state":"Pending","ready":false,"deferred":true}`:           "deferred",
		`{"state":"implement","ready":false}`:                         "not ready (implement)",
		`{"other":1}`:                                                 `{"other":1}`,
	} {
		if got := readinessText(json.RawMessage(raw)); got != want {
			t.Fatalf("%s: %q, want %q", raw, got, want)
		}
	}
}

func TestDecodeTaskGetExtendedWave19To21Fields(t *testing.T) {
	raw := json.RawMessage(`{
		"id":"task-ext",
		"follow_up_of":"task-prev",
		"turn_budget":{"wall_seconds":600,"max_assistant_turns":12,"max_turns":20,"on_exhaustion":"park"},
		"read_only_files":["docs/spec.md","README.md"],
		"reads":{"allow":["docs/**"],"deny":["secrets/**"]},
		"notifications":[
			{"id":"n-1","kind":"info","delivery":"next-turn","summary":"dep updated","from_task":"t-0","actor":"operator","created_at":"2026-10-30T10:00:00Z"}
		],
		"approved_scope":["tui/client/task.go","tui/client/reads.go"],
		"pending_request":{"kind":"scope-widen","paths":["tui/client/writes.go"],"reason":"need write RPCs"},
		"request_counts":{"scope-widen":1,"red-rework":2},
		"turn_checkpoints":{
			"implement":{"provider_session":"sess-99","worktree_tree":"tree-abc","stage":"implement","assistant_turns":5,"wall_seconds":120,"updated_at":"2026-10-30T10:05:00Z"}
		},
		"fan":{
			"stage":"fan-impl",
			"join":"all",
			"parallel":true,
			"branches":[
				{"id":"b1","status":"running","stage":"fan-impl","in_flight":true,"write_scope":["a.go"],"commit_sha":"sha1","branch_ref":"refs/fae/b1","tree":"t1","base_tree":"bt1","item":"part-a"},
				{"id":"b2","status":"completed","stage":"fan-impl","in_flight":false,"write_scope":["b.go"],"commit_sha":"sha2","tree":"t2","base_tree":"bt1","item":"part-b"}
			]
		},
		"deny_paths":[".env","credentials.json"]
	}`)
	d, err := DecodeTaskGet(raw)
	if err != nil {
		t.Fatal(err)
	}
	if d.FollowUpOf != "task-prev" {
		t.Fatalf("FollowUpOf = %q, want task-prev", d.FollowUpOf)
	}
	if d.TurnBudget == nil || d.TurnBudget.WallSeconds != 600 || d.TurnBudget.MaxAssistantTurns != 12 || d.TurnBudget.MaxTurns != 20 || d.TurnBudget.OnExhaustion != "park" {
		t.Fatalf("TurnBudget = %+v", d.TurnBudget)
	}
	if !reflect.DeepEqual(d.ReadOnlyFiles, []string{"docs/spec.md", "README.md"}) {
		t.Fatalf("ReadOnlyFiles = %v", d.ReadOnlyFiles)
	}
	if d.Reads == nil || !reflect.DeepEqual(d.Reads.Allow, []string{"docs/**"}) || !reflect.DeepEqual(d.Reads.Deny, []string{"secrets/**"}) {
		t.Fatalf("Reads = %+v", d.Reads)
	}
	if len(d.Notifications) != 1 || d.Notifications[0].ID != "n-1" || d.Notifications[0].Summary != "dep updated" || d.Notifications[0].FromTask != "t-0" || d.Notifications[0].Actor != "operator" {
		t.Fatalf("Notifications = %+v", d.Notifications)
	}
	if !reflect.DeepEqual(d.ApprovedScope, []string{"tui/client/task.go", "tui/client/reads.go"}) {
		t.Fatalf("ApprovedScope = %v", d.ApprovedScope)
	}
	if d.PendingRequest == nil || d.PendingRequest.Kind != "scope-widen" || !reflect.DeepEqual(d.PendingRequest.Paths, []string{"tui/client/writes.go"}) || d.PendingRequest.Reason != "need write RPCs" {
		t.Fatalf("PendingRequest = %+v", d.PendingRequest)
	}
	if !reflect.DeepEqual(d.RequestCounts, map[string]int{"scope-widen": 1, "red-rework": 2}) {
		t.Fatalf("RequestCounts = %v", d.RequestCounts)
	}
	cp, ok := d.TurnCheckpoints["implement"]
	if !ok || cp.ProviderSession != "sess-99" || cp.WorktreeTree != "tree-abc" || cp.AssistantTurns != 5 || cp.WallSeconds != 120 {
		t.Fatalf("TurnCheckpoints = %+v", d.TurnCheckpoints)
	}
	if d.Fan == nil || d.Fan.Stage != "fan-impl" || d.Fan.Join != "all" || !d.Fan.Parallel || len(d.Fan.Branches) != 2 || !d.Fan.Branches[0].InFlight || d.Fan.Branches[0].Item != "part-a" || d.Fan.Branches[1].CommitSHA != "sha2" {
		t.Fatalf("Fan = %+v", d.Fan)
	}
	if !reflect.DeepEqual(d.DenyPaths, []string{".env", "credentials.json"}) {
		t.Fatalf("DenyPaths = %v", d.DenyPaths)
	}

	// Verify nested job / admission shapes also decode cleanly.
	nestedRaw := json.RawMessage(`{
		"id":"task-nested",
		"admission":{
			"read_only_files":["ro.txt"],
			"reads":{"allow":["pkg/**"]}
		},
		"job":{
			"predecessor_task":"task-0",
			"budget":{"wall_seconds":300,"max_turns":8},
			"notifications":[{"id":"n-2","kind":"directive","delivery":"interrupt","body":"stop and pivot","from_task_id":"t-9","relayed_by":"op"}],
			"approved_scope":["pkg/a.go"],
			"pending_request":{"kind":"red-rework","test_file":"pkg/a_test.go","reason":"fix assertion"},
			"request_counts":{"red-rework":1},
			"turn_checkpoints":{"spec":{"provider_session":"sess-1","stage_id":"spec","assistant_turns":2}},
			"active_fan":{"stage_id":"fan-stage","join_policy":"first","parallel":false,"branches":[{"id":"b0","status":"running","in_flight":{"id":"disp-1"},"worktree_tree":"wt-1","item":{"k":"v"}}]},
			"deny_paths":["secret.key"]
		}
	}`)
	dn, err := DecodeTaskGet(nestedRaw)
	if err != nil {
		t.Fatal(err)
	}
	if dn.FollowUpOf != "task-0" || dn.TurnBudget == nil || dn.TurnBudget.WallSeconds != 300 || !reflect.DeepEqual(dn.ReadOnlyFiles, []string{"ro.txt"}) || dn.Reads == nil || len(dn.Notifications) != 1 || dn.Notifications[0].Summary != "stop and pivot" || dn.Notifications[0].FromTask != "t-9" || dn.Notifications[0].Actor != "op" {
		t.Fatalf("nested job decode: %+v", dn)
	}
	if dn.PendingRequest == nil || dn.PendingRequest.TestFile != "pkg/a_test.go" || dn.TurnCheckpoints["spec"].Stage != "spec" || dn.Fan == nil || dn.Fan.Stage != "fan-stage" || dn.Fan.Join != "first" || !dn.Fan.Branches[0].InFlight || dn.Fan.Branches[0].Tree != "wt-1" || !reflect.DeepEqual(dn.DenyPaths, []string{"secret.key"}) {
		t.Fatalf("nested job fan/cp/deny decode: %+v", dn)
	}
}

func TestExtendedDaemonReadMethods(t *testing.T) {
	ctx := context.Background()
	c := New(callerFunc(func(method string, params json.RawMessage) (any, error) {
		switch method {
		case "Status":
			return map[string]any{
				"instance":       "dev",
				"pid":            4242,
				"uptime_seconds": 99.5,
				"adapter":        "codex",
				"snapshot": map[string]any{
					"active": 1,
					"total":  2,
					"ticket_outbox": map[string]any{
						"depth":    3,
						"failures": 2,
						"error":    "rate limit",
					},
				},
				"provider_pools": map[string]any{
					"codex:default": map[string]any{
						"pool": map[string]any{
							"id":                  "codex:default",
							"provider":            "codex",
							"credential_identity": "default",
							"capacity":            4,
							"admission_limit":     3,
							"holders":             []map[string]any{{"task_id": "t-1"}},
							"waiting":             []string{"t-2", "t-3"},
						},
						"quota": map[string]any{
							"reason":       "429 reset",
							"cut_off_task": "t-1",
						},
						"pause": map[string]any{
							"scope":  "pool",
							"actor":  "operator",
							"reason": "maintenance",
						},
					},
				},
				"dispatch_pauses": []map[string]any{
					{"scope": "instance", "target": "", "actor": "operator", "reason": "hold"},
				},
				"panes": map[string]any{
					"live":  1,
					"limit": 24,
					"owned": []string{"fae-t1-impl"},
				},
				"provider_health": map[string]any{
					"bindings": 1,
					"eligible": 0,
					"ineligible": []map[string]any{
						{"provider": "codex", "model": "gpt-5", "score": 20, "eligible": false, "outcome": "rate-limited", "failures": 2},
					},
				},
			}, nil
		case "Leases":
			return map[string]any{
				"provider_pools": map[string]any{
					"claude:team": map[string]any{
						"pool": map[string]any{
							"id":                  "claude:team",
							"provider":            "claude",
							"credential_identity": "team",
							"capacity":            2,
							"admission_limit":     2,
							"holders":             []string{"t-9"},
							"waiting":             1,
						},
					},
				},
			}, nil
		case "ProviderHealth":
			return map[string]any{
				"bindings": []map[string]any{
					{"provider": "claude", "model": "opus", "binding_id": "claude:opus", "score": 100, "effective_score": 100, "eligible": true, "outcome": "ok"},
				},
			}, nil
		case "DispatchPauseList":
			return map[string]any{
				"pauses": []map[string]any{
					{"scope": "workflow", "target": "default", "actor": "operator", "reason": "upgrade"},
				},
			}, nil
		case "Panes":
			return map[string]any{
				"panes": []map[string]any{
					{"id": "pane-1", "task_id": "t-1", "role": "coder", "stage": "implement", "provider": "codex", "model": "gpt-5", "pid": 1234},
				},
			}, nil
		case "AnalyzeSession":
			return map[string]any{
				"task": "t-1",
				"metrics": []map[string]any{
					{"name": "turns", "role": "coder", "value": 3},
					{"name": "turns", "role": "reviewer", "value": 1},
					{"name": "stage_wall_seconds", "stage": "implement", "value": 45.5},
					{"name": "retries", "class": "gate", "value": 2},
					{"name": "parks", "class": "quota", "value": 1},
					{"name": "lease_wait_seconds", "class": "codex", "value": 12.25},
				},
			}, nil
		case "TargetConfigList":
			return map[string]any{
				"targets": []map[string]any{
					{
						"id":              "main",
						"revision":        2,
						"read_roots":      map[string]any{"sdk": map[string]any{"name": "sdk", "path": "/opt/sdk", "commit": "abc"}},
						"deny_paths":      []string{"secret/*"},
						"document_globs":  []string{"docs/**/*.md"},
						"document_checks": []map[string]any{{"id": "markdown-lint"}},
						"commit_identity": map[string]any{"name": "Fae Bot", "email": "fae@example.com"},
						"git_push":        map[string]any{"mode": "on-land", "remote": "origin", "branch": "main"},
					},
				},
			}, nil
		case "Events":
			return map[string]any{
				"events": []map[string]any{
					{"id": 77, "kind": "quota-set", "task": "t-1", "actor": "operator", "summary": "quota updated"},
				},
			}, nil
		case "ProvenanceChain":
			return map[string]any{
				"chain": []map[string]any{
					{"dispatch_id": "d-1", "stage": "implement", "role": "coder", "outcome": "completed", "commit_sha": "deadbeef", "read_roots": map[string]string{"sdk": "abc"}, "reads": map[string]any{"allow": []string{"**"}}},
				},
			}, nil
		case "LandStatus":
			return map[string]any{
				"task_id":     "t-1",
				"landed":      true,
				"target_head": "refs/heads/main",
				"task_commit": "deadbeef",
				"reason":      "landed cleanly",
			}, nil
		case "SymphonyState":
			return nil, &api.RPCError{Code: "unknown-method", Message: "unknown method SymphonyState"}
		case "Board":
			return map[string]any{
				"items": []map[string]any{
					{"id": "FAE-101", "state": "implement", "priority": 1, "in_flight": true},
				},
			}, nil
		case "BacklogShow":
			return map[string]any{
				"items": []map[string]any{
					{"id": "FAE-101", "source_ref": "FAE-101", "state": "implement"},
				},
			}, nil
		}
		return nil, errors.New("unexpected method " + method)
	}))

	st, err := c.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ProviderPools) != 1 || st.ProviderPools[0].ID != "codex:default" || len(st.ProviderPools[0].Holders) != 1 || st.ProviderPools[0].Holders[0] != "t-1" || st.ProviderPools[0].Waiting != 2 || !st.ProviderPools[0].Paused || st.ProviderPools[0].CutOffTaskID != "t-1" {
		t.Fatalf("Status.ProviderPools = %+v", st.ProviderPools)
	}
	if len(st.DispatchPauses) != 1 || !st.DispatchPauses[0].Paused || st.DispatchPauses[0].Scope != "instance" {
		t.Fatalf("Status.DispatchPauses = %+v", st.DispatchPauses)
	}
	if len(st.Panes) != 1 || st.Panes[0].ID != "fae-t1-impl" {
		t.Fatalf("Status.Panes = %+v", st.Panes)
	}
	if len(st.ProviderHealth) != 1 || st.ProviderHealth[0].Provider != "codex" || st.ProviderHealth[0].Eligible {
		t.Fatalf("Status.ProviderHealth = %+v", st.ProviderHealth)
	}
	if st.TicketOutbox == nil || st.TicketOutbox.Pending != 3 || st.TicketOutbox.MaxAttempts != 2 || st.TicketOutbox.LastError != "rate limit" {
		t.Fatalf("Status.TicketOutbox = %+v", st.TicketOutbox)
	}

	pools, err := c.Leases(ctx)
	if err != nil || len(pools) != 1 || pools[0].ID != "claude:team" || pools[0].Waiting != 1 {
		t.Fatalf("Leases = %+v, %v", pools, err)
	}
	health, err := c.ProviderHealth(ctx)
	if err != nil || len(health) != 1 || health[0].BindingID != "claude:opus" || !health[0].Eligible {
		t.Fatalf("ProviderHealth = %+v, %v", health, err)
	}
	pauses, err := c.DispatchPauses(ctx)
	if err != nil || len(pauses) != 1 || pauses[0].Scope != "workflow" || !pauses[0].Paused {
		t.Fatalf("DispatchPauses = %+v, %v", pauses, err)
	}
	panes, err := c.Panes(ctx)
	if err != nil || len(panes) != 1 || panes[0].TaskID != "t-1" || panes[0].PID != 1234 {
		t.Fatalf("Panes = %+v, %v", panes, err)
	}
	analysis, err := c.AnalyzeSession(ctx, "t-1")
	if err != nil || analysis.TaskID != "t-1" || analysis.Turns != 4 || analysis.TurnsPerRole["coder"] != 3 || analysis.WallSeconds != 45.5 || analysis.Retries != 2 || analysis.Parks != 1 || analysis.LeaseWaitSeconds != 12.25 {
		t.Fatalf("AnalyzeSession = %+v, %v", analysis, err)
	}
	targets, err := c.TargetConfigs(ctx)
	if err != nil || len(targets) != 1 || targets[0].ID != "main" || targets[0].ReadRoots["sdk"].Path != "/opt/sdk" || targets[0].CommitName != "Fae Bot" || targets[0].PushMode != "on-land" || !reflect.DeepEqual(targets[0].DocumentChecks, []string{"markdown-lint"}) {
		t.Fatalf("TargetConfigs = %+v, %v", targets, err)
	}
	events, err := c.Events(ctx, 50)
	if err != nil || len(events) != 1 || events[0].ID != 77 || events[0].Kind != "quota-set" {
		t.Fatalf("Events = %+v, %v", events, err)
	}
	chain, err := c.ProvenanceChain(ctx, "t-1")
	if err != nil || len(chain) != 1 || chain[0].DispatchID != "d-1" || chain[0].CommitSHA != "deadbeef" || chain[0].ReadRoots["sdk"] != "abc" || chain[0].Reads == nil {
		t.Fatalf("ProvenanceChain = %+v, %v", chain, err)
	}
	land, err := c.LandStatus(ctx, "t-1")
	if err != nil || !land.Landed || land.TaskCommit != "deadbeef" {
		t.Fatalf("LandStatus = %+v, %v", land, err)
	}
	sym, err := c.SymphonyState(ctx)
	if err != nil || sym.Outbox.Pending != 3 || len(sym.Running) != 1 || sym.Running[0].Identifier != "FAE-101" {
		t.Fatalf("SymphonyState = %+v, %v", sym, err)
	}
}

func TestExtendedDaemonWriteMethods(t *testing.T) {
	ctx := context.Background()
	var calls []string
	msgSeq := 0
	c := New(callerFunc(func(method string, params json.RawMessage) (any, error) {
		calls = append(calls, method)
		if method == "Message" {
			msgSeq++
			return map[string]any{"id": "msg-auto"}, nil
		}
		if method == "FollowUp" {
			return map[string]any{"id": "t-2", "follow_up_of": "t-1", "state": "Pending", "resumed_session": "sess-1"}, nil
		}
		if method == "SymphonyRefresh" {
			return nil, &api.RPCError{Code: "unknown-method", Message: "unknown method SymphonyRefresh"}
		}
		if method == "TrackerSync" {
			return nil, &api.RPCError{Code: "unknown-method", Message: "unknown method TrackerSync"}
		}
		var m map[string]any
		_ = json.Unmarshal(params, &m)
		if method != "Notify" && m["authorizing_message"] != "msg-auto" && m["authorizing_message"] != "msg-explicit" {
			t.Fatalf("%s missing authorizing_message: %s", method, params)
		}
		return map[string]any{"ok": true}, nil
	}))

	if err := c.Notify(ctx, NotifyParams{Task: "t-1", Kind: "info", Delivery: "next-turn", Summary: "heads up"}); err != nil {
		t.Fatal(err)
	}
	fu, err := c.FollowUp(ctx, FollowUpParams{Task: "t-1", Prompt: "continue", AuthorizingMessage: "msg-explicit"})
	if err != nil || fu.ID != "t-2" || fu.PredecessorTask != "t-1" || fu.ResumedSession != "sess-1" {
		t.Fatalf("FollowUp = %+v, %v", fu, err)
	}
	if err := c.PoolResize(ctx, PoolResizeParams{Pool: "codex:default", Cap: 6}); err != nil {
		t.Fatal(err)
	}
	if err := c.QuotaSet(ctx, QuotaSetParams{Pool: "codex:default", ResetAt: "2026-10-30T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if err := c.QuotaClear(ctx, QuotaClearParams{Pool: "codex:default"}); err != nil {
		t.Fatal(err)
	}
	if err := c.DispatchPause(ctx, DispatchPauseParams{Scope: "instance", Reason: "maint"}); err != nil {
		t.Fatal(err)
	}
	if err := c.DispatchUnpause(ctx, DispatchPauseParams{Scope: "instance"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Decide(ctx, DecideParams{Task: "t-1", Approved: true, Note: "lgtm"}); err != nil {
		t.Fatal(err)
	}
	if err := c.ResolveUncertain(ctx, ResolveUncertainParams{Task: "t-1", Action: "retry", Note: "safe"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Lifecycle(ctx, LifecycleParams{Task: "t-1", Verb: "ResumeWithContext", Context: "extra info"}); err != nil {
		t.Fatal(err)
	}
	if err := c.SymphonyRefresh(ctx); err != nil {
		t.Fatal(err)
	}
	if msgSeq != 8 {
		t.Fatalf("expected 8 auto-minted authorizing messages, got %d (calls: %v)", msgSeq, calls)
	}
}
