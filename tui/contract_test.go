package tui

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/tui/client"
)

// The TUI resolves the instance socket and credential exactly as the daemon
// lays them out, without importing the daemon package.
func TestPathsMatchTheDaemonLayout(t *testing.T) {
	for _, c := range [][2]string{{"/home/u/.fae/instances", "default"}, {"/var/tmp/x", "dev_2"}, {"relative", "dev"}, {"/home/u", "-bad"}, {"/home/u/./x", "dev"}} {
		layout, want := daemon.Paths(c[0], c[1])
		socket, credential, got := client.Paths(c[0], c[1])
		if (want == nil) != (got == nil) {
			t.Fatalf("%v: daemon %v, tui %v", c, want, got)
		}
		if want == nil && (socket != layout.Socket || credential != layout.Credential) {
			t.Fatalf("%v: daemon %s %s, tui %s %s", c, layout.Socket, layout.Credential, socket, credential)
		}
	}
}

// TestReadsAgainstARealDaemon runs the TUI's reads against a scripted
// daemon with its private Dolt plane (set FAE_TEST_DOLT_BINARY): every read
// decodes, the views show the admitted graph, and a task's detail comes from
// TaskGet or, on a daemon without it, from Show and TaskHistory.
func TestReadsAgainstARealDaemon(t *testing.T) {
	ctx, home, socket := startDaemon(t)
	c := client.New(socket)
	message, err := c.Message(ctx, client.Authorization("Admit a small graph for the TUI contract test.", "task"), "")
	if err != nil {
		t.Fatal(err)
	}
	item := func(id string, priority int) map[string]any {
		return map[string]any{"id": id, "title": "Task " + id, "content": "Accept " + id + ".", "priority": priority}
	}
	if _, err = socket.Call(ctx, "BacklogAdmit", map[string]any{
		"items":               []any{item("alpha", 30), item("beta", 20), item("gamma", 10)},
		"edges":               []any{map[string]any{"from": "alpha", "to": "beta", "kind": "blocking"}, map[string]any{"from": "beta", "to": "gamma", "kind": "blocking"}, map[string]any{"from": "alpha", "to": "gamma", "kind": "relates"}},
		"authorizing_message": message,
	}); err != nil {
		t.Fatal(err)
	}
	// Every read the TUI makes decodes against the real daemon.
	var snap snapshotMsg
	deadline := time.Now().Add(90 * time.Second)
	for {
		snap = fetchSnapshot(c, parts, false, time.Now)().(snapshotMsg)
		started := false
		for _, task := range snap.board.Tasks {
			started = started || (task.ID == "alpha" && task.State != "Pending")
		}
		// Topology is a cached read (up to 10 s old): wait until it lists
		// the molecule alpha runs in, so the Molecules check below is real.
		if len(snap.errs) == 0 && started && len(snap.topology.Molecules) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the daemon never started alpha in a listed molecule: errors %v, board %+v, molecules %d", snap.errs, snap.board.Tasks, len(snap.topology.Molecules))
		}
		time.Sleep(200 * time.Millisecond)
	}
	if snap.status.PID != os.Getpid() || snap.status.Instance != "tui" || snap.status.Snapshot == nil {
		t.Fatalf("status %+v", snap.status)
	}
	ids := []string{}
	for _, task := range snap.board.Tasks {
		ids = append(ids, task.ID)
	}
	for _, id := range []string{"alpha", "beta", "gamma"} {
		if !slices.Contains(ids, id) {
			t.Fatalf("board lacks %s: %v", id, ids)
		}
	}
	if len(snap.backlog.Items) != 3 || len(snap.backlog.Edges) != 3 {
		t.Fatalf("backlog %+v", snap.backlog)
	}
	// The views render what the daemon reported.
	m := New(Options{Home: home, Instance: "tui", Refresh: time.Second}, c)
	m.spawn = func(string, func() tea.Msg) tea.Cmd { return nil }
	m.width, m.height = 160, 45
	m.Update(snap)
	m.view = viewGraph
	screen := ansi.Strip(m.View().Content)
	for _, want := range []string{"alpha", "beta", "gamma", "layer 0", "layer 1", "layer 2"} {
		if !strings.Contains(screen, want) {
			t.Fatalf("graph lacks %q:\n%s", want, screen)
		}
	}
	m.view = viewMolecules
	screen = ansi.Strip(m.View().Content)
	if !strings.Contains(screen, "Molecule "+snap.topology.Molecules[0].ID) || !strings.Contains(screen, "alpha") {
		t.Fatalf("molecules view lacks molecule %s with alpha:\n%s", snap.topology.Molecules[0].ID, screen)
	}
	// A Board row that reports pending_delete reports it as a boolean, and no
	// task here has a delete pending. (The field arrives with stream cr's
	// daemon; a daemon without it omits the key.)
	raw, err := socket.Call(ctx, "Board", map[string]any{"limit": 500})
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err = json.Unmarshal(raw, &page); err != nil || len(page.Items) == 0 {
		t.Fatalf("board page %s: %v", raw, err)
	}
	for _, item := range page.Items {
		if v, ok := item["pending_delete"]; ok && string(v) != "false" {
			t.Fatalf("board row %s has pending_delete %s", item["id"], v)
		}
	}
	// A task's detail: TaskGet, or Show and TaskHistory on a daemon without it.
	detail, err := c.Detail(ctx, "alpha", m.snap.have(), false)
	if err != nil {
		t.Fatal(err)
	}
	if detail.ID != "alpha" || detail.State == "" || detail.Workflow == "" || len(detail.Trace) < 2 {
		t.Fatalf("detail %+v", detail)
	}
	out := []string{}
	for _, e := range detail.EdgesOut {
		out = append(out, e.Other+"/"+e.Kind)
	}
	slices.Sort(out)
	if !slices.Equal(out, []string{"beta/blocking", "gamma/relates"}) {
		t.Fatalf("alpha's edges out %v (detail from %s)", out, detail.Source)
	}
	if detail.Source == "TaskGet" && (detail.Title != "Task alpha" || detail.Content != "Accept alpha.") {
		t.Fatalf("TaskGet detail %+v", detail)
	}
	t.Logf("detail read from %s with %d trace entries", detail.Source, len(detail.Trace))
}

