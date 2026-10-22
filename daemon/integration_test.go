package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

func fixture(t *testing.T) (Config, context.Context) {
	t.Helper()
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("FAE_TEST_DOLT_BINARY required for isolated daemon integration")
	}
	base := "/var/tmp"
	if runtime.GOOS == "darwin" {
		base = "/private/tmp"
	}
	root, err := os.MkdirTemp(base, "fae-daemon-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	ws := filepath.Join(root, "workspace")
	if err = os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(ws)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	return Config{Root: filepath.Join(root, "instances"), Workspace: ws, Instance: "test", DoltBinary: binary, MaxInFlight: 2}, ctx
}
func call(t *testing.T, d *Daemon, method string, p any, key string) (api.Response, error) {
	t.Helper()
	raw, _ := json.Marshal(p)
	token, err := os.ReadFile(d.layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	return api.Call(context.Background(), d.layout.Socket, token, api.Request{Version: 1, Method: method, Params: raw, Key: key})
}
func authorize(t *testing.T, d *Daemon) {
	t.Helper()
	if _, err := call(t, d, "Message", map[string]string{"id": "message", "text": "{\"text\":\"Run local scripted tasks and stop this test instance.\",\"authorize\":{\"classes\":[\"task\",\"governance\",\"runtime\",\"schedule\",\"controller\"]}}"}, ""); err != nil {
		t.Fatal(err)
	}
}
func stop(t *testing.T, d *Daemon) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Close(ctx); err != nil {
		t.Error(err)
	}
}
func awaitState(t *testing.T, d *Daemon, id, state string) controlplane.JobSnapshot {
	t.Helper()
	// Native race-instrumented workflows now persist intent/start/result for
	// every local effect. Allow their full pipeline to finish within the
	// fixture's 90-second context, rather than a timing-sensitive 40s bound.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		j, err := d.store.GetJob(context.Background(), id)
		if err == nil && j.State == state {
			return j
		}
		select {
		case <-d.done:
			t.Fatalf("daemon stopped: %v", d.err)
		default:
		}
		time.Sleep(30 * time.Millisecond)
	}
	j, err := d.store.GetJob(context.Background(), id)
	if err == nil && j.State == state {
		return j
	}
	t.Fatalf("task %s did not reach %s: state=%s err=%v payload=%s", id, state, j.State, err, j.Payload)
	return j
}
func TestDaemonScriptedLifecycle(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	if foreign, err := controlplane.Open(ctx, fmt.Sprintf("root@tcp(127.0.0.1:%d)/plane?parseTime=true", d.child.Port()), controlplane.Options{}); err == nil {
		foreign.Close()
		t.Fatal("passwordless root account accepted")
	}

	if _, err := Start(ctx, cfg); err == nil {
		t.Fatal("second daemon accepted")
	}
	if _, err := call(t, d, "Admit", AdmitParams{ID: "unauthorized", Tree: "tree"}, ""); err == nil {
		t.Fatal("missing authority accepted")
	}
	authorize(t, d)
	for _, id := range []string{"one", "two"} {
		p := AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}
		if _, err := call(t, d, "Admit", p, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"one", "two"} {
		snapshot := awaitState(t, d, id, "Done")
		var e envelope
		if err := json.Unmarshal(snapshot.Payload, &e); err != nil {
			t.Fatal(err)
		}
		if len(e.Job.Provenance) < 20 {
			t.Fatalf("incomplete artifact chain: %d", len(e.Job.Provenance))
		}
	}
	// Summary history follows durable event cursors and returns artifact references
	// without embedding the workflow document or full evidence bodies.
	cursor := int64(0)
	historyCount := 0
	referenceCount := 0
	for {
		reply, err := call(t, d, "History", HistoryParams{ID: "one", After: cursor, Limit: 5, Summary: true}, "")
		if err != nil {
			t.Fatal(err)
		}
		var page HistoryPage
		if err = json.Unmarshal(reply.Result, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Events) > 5 || len(reply.Result) > readPageBytes {
			t.Fatal("history page bound")
		}
		for _, event := range page.Events {
			if event.Sequence <= cursor {
				t.Fatal("duplicate history event")
			}
			cursor = event.Sequence
		}
		historyCount += len(page.Events)
		referenceCount += len(page.ArtifactReferences)
		if page.NextCursor == 0 {
			break
		}
		if page.NextCursor != cursor {
			t.Fatal("bad history cursor")
		}
	}
	expectedHistory, err := d.store.JobEvents(ctx, "one", 0, 1000)
	if err != nil || historyCount != len(expectedHistory) {
		t.Fatal("lost history events")
	}
	if referenceCount < 20 {
		t.Fatalf("missing artifact references: %d", referenceCount)
	}
	boardReply, err := call(t, d, "Board", BoardParams{Limit: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	var board BoardPage
	if err = json.Unmarshal(boardReply.Result, &board); err != nil || len(board.Items) != 1 || board.NextCursor == "" {
		t.Fatal("bounded board RPC")
	}
	original, err := call(t, d, "Admit", AdmitParams{ID: "one", Tree: "tree", AuthorizingMessage: "message"}, "one")
	if err != nil {
		t.Fatal(err)
	}
	var response controlplane.JobSnapshot
	_ = json.Unmarshal(original.Result, &response)
	if response.Version != 1 {
		t.Fatal("replay did not return original admission")
	}
	current, err := d.store.GetJob(ctx, "one")
	if err != nil || current.State != "Done" {
		t.Fatal("replay rewound job")
	}
	events, err := d.store.Events(ctx, 0, 1000)
	if err != nil || len(events) < 20 {
		t.Fatalf("events %d %v", len(events), err)
	}
	resumed, err := d.store.Events(ctx, events[10].Sequence, 1000)
	if err != nil || len(resumed) != len(events)-11 {
		t.Fatal("event resume lost records")
	}
	token, _ := os.ReadFile(d.layout.Credential)
	streamctx, streamcancel := context.WithTimeout(ctx, 3*time.Second)
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", d.layout.Socket)
	}}
	client := &http.Client{Transport: transport}
	request, _ := http.NewRequestWithContext(streamctx, "GET", fmt.Sprintf("http://unix/v1/events?after=%d", events[10].Sequence), nil)
	request.Header.Set("Authorization", hex.EncodeToString(token))
	streamed, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	var firstEvent controlplane.Event
	if err = json.NewDecoder(streamed.Body).Decode(&firstEvent); err != nil {
		t.Fatal(err)
	}
	if firstEvent.Sequence != events[11].Sequence {
		t.Fatal("stream resume sequence wrong")
	}
	streamed.Body.Close()
	streamcancel()
	transport.CloseIdleConnections()
	if _, err = call(t, d, "Status", nil, ""); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "one", "Done")
}
func TestDaemonScriptedRestartInFlight(t *testing.T) {
	cfg, ctx := fixture(t)
	script := defaults.HappyScript()
	first := defaults.Workflow().Start
	steps := script[first]
	if len(steps) == 0 {
		t.Fatal("first stage lacks scripted step")
	}
	steps[0].Delay = 500 * time.Millisecond
	script[first] = steps
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "restart", Tree: "tree", AuthorizingMessage: "message"}, "restart"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var token string
	for time.Now().Before(deadline) {
		s, err := d.store.GetJob(ctx, "restart")
		if err == nil {
			var e envelope
			_ = json.Unmarshal(s.Payload, &e)
			if e.Job.InFlight != nil {
				token = e.Job.InFlight.ID
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if token == "" {
		stop(t, d)
		t.Fatal("no durable dispatch")
	}
	stop(t, d)
	cfg.Executor = engine.NewScripted(script)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	snapshot := awaitState(t, d, "restart", "Done")
	var e envelope
	_ = json.Unmarshal(snapshot.Payload, &e)
	count := 0
	for _, p := range e.Job.Provenance {
		if p.DispatchID == token {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("replayed token artifact count %d", count)
	}
}
func TestDaemonForeignPortRefused(t *testing.T) {
	cfg, ctx := fixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	cfg.Port = listener.Addr().(*net.TCPAddr).Port
	if d, err := Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("adopted foreign server")
	}
}

type faultyExecutor struct{ good *engine.Scripted }

func (f faultyExecutor) Execute(ctx context.Context, d engine.Dispatch) (engine.Result, error) {
	r, err := f.good.Execute(ctx, d)
	if d.TaskID == "bad" {
		r.Session = "foreign-session"
	}
	return r, err
}
func TestDaemonFaultyTaskDoesNotStopSibling(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = faultyExecutor{engine.NewScripted(defaults.HappyScript())}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, id := range []string{"bad", "good"} {
		if _, err := call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	awaitState(t, d, "bad", "Escalated")
	awaitState(t, d, "good", "Done")
	if _, err := call(t, d, "Status", nil, ""); err != nil {
		t.Fatal(err)
	}
}
func TestDaemonDoltChildCrashRecovers(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	pid := d.child.PID()
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err = process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(15 * time.Second)
	recovered := false
	for time.Now().Before(deadline) {
		if _, err := call(t, d, "Status", nil, ""); err != nil {
			t.Fatal("status unavailable during recovery", err)
		}
		raw, err := d.store.GetMarker(ctx, "plane-recovery")
		if err == nil && len(raw) > 0 {
			recovered = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !recovered {
		t.Fatal("owned Dolt child not recovered")
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "after-crash", Tree: "tree", AuthorizingMessage: "message"}, "after-crash"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "after-crash", "Done")
}
