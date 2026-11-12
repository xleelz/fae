package controlplane

import (
	"context"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"regexp"
	"testing"
	"time"
)

func TestEventMigrationV6InterruptedReplay(t *testing.T) {
	s, m := fixture(t, 1)
	for attempt := 0; attempt < 2; attempt++ {
		m.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").WillReturnResult(sqlmock.NewResult(0, 0))
		m.ExpectQuery("SELECT COALESCE").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(5))
		m.ExpectBegin()
		for _, q := range Migration6 {
			m.ExpectExec(regexp.QuoteMeta(q)).WillReturnResult(sqlmock.NewResult(0, 0))
		}
		if attempt == 0 {
			m.ExpectExec("INSERT INTO migrations").WithArgs(6).WillReturnError(errors.New("lost DDL acknowledgement"))
			m.ExpectRollback()
		} else {
			m.ExpectExec("INSERT INTO migrations").WithArgs(6).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()
			m.ExpectBegin()
			for _, q := range Migration7 {
				m.ExpectExec(regexp.QuoteMeta(q)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			m.ExpectExec("INSERT INTO migrations").WithArgs(7).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()
			expectMigration8(m)
			expectMigration9(m)
			expectMigration10(m)
			expectMigration11(m)
		}
		if err := s.Migrate(context.Background()); (err != nil) != (attempt == 0) {
			t.Fatal(err)
		}
	}
}
func TestEventWindowRejectsInvalid(t *testing.T) {
	s, _ := fixture(t, 1)
	for _, v := range []struct {
		age   time.Duration
		count int
	}{{0, 1}, {-1, 1}, {time.Hour, 0}} {
		if _, e := s.EventWindow(context.Background(), v.age, v.count, time.Now()); e == nil {
			t.Fatal(v)
		}
	}
}

func TestEventWindowAuditedAdvanceAndLostAck(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "lost-ack"}[lost], func(t *testing.T) {
			s, m := fixture(t, 1)
			now := time.Now()
			m.ExpectQuery("SELECT floor_sequence").WillReturnRows(sqlmock.NewRows([]string{"floor"}).AddRow(0))
			m.ExpectQuery("SELECT MIN").WithArgs(now.Add(-time.Hour)).WillReturnRows(sqlmock.NewRows([]string{"oldest"}).AddRow(nil))
			m.ExpectQuery("SELECT sequence FROM events").WithArgs(2).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(8))
			m.ExpectBegin()
			m.ExpectQuery("SELECT floor_sequence.*FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"floor"}).AddRow(0))
			m.ExpectExec("UPDATE event_retention").WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectExec("INSERT INTO audit").WithArgs("daemon", "event retention floor", "", sqlmock.AnyArg(), sqlmock.AnyArg(), "advanced", sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
			if lost {
				m.ExpectCommit().WillReturnError(errors.New("lost commit acknowledgement"))
				m.ExpectQuery("SELECT floor_sequence").WillReturnRows(sqlmock.NewRows([]string{"floor"}).AddRow(7))
			} else {
				m.ExpectCommit()
			}
			floor, err := s.EventWindow(context.Background(), time.Hour, 3, now)
			if err != nil || floor != 7 {
				t.Fatalf("%d %v", floor, err)
			}
			// Unchanged floor performs no transaction, audit, or event write.
			m.ExpectQuery("SELECT floor_sequence").WillReturnRows(sqlmock.NewRows([]string{"floor"}).AddRow(7))
			m.ExpectQuery("SELECT MIN").WillReturnRows(sqlmock.NewRows([]string{"oldest"}).AddRow(nil))
			m.ExpectQuery("SELECT sequence FROM events").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(8))
			floor, err = s.EventWindow(context.Background(), time.Hour, 3, now)
			if err != nil || floor != 7 {
				t.Fatal(floor, err)
			}
		})
	}
}
func TestEventWindowConcurrentAdvancementAdopted(t *testing.T) {
	s, m := fixture(t, 1)
	m.ExpectQuery("SELECT floor_sequence").WillReturnRows(sqlmock.NewRows([]string{"floor"}).AddRow(0))
	m.ExpectQuery("SELECT MIN").WillReturnRows(sqlmock.NewRows([]string{"oldest"}).AddRow(nil))
	m.ExpectQuery("SELECT sequence FROM events").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(8))
	m.ExpectBegin()
	m.ExpectQuery("SELECT floor_sequence.*FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"floor"}).AddRow(9))
	m.ExpectRollback()
	floor, err := s.EventWindow(context.Background(), time.Hour, 3, time.Now())
	if err != nil || floor != 9 {
		t.Fatal(floor, err)
	}
}
