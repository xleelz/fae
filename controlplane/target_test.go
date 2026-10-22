package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/go-sql-driver/mysql"
	"testing"
)

func TestTargetWriteValidationBeforeSQL(t *testing.T) {
	s, m := fixture(t, 1)
	sentinel := errors.New("invalid target")
	if _, err := s.WriteTarget(context.Background(), audit(), "target", json.RawMessage(`{}`), func([]byte) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
func TestTargetWriteRollbackAndRevisionRetry(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(map[bool]string{false: "audit rollback", true: "concurrent revision retry"}[retry], func(t *testing.T) {
			s, m := fixture(t, 2)
			raw := json.RawMessage(`{"v":1}`)
			attempts := 1
			if retry {
				attempts = 2
			}
			for i := 0; i < attempts; i++ {
				m.ExpectBegin()
				m.ExpectQuery("SELECT COUNT.*daemon_markers").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
				m.ExpectQuery("SELECT COALESCE.*target_configurations").WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow(4 + i))
				m.ExpectQuery("SELECT size FROM blobs").WillReturnRows(sqlmock.NewRows([]string{"size"}).AddRow(len(raw)))
				insert := m.ExpectExec("INSERT INTO target_configurations").WithArgs("target", 5+i, Digest(raw), sqlmock.AnyArg())
				if retry && i == 0 {
					insert.WillReturnError(&mysql.MySQLError{Number: 1213, Message: "concurrent commit"})
					m.ExpectRollback()
					continue
				}
				insert.WillReturnResult(sqlmock.NewResult(1, 1))
				audit := m.ExpectExec("INSERT INTO audit")
				if !retry {
					audit.WillReturnError(errors.New("audit unavailable"))
					m.ExpectRollback()
				} else {
					audit.WillReturnResult(sqlmock.NewResult(1, 1))
					m.ExpectCommit()
				}
			}
			r, err := s.WriteTarget(context.Background(), audit(), "target", raw, func([]byte) error { return nil })
			if retry {
				if err != nil || r.Revision != 6 {
					t.Fatal(r, err)
				}
			} else if err == nil {
				t.Fatal("unaudited revision committed")
			}
			if err = m.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestTargetRetiredWriteRefused(t *testing.T) {
	s, m := fixture(t, 1)
	m.ExpectBegin()
	m.ExpectQuery("SELECT COUNT.*daemon_markers").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	m.ExpectRollback()
	if _, err := s.WriteTarget(context.Background(), audit(), "retired", json.RawMessage(`{}`), func([]byte) error { return nil }); err == nil {
		t.Fatal("retired target revised")
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTargetRetirementPreservesFirstTombstone(t *testing.T) {
	s, m := fixture(t, 1)
	m.ExpectBegin()
	m.ExpectQuery("SELECT COUNT.*target_configurations").WithArgs("target").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	m.ExpectQuery("SELECT COUNT.*daemon_markers").WithArgs(targetRetiredKey("target")).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	m.ExpectExec("INSERT INTO audit").WillReturnResult(sqlmock.NewResult(1, 1))
	m.ExpectCommit()
	if err := s.RetireTarget(context.Background(), audit(), "target", "different reason"); err != nil {
		t.Fatal(err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
