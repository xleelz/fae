package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
)

func TestAdapterRetirementGuard(t *testing.T) {
	if !AdapterAcceptsBinding(false) || AdapterAcceptsBinding(true) {
		t.Fatal("retirement guard")
	}
}
func TestAdapterRetirementValidation(t *testing.T) {
	s, m := fixture(t, 1)
	for _, p := range [][2]string{{"", "reason"}, {"x", ""}, {"x", " \n"}} {
		if s.RetireAdapter(context.Background(), audit(), p[0], p[1]) == nil {
			t.Fatal(p)
		}
	}
	if e := m.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestAdapterRetirementMissingAndRepeated(t *testing.T) {
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "repeat"}[exists], func(t *testing.T) {
			s, m := fixture(t, 1)
			m.ExpectBegin()
			m.ExpectQuery("SELECT sequence FROM event_clock").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(0))
			n := 0
			if exists {
				n = 1
			}
			m.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(n))
			if exists {
				m.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
				m.ExpectExec("INSERT INTO audit").WillReturnResult(sqlmock.NewResult(1, 1))
				m.ExpectCommit()
			} else {
				m.ExpectRollback()
			}
			e := s.RetireAdapter(context.Background(), audit(), "id", "new reason")
			if exists && e != nil || !exists && !errors.Is(e, sql.ErrNoRows) {
				t.Fatal(e)
			}
			if e = m.ExpectationsWereMet(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
func TestAdapterRetiredInstallRefused(t *testing.T) {
	s, m := fixture(t, 1)
	m.ExpectBegin()
	m.ExpectQuery("SELECT sequence FROM event_clock").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(0))
	m.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	m.ExpectRollback()
	_, e := s.WriteAdapter(context.Background(), audit(), "id", []byte(`{}`))
	if !errors.Is(e, ErrAdapterRetired) {
		t.Fatal(e)
	}
	if e = m.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}

func TestNewJobAdapterRetirementAtTransactionBoundary(t *testing.T) {
	s, m := fixture(t, 1)
	ctx := context.Background()
	m.ExpectBegin()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	raw := []byte(`{"normal":{"adapter":{"id":"one"}}}`)
	m.ExpectQuery("SELECT content_digest FROM target_configurations").WithArgs("target", int64(1)).WillReturnRows(sqlmock.NewRows([]string{"content_digest"}).AddRow(Digest(raw)))
	m.ExpectQuery("SELECT chunk_index,data FROM blob_chunks").WithArgs(Digest(raw)).WillReturnRows(sqlmock.NewRows([]string{"chunk_index", "data"}).AddRow(0, raw))
	m.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	e = checkJobAdapterTx(ctx, tx, JobSnapshot{TargetID: "target", TargetRevision: 1, Payload: []byte(`{"admission":{"target_config":"target"}}`)})
	if !errors.Is(e, ErrAdapterRetired) {
		t.Fatal(e)
	}
	m.ExpectRollback()
	tx.Rollback()
	if e = m.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
