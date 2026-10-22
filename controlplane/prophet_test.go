package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
)

func TestProphetStorageValidationAndScopes(t *testing.T) {
	s := &Store{}
	for _, e := range []ProphetEntry{{}, {ID: "id", Author: "model-admin"}, {ID: "id", Author: "operator", Task: string(make([]byte, 256))}} {
		if _, err := s.AppendProphet(context.Background(), Audit{}, e); err == nil {
			t.Fatal(e)
		}
	}
	for _, p := range [][2]int64{{-1, 1}, {0, 0}, {0, 101}} {
		if _, e := s.ProphetPage(context.Background(), "", p[0], int(p[1])); e == nil {
			t.Fatal(p)
		}
	}
	seen := map[string]bool{}
	for _, task := range []string{"", "global", "alpha", "alpha/../beta", "beta"} {
		k := prophetPrefix(task)
		if seen[k] {
			t.Fatal("scope collision", task)
		}
		seen[k] = true
	}
}

func TestProphetAppendAtomicPageAndCounter(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			s, m := fixture(t, 1)
			m.ExpectBegin()
			m.ExpectQuery("SELECT payload_digest,payload_size.*FOR UPDATE").WillReturnError(sql.ErrNoRows)
			m.ExpectQuery("SELECT size FROM blobs").WillReturnError(sql.ErrNoRows)
			m.ExpectExec("INSERT INTO blob_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectExec("INSERT INTO blobs").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectExec("INSERT INTO daemon_markers").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectQuery("SELECT size FROM blobs").WillReturnError(sql.ErrNoRows)
			m.ExpectExec("INSERT INTO blob_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectExec("INSERT INTO blobs").WillReturnResult(sqlmock.NewResult(0, 1))
			m.ExpectExec("REPLACE INTO daemon_markers").WillReturnResult(sqlmock.NewResult(0, 1))
			if fail {
				m.ExpectExec("INSERT INTO audit").WillReturnError(errors.New("audit failed"))
				m.ExpectRollback()
			} else {
				m.ExpectExec("INSERT INTO audit").WillReturnResult(sqlmock.NewResult(0, 1))
				m.ExpectCommit()
			}
			v, e := s.AppendProphet(context.Background(), Audit{Actor: "operator", Action: "ProphetChat", Outcome: "queued"}, ProphetEntry{ID: "m", Task: "task", Author: "operator", Text: "hello"})
			if (e != nil) != fail || v.Sequence != 1 || v.Time.IsZero() {
				t.Fatal(v, e)
			}
		})
	}
}
