package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
)

// saturateReadPool holds every read-pool slot with a blocked query until the
// returned release is called (or the pool's 5 s deadline frees them).
func saturateReadPool(t *testing.T, d *Daemon) func() {
	t.Helper()
	release, started := make(chan struct{}), make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go d.readPool.run(context.Background(), fmt.Sprint("saturate-", i), func(work context.Context) (any, error) {
			started <- struct{}{}
			select {
			case <-release:
			case <-work.Done():
			}
			return nil, nil
		})
	}
	for i := 0; i < 8; i++ {
		<-started
	}
	return func() { close(release) }
}

// boardPages pages a Board read to its end and encodes every page.
func boardPages(t *testing.T, d *Daemon, p BoardParams) string {
	t.Helper()
	pages := []string{}
	for p.After = ""; ; {
		r, err := call(t, d, "Board", p, "")
		if err != nil {
			t.Fatal(p, err)
		}
		var page BoardPage
		if err = json.Unmarshal(r.Result, &page); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal([]any{page.Items, page.NextCursor})
		pages = append(pages, string(raw))
		if p.After = page.NextCursor; p.After == "" || len(pages) > 400 {
			return strings.Join(pages, "\n")
		}
	}
}

// statusRows is status --tasks' rows, total and counts.
func statusRows(t *testing.T, d *Daemon, retired bool) string {
	t.Helper()
	snapshot, _ := lookup(statusBody(t, d, map[string]bool{"tasks": true, "include_retired": retired}), "snapshot")
	view := snapshot.(map[string]any)
	raw, _ := json.Marshal([]any{view["tasks"], view["total"], view["counts"]})
	return string(raw)
}

// FAE-CP-009, FAE-API-001, FAE-ROB-020: after a restart, Board and status
// --tasks read the tasks that finished before it from their columns, and a
// filtered Board reads only the finished rows it shows: none when no finished
// task can match. Once the finished-task summary is built, both answer from
// memory, reading no plane row, while the read pool is saturated. Every
// answer equals the one before the restart.
func TestFinishedReadsAfterRestartReadOnlyMatchingRows(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "done", RunID: "rare", Priority: 60, Tree: "tree", AuthorizingMessage: "message"}, "done"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "done", "Done")
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	// 300 tasks cancelled before they start: 3 in run "rare", 2 at priority
	// 60, the rest in run "common" at priority 10.
	const n = 300
	items := []BacklogItemParams{}
	for i := 0; i < n; i++ {
		run, priority := "common", 10
		if i%100 == 7 {
			run = "rare"
		}
		if i%150 == 11 {
			priority = 60
		}
		items = append(items, BacklogItemParams{AdmitParams: AdmitParams{ID: fmt.Sprintf("bulk-%03d", i), Tree: "tree", RunID: run, Priority: priority, Content: "accept"}})
	}
	for start := 0; start < n; start += 5 {
		if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: items[start : start+5], AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
	}
	for _, item := range items {
		if _, err = call(t, d, "Cancel", TaskControlParams{ID: item.ID, Reason: "not needed", AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(item.ID, err)
		}
	}
	// Each filtered board, and the finished rows it shows.
	boards := []struct {
		name  string
		p     BoardParams
		shown int
	}{
		{"run", BoardParams{RunID: "rare"}, 4},
		{"priority", BoardParams{MinPriority: 60}, 3},
		{"mode", BoardParams{Mode: "normal"}, 0},
		{"in-flight", BoardParams{InFlight: true}, 0},
		{"run-state", BoardParams{RunID: "rare", States: []string{"Done"}}, 1},
		{"first", BoardParams{Limit: 5}, 5},
	}
	before := map[string]string{}
	for _, b := range boards {
		before[b.name] = boardPages(t, d, b.p)
	}
	before["status"] = statusRows(t, d, false)
	first, err := call(t, d, "Board", BoardParams{Limit: 5}, "")
	if err != nil {
		t.Fatal(err)
	}
	before["first-page"] = string(mustRaw(t, first.Result, "items"))
	if !strings.Contains(before["run"], `"id":"bulk-207"`) || !strings.Contains(before["status"], `"id":"bulk-299"`) || strings.Count(before["first"], "\n") < 60 {
		t.Fatalf("fixture reads: %s\n%s", before["run"], before["first"][:200])
	}
	stop(t, d)
	d = nil
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	// Hold the summary so the reads page the plane in the read pool.
	hold := make(chan struct{})
	d.finished.mu.Lock()
	d.finished.hold = hold
	d.finished.mu.Unlock()
	for _, b := range boards {
		if b.p.Limit != 0 {
			continue
		}
		read := d.finished.read.Load()
		if got := boardPages(t, d, b.p); got != before[b.name] {
			t.Errorf("%s board changed across the restart:\nbefore %s\nafter  %s", b.name, before[b.name], got)
		}
		if rows := d.finished.read.Load() - read; rows != int64(b.shown) {
			t.Errorf("%s board read %d finished rows, showing %d", b.name, rows, b.shown)
		}
	}
	// A page of five reads the six rows that settle it.
	b := boards[len(boards)-1]
	read := d.finished.read.Load()
	r, err := call(t, d, "Board", b.p, "")
	if err != nil {
		t.Fatal(err)
	}
	if rows := d.finished.read.Load() - read; rows != 6 || string(mustRaw(t, r.Result, "items")) != before["first-page"] {
		t.Errorf("first board page read %d rows: %s", rows, r.Result)
	}
	// status --tasks reads every finished row, from the columns alone.
	read = d.finished.read.Load()
	if got := statusRows(t, d, false); got != before["status"] {
		t.Errorf("status --tasks changed across the restart:\nbefore %s\nafter  %s", before["status"], got)
	}
	if rows := d.finished.read.Load() - read; rows != n+1 {
		t.Errorf("status --tasks read %d finished rows, want %d", rows, n+1)
	}
	if decoded := d.finished.snapshots.Load(); decoded != 0 {
		t.Errorf("%d finished rows decoded from snapshots", decoded)
	}
	// The summary is read once; then nothing reads the plane, and a
	// saturated read pool delays no read.
	read = d.finished.read.Load()
	close(hold)
	deadline := time.Now().Add(time.Minute)
	for d.finishedSummaryBuilt() == nil {
		if time.Now().After(deadline) {
			t.Fatal("finished-task summary not built")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rows := d.finished.read.Load() - read; rows != n+1 {
		t.Errorf("summary build read %d rows, want %d", rows, n+1)
	}
	release := saturateReadPool(t, d)
	defer release()
	read = d.finished.read.Load()
	for _, b := range boards {
		begin := time.Now()
		if got := boardPages(t, d, b.p); got != before[b.name] {
			t.Errorf("%s board from the summary differs:\nbefore %s\nafter  %s", b.name, before[b.name], got)
		}
		if b.p.Limit == 0 && time.Since(begin) >= time.Second {
			t.Errorf("%s board took %s with the read pool saturated", b.name, time.Since(begin))
		}
	}
	begin := time.Now()
	if got := statusRows(t, d, false); got != before["status"] {
		t.Errorf("status --tasks from the summary differs")
	}
	if elapsed := time.Since(begin); elapsed >= time.Second {
		t.Errorf("status --tasks took %s with the read pool saturated", elapsed)
	}
	if rows := d.finished.read.Load() - read; rows != 0 {
		t.Errorf("reads after the summary read %d plane rows", rows)
	}
}

// finishedSummaryBuilt is the built summary, without starting a build.
func (d *Daemon) finishedSummaryBuilt() *finishedSummary {
	d.finished.mu.Lock()
	defer d.finished.mu.Unlock()
	return d.finished.summary
}

func mustRaw(t *testing.T, raw json.RawMessage, field string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m[field]
}

// FAE-ROB-020, FAE-CP-009: a status --tasks whose plane read waits (here on a
// saturated read pool) holds no lock a plane restart or shutdown takes: the
// child lock is free while it waits.
func TestStatusTaskRowsHoldNoChildLock(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "done", Tree: "tree", AuthorizingMessage: "message"}, "done"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "done", "Done")
	stop(t, d)
	d = nil
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	release := saturateReadPool(t, d)
	answered := make(chan error, 1)
	go func() {
		_, err := callOnce(t, d, "Status", map[string]bool{"tasks": true}, "")
		answered <- err
	}()
	// Wait until the status read is queued in the pool behind the blockers.
	deadline := time.Now().Add(3 * time.Second)
	for {
		d.readPool.mu.Lock()
		queued := len(d.readPool.flights)
		d.readPool.mu.Unlock()
		if queued > 8 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("status --tasks never queued for the plane")
		}
		time.Sleep(5 * time.Millisecond)
	}
	locked := make(chan struct{})
	go func() {
		d.childMu.Lock()
		close(locked)
		d.childMu.Unlock()
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Error("a waiting status --tasks holds the child lock")
	}
	release()
	if err := <-answered; err != nil {
		t.Fatal(err)
	}
	<-locked
}

