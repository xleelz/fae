package controlplane

import (
	"context"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
	"time"
)

func TestMessageAuthorityAgeAndOperatorIntegrity(t *testing.T) {
	for _, tc := range []struct {
		name       string
		age, limit time.Duration
		author     string
		valid      bool
	}{
		{"fresh", time.Hour, 24 * time.Hour, "operator", true},
		{"stale", 48 * time.Hour, 24 * time.Hour, "operator", false},
		{"configured", 48 * time.Hour, 72 * time.Hour, "operator", true},
		{"future", -time.Hour, 24 * time.Hour, "operator", false},
		{"nonoperator", time.Hour, 24 * time.Hour, "agent", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := fixture(t, 1)
			s.options.MessageMaxAge = tc.limit
			raw := []byte("authorize operation")
			digest := Digest(raw)
			m.ExpectQuery("SELECT m.text_digest").WithArgs("message").WillReturnRows(sqlmock.NewRows([]string{"digest", "size", "author"}).AddRow(digest, len(raw), tc.author))
			if tc.author == "operator" {
				m.ExpectQuery("SELECT size,chunks").WithArgs(digest).WillReturnRows(sqlmock.NewRows([]string{"size", "chunks"}).AddRow(len(raw), 1))
				m.ExpectQuery("SELECT chunk_index,data").WithArgs(digest).WillReturnRows(sqlmock.NewRows([]string{"index", "data"}).AddRow(0, raw))
				m.ExpectQuery("SELECT created_at").WithArgs("message").WillReturnRows(sqlmock.NewRows([]string{"created"}).AddRow(time.Now().Add(-tc.age)))
			}
			if e := s.RequireMessage(context.Background(), "message"); (e == nil) != tc.valid {
				t.Fatal(tc.name, e)
			}
		})
	}
	s, _ := fixture(t, 1)
	if s.options.MessageMaxAge != 24*time.Hour {
		t.Fatal(s.options.MessageMaxAge)
	}
}
