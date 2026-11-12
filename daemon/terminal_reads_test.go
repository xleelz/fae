package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

// FAE-API-001, FAE-OPS-011, FAE-CLI-012/-013/-015: startup materialises only
// active jobs (FAE-CP-009), so every read that can name a finished task reads
// it from the plane. A restart changes none of them: Board and status --tasks
// (with and without the retired opt-in), a backlog edge's readiness effect
// against finished prerequisites, and the terminal refusal of resume.
func TestRestartLeavesTerminalReadsUnchanged(t *testing.T) {
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
	for _, id := range []string{"done-a", "done-b"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, RunID: "finished", Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"done-a", "done-b"} {
		awaitState(t, d, id, "Done")
	}
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	items := []BacklogItemParams{}
	for _, id := range []string{"cancelled", "contained", "dependent", "holder", "obsolete", "superseded"} {
		items = append(items, BacklogItemParams{AdmitParams: AdmitParams{ID: id, Tree: "tree", Priority: 10, Content: "accept " + id}})
	}
	// The Escalated holder keeps the dependent Pending through the restart.
	if _, err = call(t, d, "BacklogAdmit", BacklogParams{Items: items, Edges: []controlplane.BacklogEdge{{From: "holder", To: "dependent", Kind: "blocking"}}, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	control := func(id, method, reason string) {
		t.Helper()
		if _, err := call(t, d, method, TaskControlParams{ID: id, Reason: reason, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(id, method, err)
		}
	}
	control("cancelled", "Cancel", "not needed")
	for _, id := range []string{"holder", "obsolete", "superseded", "contained"} {
		control(id, "Escalate", "review")
	}
	control("obsolete", "Retire", "obsolete ruling-7")
	control("superseded", "Retire", "superseded-by done-b")
	// A contained reason needs a live landing receipt (validated elsewhere);
	// the reads only need the retired checkpoint it produces.
	d.mu.Lock()
	s := d.jobs["contained"]
	var e envelope
	if err = decodeEnvelope(s.Payload, &e); err != nil {
		t.Fatal(err)
	}
	next, events, err := engine.ApplyControl(e.Job, engine.Control{Method: "Retire", Reason: "contained " + strings.Repeat("ab", 20)}, time.Now().UTC())
	if err == nil {
		e.Job = next
		s, err = d.persist(d.ctx, s, e, events)
	}
	if err == nil {
		d.jobs["contained"] = s
		d.publishStatus()
	}
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	reads := func() map[string]string {
		t.Helper()
		out := map[string]string{}
		encode := func(v any) string {
			raw, _ := json.Marshal(v)
			return string(raw)
		}
		for name, p := range map[string]BoardParams{"board": {}, "board-retired": {IncludeRetired: true}, "board-states": {States: []string{"Done", "Escalated", "Retired"}, IncludeRetired: true}, "board-run": {RunID: "finished"}, "board-mode": {Mode: "local", IncludeRetired: true}} {
			// Pages of two cross between in-memory and plane rows.
			for _, limit := range []int{0, 2} {
				p.Limit, p.After = limit, ""
				pages := []string{}
				for {
					r, err := call(t, d, "Board", p, "")
					if err != nil {
						t.Fatal(name, err)
					}
					var page BoardPage
					if err = json.Unmarshal(r.Result, &page); err != nil {
						t.Fatal(err)
					}
					pages = append(pages, encode([]any{page.Items, page.NextCursor}))
					if p.After = page.NextCursor; p.After == "" || len(pages) > 20 {
						break
					}
				}
				out[name+"/"+encode(limit)] = strings.Join(pages, "\n")
			}
		}
		for name, p := range map[string]map[string]bool{"status": {}, "status-retired": {"include_retired": true}, "status-tasks": {"tasks": true}, "status-tasks-retired": {"tasks": true, "include_retired": true}} {
			snapshot, _ := lookup(statusBody(t, d, p), "snapshot")
			view := snapshot.(map[string]any)
			out[name] = encode([]any{view["tasks"], view["total"], view["counts"]})
		}
		for _, from := range []string{"done-a", "contained", "obsolete", "cancelled"} {
			for _, remove := range []bool{false, true} {
				r, err := call(t, d, "BacklogEdge", BacklogEdgeParams{BacklogEdge: controlplane.BacklogEdge{From: from, To: "dependent", Kind: "blocking"}, Remove: remove, AuthorizingMessage: "message"}, "")
				if err != nil {
					t.Fatal(from, remove, err)
				}
				out["edge/"+from+"/"+encode(remove)] = string(r.Result)
			}
		}
		for name, p := range map[string]ResumeRunParams{"resume-task": {Task: "done-a"}, "resume-run": {RunID: "finished"}} {
			p.AuthorizingMessage = "message"
			_, err := call(t, d, "ResumeRun", p, "")
			if err == nil || !strings.Contains(err.Error(), "terminal") {
				t.Fatalf("%s of finished work not refused as terminal: %v", name, err)
			}
			out[name] = err.Error()
		}
		return out
	}
	before := reads()
	for _, name := range []string{"board-retired/0", "status-tasks-retired"} {
		for _, id := range []string{"done-a", "cancelled", "contained", "obsolete", "superseded"} {
			if !strings.Contains(before[name], `"id":"`+id+`"`) {
				t.Fatalf("%s lacks %s: %s", name, id, before[name])
			}
		}
	}
	if strings.Contains(before["board/0"], `"Retired"`) || strings.Contains(before["status-tasks"], `"Retired"`) {
		t.Fatal("retired tasks shown without the opt-in")
	}
	if !strings.Contains(before["edge/done-a/false"], `"effect":"unchanged"`) || !strings.Contains(before["edge/obsolete/false"], `"effect":"unchanged"`) || !strings.Contains(before["edge/obsolete/false"], `obsolete: Retired`) {
		t.Fatalf("readiness effects: %s\n%s", before["edge/done-a/false"], before["edge/obsolete/false"])
	}
	stop(t, d)
	d = nil
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	_, loaded := d.jobs["done-a"]
	d.mu.Unlock()
	if loaded {
		t.Fatal("startup materialised a finished task")
	}
	// The reads page the plane until the finished-task summary is built, and
	// answer from it after; both answer as before the restart.
	hold := make(chan struct{})
	d.finished.mu.Lock()
	d.finished.hold = hold
	d.finished.mu.Unlock()
	for _, source := range []string{"plane", "summary"} {
		if source == "summary" {
			close(hold)
			for deadline := time.Now().Add(time.Minute); d.finishedSummaryBuilt() == nil; time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("finished-task summary not built")
				}
			}
		}
		after := reads()
		for name, want := range before {
			if after[name] != want {
				t.Errorf("%s from the %s changed across the restart:\nbefore %s\nafter  %s", name, source, want, after[name])
			}
		}
	}
	// A task finished since the start is on the plane and in the snapshot:
	// every read shows it once, with its current state.
	control("dependent", "Cancel", "not needed")
	seen := map[string]string{}
	for p := (BoardParams{IncludeRetired: true, Limit: 2}); ; {
		r, err := call(t, d, "Board", p, "")
		if err != nil {
			t.Fatal(err)
		}
		var page BoardPage
		if err = json.Unmarshal(r.Result, &page); err != nil {
			t.Fatal(err)
		}
		for _, item := range page.Items {
			if seen[item.ID] != "" {
				t.Fatal("board repeats", item.ID)
			}
			seen[item.ID] = item.State
		}
		if p.After = page.NextCursor; p.After == "" {
			break
		}
	}
	rows, _ := lookup(statusBody(t, d, map[string]bool{"tasks": true, "include_retired": true}), "snapshot.tasks")
	if len(seen) != 8 || seen["dependent"] != "Cancelled" || len(rows.([]any)) != 8 {
		t.Fatalf("board %v, status rows %v", seen, rows)
	}
}

// A runtime pane limit is checked against the tasks that can still run; a
// finished task, materialised or not, never refuses it.
func TestTaskPaneFloorsSkipFinishedTasks(t *testing.T) {
	doc := defaults.Workflow()
	for id, role := range doc.Roles {
		if role.Scope == "workflow-global" {
			role.Cap = 100
			doc.Roles[id] = role
		}
	}
	jobs := map[string]controlplane.JobSnapshot{}
	for _, state := range []string{"Done", "Cancelled", "Retired", "Pending"} {
		job, err := engine.NewLive(state, 1, doc, "tree")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(envelope{Job: job})
		jobs[state] = controlplane.JobSnapshot{ID: state, State: state, Payload: raw}
		err = validateTaskPaneFloors(jobs, 50)
		if (state == "Pending") != (err != nil) {
			t.Fatal(state, err)
		}
	}
}
