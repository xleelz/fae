package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// expectMigration11 expects version 11 on an empty v10 plane: both Board
// columns added, and no finished job to backfill.
func expectMigration11(m sqlmock.Sqlmock) {
	m.ExpectBegin()
	for range 2 {
		m.ExpectQuery("SELECT COUNT.*information_schema.columns").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
		m.ExpectExec("ALTER TABLE tasks ADD COLUMN").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectQuery("SELECT j.job_id,j.payload_digest,j.payload_size FROM jobs j JOIN tasks").WillReturnRows(sqlmock.NewRows([]string{"j", "d", "s"}))
	m.ExpectExec("INSERT INTO migrations").WithArgs(11).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
	expectMigration12(m)
}

// FAE-CP-009, FAE-CP-010: migration 11 projects every finished job's Board
// facts into its task row from the snapshot, once; a row the columns cannot
// reproduce stays NULL and is read from its snapshot. The step replays.
func TestMigration11BackfillsFinishedBoardColumns(t *testing.T) {
	s := testPlane(t, 10)
	ctx := context.Background()
	now := time.Now().UTC()
	reason := strings.Repeat("r", DefaultInlineLimit+1)
	seed := []struct {
		id, state, payload string
	}{
		{"done", "Done", `{"admission":{"mode":"Local","priority":20,"run_id":"run-1"},"job":{"execution_kind":"scripted","reason":"finished"}}`},
		{"retired", "Retired", `{"admission":{"mode":"Normal","priority":10,"run_id":"run-2"},"job":{"escalation_kind":"OperatorEscalate","execution_kind":"live","reason":"` + reason + `"}}`},
		{"low", "Cancelled", `{"admission":{"mode":"Local","priority":0,"run_id":"run-1"},"job":{"execution_kind":"scripted"}}`},
		{"opaque", "Done", `["not an envelope"]`},
		{"pending", "Pending", `{"admission":{"mode":"Local","priority":10,"run_id":"run-1"},"job":{"execution_kind":"scripted"}}`},
	}
	for _, r := range seed {
		ref, err := s.PutBlob(ctx, audit(), []byte(r.payload))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.ExecContext(ctx, `INSERT INTO jobs (`+jobColumns+`) VALUES (?,1,'wf',1,'target',1,?,?,?,'m')`, r.id, r.state, ref.Digest, ref.Size); err != nil {
			t.Fatal(err)
		}
		// Written as a v10 binary would: no Board columns, stale facts.
		if _, err = s.db.ExecContext(ctx, `INSERT INTO tasks (task_id,run_id,molecule_id,target_id,target_revision,state,attempt,attempt_limit,priority,disposition,created_at,updated_at) VALUES (?,'stale','m','target',1,?,1,20,10,'{}',?,?)`, r.id, r.state, now, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	columns := func() string {
		t.Helper()
		rows, err := s.db.QueryContext(ctx, `SELECT task_id,execution_kind,mode,run_id,priority,COALESCE(escalation_kind,'') FROM tasks ORDER BY task_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var id, run, escalation string
			var kind, mode sql.NullString
			var priority int
			if err = rows.Scan(&id, &kind, &mode, &run, &priority, &escalation); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal([]any{id, kind.String, kind.Valid, mode.String, run, priority, escalation})
			out = append(out, string(raw))
		}
		return strings.Join(out, "\n")
	}
	want := strings.Join([]string{
		`["done","scripted",true,"Local","run-1",20,""]`,
		`["low","",false,"","stale",10,""]`,
		`["opaque","",false,"","stale",10,""]`,
		`["pending","",false,"","stale",10,""]`,
		`["retired","live",true,"Normal","run-2",10,"OperatorEscalate"]`,
	}, "\n")
	if got := columns(); got != want {
		t.Fatalf("backfilled columns:\n%s\nwant\n%s", got, want)
	}
	if err := s.transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if err := migration11Columns(ctx, tx); err != nil {
			return err
		}
		return s.backfillBoardColumns(ctx, tx)
	}); err != nil {
		t.Fatal("migration 11 replay", err)
	}
	if got := columns(); got != want {
		t.Fatalf("replayed columns:\n%s", got)
	}
	if n := familyCount(t, s, `SELECT COUNT(*) FROM migrations WHERE version=11`); n != 1 {
		t.Fatal("migration 11 recorded", n)
	}
	// The reason spilled by reference reads back whole; the rows the columns
	// cannot reproduce carry their snapshots.
	rows, err := s.TerminalRowsAfter(ctx, "", 10, TerminalFilter{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]TerminalRow{}
	for _, r := range rows {
		got[r.ID] = r
	}
	if r := got["retired"]; !r.Columns || r.Reason != reason || r.Escalation != "OperatorEscalate" || r.ExecutionKind != "live" || len(r.Payload) != 0 {
		t.Fatalf("retired row %+v", r)
	}
	if r := got["done"]; !r.Columns || r.Reason != "finished" || r.RunID != "run-1" || r.Priority != 20 || r.Mode != "Local" || len(r.Payload) != 0 {
		t.Fatalf("done row %+v", r)
	}
	for _, id := range []string{"low", "opaque"} {
		if r := got[id]; r.Columns || len(r.Payload) == 0 {
			t.Fatalf("%s row %+v", id, r)
		}
	}
	if len(rows) != 4 {
		t.Fatalf("terminal rows %d", len(rows))
	}
}
