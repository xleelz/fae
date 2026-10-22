package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"regexp"
	"strings"
	"testing"
)

func emission() TicketEmission {
	return TicketEmission{Key: strings.Repeat("a", 64), Task: "task", Room: "room", Step: "Pending"}
}
func TestTicketOutboxLostAcknowledgement(t *testing.T) {
	for _, accepted := range []bool{true, false} {
		t.Run(map[bool]string{true: "exact", false: "different"}[accepted], func(t *testing.T) {
			s, m := fixture(t, 1)
			before := emission()
			after := before
			after.Started = true
			after.Intent = json.RawMessage(`{"key":"immutable"}`)
			actual := after
			if !accepted {
				actual = before
			}
			raw, _ := json.Marshal(actual)
			m.ExpectBegin()
			m.ExpectExec("UPDATE ticket_emissions").WillReturnError(errors.New("lost commit acknowledgement"))
			m.ExpectRollback()
			m.ExpectQuery("SELECT payload FROM ticket_emissions").WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow(string(raw)))
			err := s.SaveTicket(context.Background(), before, after)
			if (err == nil) != accepted {
				t.Fatal(err)
			}
		})
	}
}
func TestTicketOutboxRejectsRegressions(t *testing.T) {
	s, _ := fixture(t, 1)
	r := emission()
	r.Started = true
	r.Emitted = true
	r.Attempts = 2
	r.Intent = json.RawMessage(`{"id":1}`)
	for _, change := range []func(*TicketEmission){func(r *TicketEmission) { r.Started = false }, func(r *TicketEmission) { r.Emitted = false }, func(r *TicketEmission) { r.Attempts-- }, func(r *TicketEmission) { r.Intent = json.RawMessage(`{"id":2}`) }, func(r *TicketEmission) { r.Position++ }} {
		next := r
		change(&next)
		if s.SaveTicket(context.Background(), r, next) == nil {
			t.Fatal("regression accepted")
		}
	}
}
func TestTicketOutboxEnqueueIdempotence(t *testing.T) {
	s, m := fixture(t, 1)
	r := emission()
	existing := r
	existing.Started = true
	existing.Attempts = 4
	raw, _ := json.Marshal(existing)
	m.ExpectQuery("SELECT payload").WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow(string(raw)))
	if err := s.EnqueueTicket(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}
func TestTicketOutboxStats(t *testing.T) {
	s, m := fixture(t, 1)
	a := emission()
	b := a
	b.Started = true
	b.LastError = "uncertain"
	c := a
	c.Emitted = true
	c.LastError = "old"
	rows := sqlmock.NewRows([]string{"payload"})
	for _, r := range []TicketEmission{a, b, c} {
		raw, _ := json.Marshal(r)
		rows.AddRow(string(raw))
	}
	m.ExpectQuery("SELECT payload").WillReturnRows(rows)
	stats, err := s.TicketStats(context.Background())
	if err != nil || stats.Depth != 2 || stats.Failures != 1 {
		t.Fatal(stats, err)
	}
}

func TestTicketCheckpointAuditAtomicity(t *testing.T) {
	for _, failure := range []string{"audit", "lost-commit"} {
		t.Run(failure, func(t *testing.T) {
			s, m := fixture(t, 1)
			before := emission()
			after := before
			after.Attempts = 1
			b, _ := json.Marshal(after)
			a, _ := json.Marshal(before)
			m.ExpectBegin()
			m.ExpectExec("UPDATE ticket_emissions").WillReturnResult(sqlmock.NewResult(0, 1))
			expectClock(m)
			m.ExpectQuery("SELECT sequence FROM event_clock").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(0))
			m.ExpectExec("INSERT INTO events").WillReturnResult(sqlmock.NewResult(1, 1))
			m.ExpectExec("UPDATE event_clock").WillReturnResult(sqlmock.NewResult(0, 1))
			if failure == "audit" {
				m.ExpectExec("INSERT INTO audit").WillReturnError(errors.New("audit failed"))
				m.ExpectRollback()
				m.ExpectQuery("SELECT payload").WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow(string(a)))
			} else {
				expectAudit(m)
				m.ExpectCommit().WillReturnError(errors.New("ack lost"))
				m.ExpectQuery("SELECT payload").WillReturnRows(sqlmock.NewRows([]string{"payload"}).AddRow(string(b)))
			}
			err := s.SaveTicket(context.Background(), before, after)
			if (err == nil) != (failure == "lost-commit") {
				t.Fatal(err)
			}
		})
	}
}

// Dolt DDL may commit before the migration version row. Replaying v5 after
// interruption must recreate no data and only then record the new version.
func TestTicketMigrationV5InterruptedReplay(t *testing.T) {
	s, m := fixture(t, 1)
	for attempt := 0; attempt < 2; attempt++ {
		m.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").WillReturnResult(sqlmock.NewResult(0, 0))
		m.ExpectQuery("SELECT COALESCE").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(4))
		m.ExpectBegin()
		for _, statement := range Migration5 {
			m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
		}
		if attempt == 0 {
			m.ExpectExec("INSERT INTO migrations").WithArgs(5).WillReturnError(errors.New("interrupted after DDL"))
			m.ExpectRollback()
		} else {
			m.ExpectExec("INSERT INTO migrations").WithArgs(5).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()
			m.ExpectBegin()
			for _, statement := range Migration6 {
				m.ExpectExec(regexp.QuoteMeta(statement)).WillReturnResult(sqlmock.NewResult(0, 0))
			}
			m.ExpectExec("INSERT INTO migrations").WithArgs(6).WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectCommit()

		}
		if err := s.Migrate(context.Background()); (err != nil) != (attempt == 0) {
			t.Fatal(attempt, err)
		}
	}
}
