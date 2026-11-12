package controlplane

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func expectMigration12(m sqlmock.Sqlmock) {
	m.ExpectBegin()
	for _, ddl := range Migration12 {
		m.ExpectExec(regexp.QuoteMeta(ddl)).WillReturnResult(sqlmock.NewResult(0, 0))
	}
	pairs := map[string]map[string]bool{}
	for _, group := range [][]string{SchemaStatements, Migration2, Migration3, Migration4, Migration5, Migration6, Migration7, Migration8, Migration9, Migration10, Migration11, {TurnCheckpointsTableDDL}} {
		for _, ddl := range group {
			table := createTableName.FindStringSubmatch(ddl)
			if len(table) == 0 {
				continue
			}
			for _, c := range treeIdentityColumn.FindAllStringSubmatch(ddl, -1) {
				if pairs[table[1]] == nil {
					pairs[table[1]] = map[string]bool{}
				}
				pairs[table[1]][c[1]] = true
			}
		}
	}
	for _, p := range [][2]string{{"external_effects", "session_id"}, {"engine_runs", "dispatch_id"}} {
		pairs[p[0]][p[1]] = true
	}
	tables := []string{}
	for table := range pairs {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	for _, table := range tables {
		cols := []string{}
		for col := range pairs[table] {
			cols = append(cols, col)
		}
		sort.Strings(cols)
		for _, col := range cols {
			size := 2048
			if col == "session_id" || col == "dispatch_id" {
				size = 3072
			}
			m.ExpectQuery("SELECT DATA_TYPE,CHARACTER_MAXIMUM_LENGTH,IS_NULLABLE,COLUMN_DEFAULT").WithArgs(table, col).WillReturnRows(sqlmock.NewRows([]string{"type", "length", "nullable", "default"}).AddRow("varbinary", size, "NO", nil))
		}
	}
	m.ExpectQuery("SELECT job_id,payload_digest,payload_size FROM jobs WHERE job_id").WillReturnRows(sqlmock.NewRows([]string{"id", "digest", "size"}))
	m.ExpectExec("INSERT INTO migrations").WithArgs(12).WillReturnResult(sqlmock.NewResult(0, 1))
	m.ExpectCommit()
}
func TestMigration12PreservesLongUTF8TaskIDsAndExactEdges(t *testing.T) {
	s := testPlane(t, 11)
	ctx := context.Background()
	doc := defaults.Workflow()
	installWorkflow(t, s, doc)
	_, err := s.db.ExecContext(ctx, `INSERT INTO jobs (job_id,version,workflow_id,workflow_revision,target_id,target_revision,state,payload_digest,payload_size,molecule_id) VALUES ('old-根',1,'default',1,'target',1,'Done',?,2,'')`, Digest([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PutBlob(ctx, audit(), []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Migrate(ctx); err != nil {
		t.Fatal("migration replay", err)
	}
	ids := []string{strings.Repeat("child.", 220) + "根", strings.Repeat("child.", 220) + "葉"}
	for _, id := range ids {
		j, err := engine.New(id, 1, doc, "tree")
		if err != nil {
			t.Fatal(err)
		}
		saved, err := s.AdmitJob(ctx, audit(), "", envelopeSnapshot(t, j))
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.GetJob(ctx, id)
		if err != nil || got.ID != id || saved.ID != id {
			t.Fatal("long id altered", err)
		}
	}
	edge := BacklogEdge{From: ids[0], To: ids[1], Kind: "blocking"}
	if err = s.EditBacklogEdge(ctx, audit(), edge, false); err != nil {
		t.Fatal(err)
	}
	edges, err := s.DependentEdges(ctx, ids[1])
	if err != nil || len(edges) != 1 || edges[0] != edge {
		t.Fatal("long edge lost", edges, err)
	}
	got, err := s.GetJob(ctx, "old-根")
	if err != nil || got.ID != "old-根" {
		t.Fatal("existing bytes changed", err)
	}
}