// startDaemon starts a scripted daemon with its private Dolt plane in a
// fresh git workspace, skipping without FAE_TEST_DOLT_BINARY, and stops it
// and reaps anything left under its root when the test ends.
func startDaemon(t *testing.T) (context.Context, string, *client.Socket) {
	t.Helper()
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("FAE_TEST_DOLT_BINARY required for the TUI's daemon contract tests")
	}
	root, err := os.MkdirTemp("/var/tmp", "fae-tui-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reapUnder(t, root); os.RemoveAll(root) })
	ws := filepath.Join(root, "workspace")
	if err = os.Mkdir(ws, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@localhost", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = ws
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	t.Chdir(ws)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)
	home := filepath.Join(root, "instances")
	d, err := daemon.Start(ctx, daemon.Config{Root: home, Workspace: ws, Instance: "tui", DoltBinary: binary, MaxInFlight: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = d.Close(closeCtx)
	})
	socket, err := client.NewSocket(home, "tui")
	if err != nil {
		t.Fatal(err)
	}
	return ctx, home, socket
}

// TestWritesAgainstARealDaemon drives the TUI's write dialogs against a
// scripted daemon (set FAE_TEST_DOLT_BINARY): create a task held by an
// escalated prerequisite, edit it, add an edge, then delete it, each through
// its authorizing message, and read the result back with TaskGet. A daemon
// without the live task-editing RPCs skips it.
func TestWritesAgainstARealDaemon(t *testing.T) {
	ctx, home, socket := startDaemon(t)
	c := client.New(socket)
	message, err := c.Message(ctx, client.Authorization("Admit and hold a prerequisite for the TUI write test.", "task"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = socket.Call(ctx, "BacklogAdmit", map[string]any{"items": []any{map[string]any{"id": "base", "title": "Base", "content": "Accept base.", "priority": 10}}, "authorizing_message": message}); err != nil {
		t.Fatal(err)
	}
	if _, err = socket.Call(ctx, "Escalate", map[string]any{"id": "base", "reason": "hold it for the write test", "authorizing_message": message}); err != nil {
		t.Fatal(err)
	}
	h := harnessFor(t, Options{Home: home, Instance: "tui", Write: true, Refresh: time.Second}, c)
	h.start()
	// Create, blocked by the escalated task so it stays Pending, in local
	// mode stated explicitly.
	h.press("c")
	h.typeText("delta")
	h.press("tab")
	h.typeText("Delta")
	h.press("tab")
	h.typeText("Accept delta.")
	h.focusField("mode")
	h.typeText("local")
	h.focusField("blocked_by")
	h.typeText("base")
	h.press("ctrl+s")
	h.drain()
	if strings.Contains(h.screen(), "predates live task editing") {
		t.Skip("this daemon has no TaskCreate (live task editing arrives with stream cr)")
	}
	h.contains("TaskCreate done", "created delta, state Pending")
	h.press("esc")
	task, err := c.TaskGet(ctx, "delta", false)
	if err != nil || task.Title != "Delta" || task.Content != "Accept delta." || len(task.EdgesIn) != 1 || task.EdgesIn[0].Other != "base" || task.Version == 0 {
		t.Fatalf("created task %+v %v", task, err)
	}
	// Edit it from its task view: the edit carries the version the form was
	// filled from, and a stale one is refused and offers a reload.
	h.m.openDetail("delta")
	h.drain()
	stale, err := c.TaskUpdate(ctx, client.TaskUpdateParams{ID: "delta", Title: &[]string{"Delta (CLI)"}[0], ExpectedVersion: task.Version + 100, AuthorizingMessage: message})
	if err == nil || !client.StaleVersion(err) {
		t.Fatalf("a wrong expected_version was not refused as stale: %+v %v", stale, err)
	}
	if _, err = c.TaskUpdate(ctx, client.TaskUpdateParams{ID: "delta", Title: &[]string{"Delta (CLI)"}[0], ExpectedVersion: task.Version, AuthorizingMessage: message}); err != nil {
		t.Fatal(err)
	}
	// The form was filled before the CLI's edit: the TUI's write is refused
	// as stale and nothing changes until the operator reloads, which keeps
	// the operator's typed edit against the reloaded version.
	h.press("e")
	h.drain()
	h.typeText(" v2")
	h.press("ctrl+s")
	h.drain()
	h.contains("TaskUpdate refused: task delta changed since you opened the form", "Reload the task")
	if current, err := c.TaskGet(ctx, "delta", false); err != nil || current.Title != "Delta (CLI)" {
		t.Fatalf("a stale edit was applied: %+v %v", current, err)
	}
	h.press("y")
	h.drain()
	h.contains("Edit task delta", "Delta v2")
	h.press("ctrl+s")
	h.drain()
	h.contains("TaskUpdate done", "changed title on delta")
	h.press("esc")
	// Add a relates edge.
	h.press("+")
	h.typeText("delta")
	h.press("tab", "backspace", "backspace", "backspace", "backspace", "backspace")
	h.typeText("base")
	h.press("tab", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace", "backspace")
	h.typeText("relates")
	h.press("ctrl+s")
	// base is Escalated, so the edge needs the operator's acceptance.
	h.contains("Dependent base has already been dispatched")
	h.press("y")
	h.drain()
	h.contains("BacklogEdge done", "added relates edge delta → base")
	h.press("esc")
	// Delete it: it never started, so it is withdrawn.
	h.press("D")
	h.drain()
	h.contains("It never started, so it will be withdrawn.")
	h.typeText("the write test is done")
	h.press("ctrl+s", "y")
	h.drain()
	h.contains("TaskDelete done", "delta withdrawn; its history stays")
	task, err = c.TaskGet(ctx, "delta", true)
	if err != nil || task.State != "Retired" || task.Title != "Delta v2" || task.PendingDelete != nil {
		t.Fatalf("deleted task %+v %v", task, err)
	}
	// TaskGet's pending_delete, when a daemon reports it, is null or the
	// contract object; a withdrawn task has none.
	raw, err := socket.Call(ctx, "TaskGet", map[string]any{"id": "delta", "include_retired": true})
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err = json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if v, ok := top["pending_delete"]; ok && string(v) != "null" {
		t.Fatalf("a withdrawn task reports pending_delete %s", v)
	}
	kinds := []string{}
	for _, e := range task.Edits {
		kinds = append(kinds, e.Kind)
	}
	for _, want := range []string{"task-edited", "backlog_edge_changed", "task-deleted"} {
		if !slices.Contains(kinds, want) {
			t.Fatalf("delta's edits %v lack %s", kinds, want)
		}
	}
}

// reapUnder kills processes left running under root (the plane's Dolt child
// if a close failed) so the test leaves nothing behind.
func reapUnder(t *testing.T, root string) {
	t.Helper()
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		raw, _ := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		cwd, _ := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if strings.Contains(strings.ReplaceAll(string(raw), "\x00", " "), root) || strings.HasPrefix(cwd, root+"/") || cwd == root {
			t.Logf("reaping pid %d left under %s", pid, root)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}
