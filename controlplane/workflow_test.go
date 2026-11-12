package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestWorkflowWriteAssignsLatestPlusOneWithParent(t *testing.T) {
	s, m := fixture(t, 1)
	m.ExpectBegin()
	m.ExpectQuery("SELECT COUNT").WithArgs("w").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	m.ExpectQuery("SELECT COALESCE").WithArgs("w").WillReturnRows(sqlmock.NewRows([]string{"latest"}).AddRow(2))
	m.ExpectQuery("SELECT size FROM blobs").WillReturnRows(sqlmock.NewRows([]string{"size"}).AddRow(2))
	m.ExpectExec("INSERT INTO workflows").WithArgs("w", int64(3), int64(2), "display", sqlmock.AnyArg(), "operator", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	expectAudit(m)
	m.ExpectCommit()
	r, e := s.WriteWorkflow(context.Background(), audit(), "w", "display", json.RawMessage(`{}`), func([]byte) error { return nil })
	if e != nil || r.Number != 3 || r.Parent != 2 || r.Name != "display" || r.Hash == "" {
		t.Fatal(r, e)
	}
}
func TestWorkflowWriteRefusedAfterRetirement(t *testing.T) {
	s, m := fixture(t, 1)
	m.ExpectBegin()
	m.ExpectQuery("SELECT COUNT").WithArgs("w").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(1))
	m.ExpectRollback()
	if _, e := s.WriteWorkflow(context.Background(), audit(), "w", "w", json.RawMessage(`{}`), func([]byte) error { return nil }); !errors.Is(e, ErrWorkflowRetired) {
		t.Fatal(e)
	}
}
func TestWorkflowActiveAndRetirementRecord(t *testing.T) {
	s, m := fixture(t, 1)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.ExpectQuery("SELECT retired_at,retired_by,reason FROM workflow_status").WithArgs("live").WillReturnError(sql.ErrNoRows)
	m.ExpectQuery("SELECT retired_at,retired_by,reason FROM workflow_status").WithArgs("old").WillReturnRows(sqlmock.NewRows([]string{"retired_at", "retired_by", "reason"}).AddRow(at, "operator", "superseded"))
	m.ExpectQuery("SELECT retired_at,retired_by,reason FROM workflow_status").WithArgs("old").WillReturnRows(sqlmock.NewRows([]string{"retired_at", "retired_by", "reason"}).AddRow(at, "operator", "superseded"))
	if e := s.WorkflowActive(context.Background(), "live"); e != nil {
		t.Fatal(e)
	}
	if e := s.WorkflowActive(context.Background(), "old"); !errors.Is(e, ErrWorkflowRetired) {
		t.Fatal(e)
	}
	r, e := s.WorkflowRetirement(context.Background(), "old")
	if e != nil || r == nil || !r.RetiredAt.Equal(at) || r.RetiredBy != "operator" || r.Reason != "superseded" || r.WorkflowID != "old" {
		t.Fatal(r, e)
	}
}
func TestWorkflowListMarksRetirement(t *testing.T) {
	s, m := fixture(t, 1)
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.ExpectQuery("SELECT w.workflow_id").WithArgs("w", "w").WillReturnRows(sqlmock.NewRows([]string{"workflow_id", "revision", "parent", "name", "content_digest", "created_by", "created_at", "retired_at"}).
		AddRow("w", 1, 0, "w", "h1", "operator", at, at).AddRow("w", 2, 1, "w", "h2", "operator", at, at))
	rows, e := s.ListWorkflows(context.Background(), "w")
	if e != nil || len(rows) != 2 || rows[1].Revision != 2 || rows[1].Parent != 1 || !rows[0].Retired || rows[1].Hash != "h2" {
		t.Fatal(rows, e)
	}
}

func TestWorkflowFootprintFailureRollsBackRevision(t *testing.T) {
	s, m := fixture(t, 1)
	m.ExpectBegin()
	m.ExpectQuery("SELECT COUNT").WithArgs("w").WillReturnRows(sqlmock.NewRows([]string{"n"}).AddRow(0))
	m.ExpectQuery("SELECT COALESCE").WithArgs("w").WillReturnRows(sqlmock.NewRows([]string{"latest"}).AddRow(0))
	m.ExpectQuery("SELECT size FROM blobs").WillReturnRows(sqlmock.NewRows([]string{"size"}).AddRow(2))
	m.ExpectExec("INSERT INTO workflows").WillReturnResult(sqlmock.NewResult(1, 1))
	m.ExpectQuery("SELECT size FROM blobs").WillReturnError(errors.New("footprint metadata unavailable"))
	m.ExpectRollback()
	if _, err := s.WriteWorkflow(context.Background(), audit(), "w", "display", json.RawMessage(`{}`), func([]byte) error { return nil }, json.RawMessage(`{"slot_scope":"tree","derived_sessions":6,"effective_sessions":8}`)); err == nil {
		t.Fatal("workflow revision committed without its footprint")
	}
}
