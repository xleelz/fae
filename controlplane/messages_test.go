package controlplane

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Replaying v7 after an interrupted acknowledgement recreates nothing and
// records the version once.
func TestMessageMigrationV7InterruptedReplay(t *testing.T) {
	s, m := fixture(t, 1)
	for attempt := 0; attempt < 2; attempt++ {
		m.ExpectExec("CREATE TABLE IF NOT EXISTS migrations").WillReturnResult(sqlmock.NewResult(0, 0))
		m.ExpectQuery("SELECT COALESCE").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(6))
		m.ExpectBegin()
		for _, q := range Migration7 {
			m.ExpectExec(regexp.QuoteMeta(q)).WillReturnResult(sqlmock.NewResult(0, 0))
		}
		if attempt == 0 {
			m.ExpectExec("INSERT INTO migrations").WithArgs(7).WillReturnError(errors.New("lost DDL acknowledgement"))
			m.ExpectRollback()
		} else {
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

func TestPostMessageRefusesBeforeWriting(t *testing.T) {
	s, _ := fixture(t, 1)
	for name, in := range map[string]MessageInput{
		"empty id":            {Text: "x"},
		"empty text":          {ID: "m"},
		"operator relay":      {ID: "m", Text: "x", Source: "chat"},
		"unmarked controller": {ID: "m", Text: "x", RecordedBy: "controller"},
		"unknown recorder":    {ID: "m", Text: "x", RecordedBy: "prophet"},
		"prophet relay":       {ID: "m", Text: "x", RecordedBy: "controller", Source: "chat", Channel: "prophet"},
		"unknown channel":     {ID: "m", Text: "x", Channel: "mail"},
	} {
		if _, e := s.PostMessage(context.Background(), in); e == nil || strings.Contains(e.Error(), "expectations") {
			t.Fatal(name, e)
		}
	}
}