// A finished task's Board row built from its columns is the row its snapshot
// decodes to.
func TestStoredRowFromColumnsMatchesSnapshot(t *testing.T) {
	j := controlplane.JobSnapshot{ID: "task", Version: 9, WorkflowID: "wf", WorkflowRevision: 2, TargetID: "target", TargetRevision: 3, State: "Retired", MoleculeID: "molecule"}
	payload, _ := json.Marshal(envelope{Job: engine.Job{ExecutionKind: "live", Reason: "obsolete ruling-7", EscalationKind: domain.OperatorEscalate}, Admission: AdmitParams{RunID: "run", Mode: "Normal", Priority: 40}})
	h := decodeHealthRow(payload, false)
	columns, ok := finishedBoardRow(controlplane.TerminalRow{JobSnapshot: j, Columns: true, ExecutionKind: "live", RunID: "run", Mode: "Normal", Reason: "obsolete ruling-7", Escalation: string(domain.OperatorEscalate), Priority: 40})
	if !ok || !h.decoded || columns != h.boardItem(j) {
		t.Fatalf("columns %+v\nsnapshot %+v", columns, h.boardItem(j))
	}
	j.Payload = payload
	if decoded, ok := finishedBoardRow(controlplane.TerminalRow{JobSnapshot: j}); !ok || decoded != columns {
		t.Fatalf("decoded %+v", decoded)
	}
}

// FAE-CLI-012, FAE-CP-009: status --tasks refuses finished rows whose count
// alone proves them past the response limit, reading none of them.
func TestStatusTaskRowsRefusedByCountUnread(t *testing.T) {
	empty, _ := json.Marshal(BoardItem{JobIdentity: JobIdentity{State: "Done"}})
	d := &Daemon{storedTerminal: map[string]int{"Done": api.MaxResponseBytes/len(empty) + 1}}
	d.status.Store(statusSnapshot{Updated: time.Now(), Counts: map[string]int{}, Tasks: []BoardItem{}})
	_, err := d.statusRead(context.Background(), false, true)
	var typed *api.RPCError
	if !errors.As(err, &typed) || typed.Code != "response-too-large" {
		t.Fatalf("status --tasks over %d finished tasks: %v", d.storedTerminal["Done"], err)
	}
	if view, err := d.statusRead(context.Background(), false, false); err != nil || view.(statusView).Total != d.storedTerminal["Done"] {
		t.Fatalf("status without rows: %+v %v", view, err)
	}
}
