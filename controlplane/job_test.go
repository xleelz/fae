package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
)

func jobFixture() JobSnapshot {
	return JobSnapshot{ID: "job", WorkflowID: "wf", WorkflowRevision: 1, TargetID: "target", TargetRevision: 1, State: "Pending", Payload: json.RawMessage(`{"stage":0}`)}
}
func jobRows(j JobSnapshot) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"id", "version", "wf", "wfr", "target", "tr", "state", "digest", "size"}).AddRow(j.ID, j.Version, j.WorkflowID, j.WorkflowRevision, j.TargetID, j.TargetRevision, j.State, Digest(j.Payload), len(j.Payload))
}
func expectClock(m sqlmock.Sqlmock) {
	m.ExpectQuery("SELECT sequence FROM event_clock WHERE id=1 FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(0))
}
func TestJobValidationAndStaleCAS(t *testing.T) {
	ctx := context.Background()
	s, m := fixture(t, 2)
	if _, e := s.AdmitJob(ctx, audit(), "", JobSnapshot{}); e == nil {
		t.Fatal("invalid job accepted")
	}
	j := jobFixture()
	old := j
	old.Version = 2
	for _, change := range []string{"stale", "workflow", "target"} {
		candidate := j
		expected := int64(2)
		if change == "stale" {
			expected = 1
		}
		if change == "workflow" {
			candidate.WorkflowRevision = 2
		}
		if change == "target" {
			candidate.TargetID = "other"
		}
		m.ExpectBegin()
		expectClock(m)
		m.ExpectQuery("SELECT job_id").WillReturnRows(jobRows(old))
		m.ExpectRollback()
		if _, e := s.SaveJob(ctx, audit(), candidate, expected); !errors.Is(e, ErrConflict) {
			t.Fatal(change, e)
		}
	}
}
func TestJobSaveRetriesAndAuditFailureRollsBack(t *testing.T) {
	for _, failAudit := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry", true: "audit rollback"}[failAudit], func(t *testing.T) {
			s, m := fixture(t, 2)
			j := jobFixture()
			old := j
			old.Version = 1
			attempts := 2
			if failAudit {
				attempts = 1
			}
			for i := 0; i < attempts; i++ {
				m.ExpectBegin()
				expectClock(m)
				m.ExpectQuery("SELECT job_id").WillReturnRows(jobRows(old))
				m.ExpectQuery("SELECT size FROM blobs").WillReturnRows(sqlmock.NewRows([]string{"size"}).AddRow(len(j.Payload)))
				m.ExpectExec("UPDATE jobs").WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectExec("UPDATE backlog_items").WillReturnResult(sqlmock.NewResult(0, 0))
				m.ExpectQuery("SELECT sequence FROM event_clock").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(0))
				m.ExpectExec("INSERT INTO events").WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectExec("UPDATE event_clock").WillReturnResult(sqlmock.NewResult(0, 1))
				if failAudit {
					m.ExpectExec("INSERT INTO audit").WillReturnError(errors.New("disk failure"))
					m.ExpectRollback()
				} else {
					expectAudit(m)
					if i == 0 {
						m.ExpectCommit().WillReturnError(&mysql.MySQLError{Number: 1213})
					} else {
						m.ExpectCommit()
					}
				}
			}
			got, e := s.SaveJob(context.Background(), audit(), j, 1)
			if failAudit {
				if e == nil {
					t.Fatal("audit failure accepted")
				}
			} else if e != nil || got.Version != 2 {
				t.Fatal(got, e)
			}
		})
	}
}
func TestJobIdempotencyCollision(t *testing.T) {
	s, m := fixture(t, 2)
	m.ExpectBegin()
	expectClock(m)
	m.ExpectQuery("SELECT request_digest").WillReturnRows(sqlmock.NewRows([]string{"request", "response", "size"}).AddRow("different", "digest", 1))
	m.ExpectRollback()
	if _, e := s.AdmitJob(context.Background(), audit(), "key", jobFixture()); !errors.Is(e, ErrIdempotencyConflict) {
		t.Fatal(e)
	}
}
func TestEventBounds(t *testing.T) {
	s, _ := fixture(t, 1)
	for _, after := range []int64{-1, 0} {
		if _, e := s.Events(context.Background(), after, 0); e == nil {
			t.Fatal("invalid bounds accepted")
		}
	}
}

func TestMigrationV2InterruptedReplay(t *testing.T) {
	s, m := fixture(t, 1)
	for attempt := 0; attempt < 2; attempt++ {
		m.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").WillReturnResult(sqlmock.NewResult(0, 0))
		m.ExpectQuery("SELECT COALESCE").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(1))
		m.ExpectBegin()
		if attempt == 0 {
			m.ExpectExec("CREATE TABLE IF NOT EXISTS jobs").WillReturnResult(sqlmock.NewResult(0, 0))
			m.ExpectExec("CREATE TABLE IF NOT EXISTS request_keys").WillReturnError(errors.New("connection lost"))
			m.ExpectRollback()
		} else {
			for _, statement := range Migration2 {
				m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			m.ExpectExec("INSERT INTO migrations").WithArgs(2).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()
			m.ExpectBegin()
			for _, statement := range Migration3 {
				m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			m.ExpectExec("INSERT INTO migrations").WithArgs(3).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()
			m.ExpectBegin()
			for _, statement := range Migration4 {
				m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			m.ExpectExec("INSERT INTO migrations").WithArgs(4).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()
			m.ExpectBegin()
			for _, statement := range Migration5 {
				m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			m.ExpectExec("INSERT INTO migrations").WithArgs(5).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()
			m.ExpectBegin()
			for _, statement := range Migration6 {
				m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			m.ExpectExec("INSERT INTO migrations").WithArgs(6).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()

		}
		e := s.Migrate(context.Background())
		if (e != nil) != (attempt == 0) {
			t.Fatal(attempt, e)
		}
	}
}

func TestAdmissionLookupMissingAndDifferentRequest(t *testing.T) {
	s, m := fixture(t, 1)
	ctx := context.Background()
	if _, found, e := s.LookupAdmission(ctx, "", json.RawMessage(`{}`)); e != nil || found {
		t.Fatal(found, e)
	}
	m.ExpectQuery("SELECT request_digest").WithArgs("missing").WillReturnRows(sqlmock.NewRows([]string{"request", "response", "size"}))
	if _, found, e := s.LookupAdmission(ctx, "missing", json.RawMessage(`{}`)); e != nil || found {
		t.Fatal(found, e)
	}
	m.ExpectQuery("SELECT request_digest").WithArgs("collision").WillReturnRows(sqlmock.NewRows([]string{"request", "response", "size"}).AddRow("different", "digest", 1))
	if _, _, e := s.LookupAdmission(ctx, "collision", json.RawMessage(`{}`)); !errors.Is(e, ErrIdempotencyConflict) {
		t.Fatal(e)
	}
}
