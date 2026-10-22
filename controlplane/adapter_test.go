package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
)

func TestAdapterInstallAuditRollback(t *testing.T) {
	s, m := fixture(t, 1)
	raw := json.RawMessage(`{"schema_version":1}`)
	m.ExpectBegin()
	m.ExpectQuery("SELECT sequence FROM event_clock").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(0))
	m.ExpectQuery("SELECT COUNT\\(\\*\\) FROM daemon_markers").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	m.ExpectQuery("SELECT payload_digest,payload_size FROM daemon_markers").WillReturnError(sql.ErrNoRows)
	stored, _ := json.Marshal(adapterStored{AdapterRevision: AdapterRevision{ID: "one", Revision: 1, Hash: Digest(raw)}, Document: raw})
	m.ExpectQuery("SELECT size FROM blobs").WillReturnRows(sqlmock.NewRows([]string{"size"}).AddRow(len(stored)))
	m.ExpectExec("INSERT INTO daemon_markers").WillReturnResult(sqlmock.NewResult(1, 1))
	m.ExpectQuery("SELECT size FROM blobs").WillReturnRows(sqlmock.NewRows([]string{"size"}).AddRow(1))
	m.ExpectExec("REPLACE INTO daemon_markers").WillReturnResult(sqlmock.NewResult(1, 1))
	m.ExpectExec("INSERT INTO audit").WillReturnError(errors.New("audit unavailable"))
	m.ExpectRollback()
	if _, err := s.WriteAdapter(context.Background(), audit(), "one", raw); err == nil {
		t.Fatal("failed audit committed")
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestAdapterInvalidIdentityBeforeSQL(t *testing.T) {
	s, m := fixture(t, 1)
	for _, id := range []string{"", " leading", "bad\nidentity"} {
		if _, err := s.WriteAdapter(context.Background(), audit(), id, json.RawMessage(`{}`)); err == nil {
			t.Fatal(id)
		}
	}
	if _, _, err := s.GetAdapter(context.Background(), "id", -1); err == nil {
		t.Fatal("negative revision")
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
