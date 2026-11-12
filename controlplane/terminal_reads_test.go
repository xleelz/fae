package controlplane

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// FAE-CP-009: terminal pages read only terminal job rows after the cursor, in
// id order, narrowed in the statement by state, the job row's columns and the
// task row's run, mode and priority. A row with Board columns carries no
// snapshot; a row without them (no task row, or columns predating migration
// 11) carries its snapshot and passes the task-column filters.
func TestTerminalRowsAfterPagesByIndex(t *testing.T) {
	s := testPlane(t, SchemaVersion)
	ctx := context.Background()
	payload := []byte(`{"job":{"reason":"r"}}`)
	ref, err := s.PutBlob(ctx, Audit{Actor: "test", Action: "seed", Outcome: "ok"}, payload)
	if err != nil {
		t.Fatal(err)
	}
	states := []string{"Done", "Pending", "Retired", "Cancelled", "Escalated"}
	rows, args := []string{}, []any{}
	for i := 0; i < 25; i++ {
		rows = append(rows, "(?,1,?,1,?,1,?,?,?,?)")
		args = append(args, fmt.Sprintf("job-%02d", i), []string{"wf-a", "wf-b"}[i%2], []string{"t-a", "t-b", "t-c"}[i%3], states[i%5], ref.Digest, ref.Size, []string{"m-a", "m-b"}[i/13])
	}
	if _, err = s.db.ExecContext(ctx, `INSERT INTO jobs (`+jobColumns+`) VALUES `+strings.Join(rows, ","), args...); err != nil {
		t.Fatal(err)
	}
	// Task rows with Board columns for jobs 10..19: run-a or run-b, priority
	// 10 or 50, mode Local or Normal. Jobs 20..24 have rows without them.
	now := time.Now().UTC()
	for i := 10; i < 25; i++ {
		var kind, mode any
		if i < 20 {
			kind, mode = "scripted", []string{"Local", "Normal"}[i%2]
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO tasks (task_id,run_id,molecule_id,target_id,target_revision,state,attempt,attempt_limit,priority,disposition,escalation_reason,created_at,updated_at,execution_kind,mode) VALUES (?,?,'m','t',1,?,1,20,?,'{}',?,?,?,?,?)`,
			fmt.Sprintf("job-%02d", i), []string{"run-a", "run-b"}[i/15], states[i%5], []int{10, 50}[i%3/2], fmt.Sprintf("reason-%d", i), now, now, kind, mode); err != nil {
			t.Fatal(err)
		}
	}
	ids := func(f TerminalFilter, after string, limit int) string {
		t.Helper()
		got, err := s.TerminalRowsAfter(ctx, after, limit, f)
		if err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, r := range got {
			n := 0
			fmt.Sscanf(r.ID, "job-%d", &n)
			if columns := n >= 10 && n < 20; r.Columns != columns || columns != (len(r.Payload) == 0) || !columns && string(r.Payload) != string(payload) {
				t.Fatalf("row %s: columns %t, payload %q", r.ID, r.Columns, r.Payload)
			}
			if r.Columns && (r.Reason != fmt.Sprintf("reason-%d", n) || r.ExecutionKind != "scripted") {
				t.Fatalf("row %+v", r)
			}
			out = append(out, r.ID+":"+r.State)
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		f     TerminalFilter
		after string
		limit int
		want  string
	}{
		{TerminalFilter{}, "", 4, "job-00:Done,job-02:Retired,job-03:Cancelled,job-05:Done"},
		{TerminalFilter{}, "job-05", 3, "job-07:Retired,job-08:Cancelled,job-10:Done"},
		{TerminalFilter{}, "job-22", 10, "job-23:Cancelled"},
		{TerminalFilter{States: []string{"Done"}}, "", 10, "job-00:Done,job-05:Done,job-10:Done,job-15:Done,job-20:Done"},
		{TerminalFilter{States: []string{"Done", "Done"}}, "job-05", 2, "job-10:Done,job-15:Done"},
		{TerminalFilter{States: []string{"Cancelled", "Retired"}, Workflow: "wf-b", Target: "t-a"}, "", 10, "job-03:Cancelled"},
		{TerminalFilter{Molecule: "m-b"}, "", 10, "job-13:Cancelled,job-15:Done,job-17:Retired,job-18:Cancelled,job-20:Done,job-22:Retired,job-23:Cancelled"},
		{TerminalFilter{Target: "t-z"}, "", 10, ""},
		// Task-column filters narrow the rows with Board columns; the others
		// always pass.
		{TerminalFilter{RunID: "run-b"}, "job-09", 20, "job-15:Done,job-17:Retired,job-18:Cancelled,job-20:Done,job-22:Retired,job-23:Cancelled"},
		{TerminalFilter{RunID: "run-z"}, "job-09", 20, "job-20:Done,job-22:Retired,job-23:Cancelled"},
		{TerminalFilter{Mode: "normal"}, "job-09", 20, "job-13:Cancelled,job-15:Done,job-17:Retired,job-20:Done,job-22:Retired,job-23:Cancelled"},
		{TerminalFilter{MinPriority: 50}, "job-09", 20, "job-17:Retired,job-20:Done,job-22:Retired,job-23:Cancelled"},
		{TerminalFilter{RunID: "run-a", Mode: "LOCAL", MinPriority: 10, States: []string{"Retired", "Cancelled"}}, "", 20, "job-02:Retired,job-03:Cancelled,job-07:Retired,job-08:Cancelled,job-12:Retired,job-22:Retired,job-23:Cancelled"},
	} {
		if got := ids(c.f, c.after, c.limit); got != c.want {
			t.Errorf("%+v after %q: got %s want %s", c.f, c.after, got, c.want)
		}
	}
	for _, bad := range []struct {
		f     TerminalFilter
		limit int
	}{{TerminalFilter{States: []string{"Pending"}}, 10}, {TerminalFilter{}, 0}, {TerminalFilter{}, 1001}, {TerminalFilter{MinPriority: -1}, 10}} {
		if _, err = s.TerminalRowsAfter(ctx, "", bad.limit, bad.f); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}
