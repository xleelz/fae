package controlplane

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// FAE-API-020: a supervised child's lifecycle is one audited event; unknown
// phases are refused before any write.
func TestRecordChildLifecycle(t *testing.T) {
	s, m := fixture(t, 1)
	if err := s.RecordChildLifecycle(context.Background(), "exploded", json.RawMessage(`{"child":"ui"}`)); err == nil {
		t.Fatal("unknown phase accepted")
	}
	m.ExpectBegin()
	expectClock(m)
	m.ExpectQuery("SELECT sequence FROM event_clock").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(4))
	m.ExpectExec("INSERT INTO events").WithArgs(int64(5), "child-crashed", "", `{"child":"ui","pid":7}`, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
	m.ExpectExec("UPDATE event_clock").WillReturnResult(sqlmock.NewResult(0, 1))
	expectAudit(m)
	m.ExpectCommit()
	if err := s.RecordChildLifecycle(context.Background(), "crashed", json.RawMessage(`{"pid":7,"child":"ui"}`)); err != nil {
		t.Fatal(err)
	}
}
