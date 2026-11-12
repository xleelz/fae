package controlplane

import (
	"regexp"
	"strings"

	"github.com/DATA-DOG/go-sqlmock"
)

var testDigest = strings.Repeat("b", 64)
var testMolecule, _ = MoleculeID("wf", 1, testDigest)

// expectMigration8 expects version 8 on an empty v7 plane: its statements,
// the three absent columns and an empty backfill.
func expectMigration8(m sqlmock.Sqlmock) {
	m.ExpectBegin()
	for _, q := range Migration8 {
		m.ExpectExec(regexp.QuoteMeta(q)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	for i := 0; i < 3; i++ {
		m.ExpectQuery("SELECT COUNT.*information_schema.columns").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
		m.ExpectExec("ALTER TABLE").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectQuery("SELECT molecule_id,workflow_id,revision,content_digest FROM molecules").WillReturnRows(sqlmock.NewRows([]string{"m", "w", "r", "d"}))
	m.ExpectQuery("SELECT job_id FROM jobs").WillReturnRows(sqlmock.NewRows([]string{"id"}))
	m.ExpectExec("INSERT INTO migrations").WithArgs(8).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
}

// expectMigration9 expects version 9 on an empty v8 plane: its statements,
// the six absent columns, the nullable session columns, the marker index and
// an empty backfill.
func expectMigration9(m sqlmock.Sqlmock) {
	m.ExpectBegin()
	for _, q := range Migration9 {
		m.ExpectExec(regexp.QuoteMeta(q)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	for i := 0; i < 6; i++ {
		m.ExpectQuery("SELECT COUNT.*information_schema.columns").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
		m.ExpectExec("ALTER TABLE").WillReturnResult(sqlmock.NewResult(0, 0))
	}
	m.ExpectQuery("SELECT COUNT.*is_nullable").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(3))
	m.ExpectExec("ALTER TABLE sessions MODIFY").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("CREATE INDEX IF NOT EXISTS schedules_marker").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectQuery("SELECT pool_key,kind,payload FROM resource_pools").WillReturnRows(sqlmock.NewRows([]string{"k", "c", "p"}))
	m.ExpectQuery("SELECT payload FROM ticket_emissions").WillReturnRows(sqlmock.NewRows([]string{"p"}))
	for i := 0; i < 3; i++ {
		m.ExpectQuery("SELECT name,payload_digest,payload_size FROM daemon_markers").WillReturnRows(sqlmock.NewRows([]string{"n", "d", "s"}))
	}
	m.ExpectQuery("SELECT session_id,task_id,role").WillReturnRows(sqlmock.NewRows([]string{"s", "t", "r", "p", "m", "e", "g", "x", "a"}))
	m.ExpectQuery("SELECT task_id FROM tasks WHERE review_ref").WillReturnRows(sqlmock.NewRows([]string{"t"}))
	m.ExpectExec("INSERT INTO migrations").WithArgs(9).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
}

// expectMigration10 expects version 10 on an empty v9 plane: no pool to
// spill, then the payload column bounded to TEXT.
func expectMigration10(m sqlmock.Sqlmock) {
	m.ExpectBegin()
	m.ExpectQuery("SELECT pool_key,payload FROM resource_pools").WillReturnRows(sqlmock.NewRows([]string{"k", "p"}))
	m.ExpectQuery("SELECT COUNT.*information_schema.columns").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	m.ExpectExec("ALTER TABLE resource_pools MODIFY COLUMN payload TEXT").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectExec("INSERT INTO migrations").WithArgs(10).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
}

// expectBind expects a new binding to workflow wf revision 1.
func expectBind(m sqlmock.Sqlmock, existing bool) {
	m.ExpectQuery("SELECT COUNT.*FROM workflow_status").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	m.ExpectQuery("SELECT content_digest FROM workflows").WillReturnRows(sqlmock.NewRows([]string{"d"}).AddRow(testDigest))
	rows := sqlmock.NewRows([]string{"m", "d", "e"})
	if existing {
		rows.AddRow(testMolecule, testDigest, nil)
	}
	m.ExpectQuery("SELECT molecule_id,content_digest,ended_at FROM molecules").WillReturnRows(rows)
	if !existing {
		resolved, _ := moleculeResolution("wf", 1, testDigest)
		m.ExpectQuery("SELECT size FROM blobs").WillReturnRows(sqlmock.NewRows([]string{"size"}).AddRow(len(resolved)))
		m.ExpectExec("INSERT INTO molecules").WillReturnResult(sqlmock.NewResult(0, 1))
	}
}

// expectProjection expects the first projection of a task with no records.
func expectProjection(m sqlmock.Sqlmock) {
	m.ExpectQuery("SELECT state,molecule_id,history_cursor,provenance_cursor FROM tasks").WillReturnRows(sqlmock.NewRows([]string{"s", "m", "h", "p"}))
	m.ExpectExec("INSERT INTO tasks").WillReturnResult(sqlmock.NewResult(0, 1))
 m.ExpectExec("REPLACE INTO task_tree").WillReturnResult(sqlmock.NewResult(0,1))
	m.ExpectExec("UPDATE backlog_items SET deferred_until").WillReturnResult(sqlmock.NewResult(0, 0))
}
