package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/xleelz/fae/providerhealth"
	"testing"
	"time"
)

func observation() ProviderObservation {
	return ProviderObservation{ID: "session-1", TaskID: "task", Provider: "p", Model: "m", Outcome: providerhealth.RateLimited, At: time.Unix(12345, 0).UTC()}
}
func TestProviderObservationAtomicAndIdempotent(t *testing.T) {
	o := observation()
	raw, _ := json.Marshal(o)
	digest, _, _ := ContentHash(raw)
	for _, mode := range []string{"new", "duplicate", "conflict", "audit-fails", "event-fails"} {
		t.Run(mode, func(t *testing.T) {
			s, m := fixture(t, 1)
			m.ExpectBegin()
			m.ExpectQuery("SELECT sequence.*FOR UPDATE").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(1))
			if mode == "duplicate" || mode == "conflict" {
				prior := digest
				if mode == "conflict" {
					prior = "different"
				}
				m.ExpectQuery("SELECT digest FROM provider_observations").WithArgs(o.ID).WillReturnRows(sqlmock.NewRows([]string{"digest"}).AddRow(prior))
				if mode == "duplicate" {
					m.ExpectCommit()
				} else {
					m.ExpectRollback()
				}
			} else {
				m.ExpectQuery("SELECT digest FROM provider_observations").WithArgs(o.ID).WillReturnError(sql.ErrNoRows)
				m.ExpectQuery("SELECT outcome,score").WithArgs(o.Provider, o.Model).WillReturnError(sql.ErrNoRows)
				m.ExpectExec("REPLACE INTO provider_health").WithArgs(o.Provider, o.Model, string(o.Outcome), 40.0, int64(1), nil, o.At, o.At.Add(time.Hour)).WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectExec("INSERT INTO provider_observations").WillReturnResult(sqlmock.NewResult(0, 1))
				if mode == "audit-fails" {
					m.ExpectExec("INSERT INTO audit").WillReturnError(errors.New("disk full"))
					m.ExpectRollback()
				} else {
					m.ExpectExec("INSERT INTO audit").WillReturnResult(sqlmock.NewResult(0, 1))
					m.ExpectQuery("SELECT sequence").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(1))
					if mode == "event-fails" {
						m.ExpectExec("INSERT INTO events").WillReturnError(errors.New("disk full"))
						m.ExpectRollback()
					} else {
						m.ExpectExec("INSERT INTO events").WillReturnResult(sqlmock.NewResult(0, 1))
						m.ExpectExec("UPDATE event_clock").WillReturnResult(sqlmock.NewResult(0, 1))
						m.ExpectCommit()
					}
				}
			}
			e := s.ObserveProvider(context.Background(), o)
			if (e == nil) != (mode == "new" || mode == "duplicate") {
				t.Fatal(e)
			}
		})
	}
}
func TestProviderRejectsInvalidObservation(t *testing.T) {
	s, _ := fixture(t, 1)
	o := observation()
	o.Outcome = "arbitrary"
	if s.ObserveProvider(context.Background(), o) == nil {
		t.Fatal("unknown outcome")
	}
	o = observation()
	o.ID = ""
	if s.ObserveProvider(context.Background(), o) == nil {
		t.Fatal("missing id")
	}
}

func checkProviderDolt(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	o := observation()
	if e := s.ObserveProvider(ctx, o); e != nil {
		t.Fatal(e)
	}
	if e := s.ObserveProvider(ctx, o); e != nil {
		t.Fatal(e)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM provider_observations WHERE observation_id='session-1'`, `SELECT COUNT(*) FROM audit WHERE action='provider observation'`, `SELECT COUNT(*) FROM events WHERE kind='provider-observation'`} {
		var n int
		if e := s.db.QueryRowContext(ctx, q).Scan(&n); e != nil || n != 1 {
			t.Fatal(q, n, e)
		}
	}
	health, e := s.ProviderHealth(ctx)
	if e != nil || len(health) != 1 || health[0].Score != 40 || health[0].Failures != 1 {
		t.Fatal(health, e)
	}
	o.Outcome = providerhealth.OK
	if !errors.Is(s.ObserveProvider(ctx, o), ErrConflict) {
		t.Fatal("duplicate mismatch accepted")
	}
	o.ID = "session-2"
	o.At = o.At.Add(time.Second)
	if e = s.ObserveProvider(ctx, o); e != nil {
		t.Fatal(e)
	}
	health, e = s.ProviderHealth(ctx)
	if e != nil || health[0].Score > 41 {
		t.Fatal("durable hold bypass", health, e)
	}
	// Reopen the connection to prove this is SQL state, not a process cache.
	other := New(s.db, Options{})
	health, e = other.ProviderHealth(ctx)
	if e != nil || len(health) != 1 || health[0].Failures != 1 {
		t.Fatal(health, e)
	}
}
