package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"testing"
	"time"
)

func TestMessageConsumptionIsAtomicWithAudit(t *testing.T) {
	for _, tc := range []struct {
		name, prior                 string
		classes                     []string
		bodyFail, auditFail, refuse bool
	}{
		{name: "first"}, {name: "inherited citation"}, {name: "same", prior: `["task"]`}, {name: "incompatible", prior: `["governance"]`, refuse: true}, {name: "explicit", prior: `["governance"]`, classes: []string{"governance", "task"}}, {name: "body rollback", bodyFail: true}, {name: "audit rollback", auditFail: true}, {name: "corrupt", prior: `bad`, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := fixture(t, 1)
			ctx := context.WithValue(context.Background(), actionAuthorityKey{}, actionAuthority{"m", "task", Direction{Classes: tc.classes}})
			m.ExpectBegin()
			m.ExpectQuery("SELECT consumed_by").WithArgs("m").WillReturnRows(sqlmock.NewRows([]string{"consumed"}).AddRow(tc.prior))
			if !tc.refuse && tc.prior != `["task"]` {
				m.ExpectExec("UPDATE operator_messages").WithArgs(sqlmock.AnyArg(), "m").WillReturnResult(sqlmock.NewResult(0, 1))
			}
			if !tc.refuse && !tc.bodyFail {
				q := m.ExpectExec("INSERT INTO audit").WithArgs("controller", "Cancel", "m", `{}`, Digest([]byte(`{}`)), "ok", sqlmock.AnyArg())
				if tc.auditFail {
					q.WillReturnError(errors.New("audit failed"))
				} else {
					q.WillReturnResult(sqlmock.NewResult(1, 1))
				}
			}
			if tc.refuse || tc.bodyFail || tc.auditFail {
				m.ExpectRollback()
			} else {
				m.ExpectCommit()
			}
			called := false
			messageID := "m"
			if tc.name == "inherited citation" {
				messageID = ""
			}
			e := s.Mutate(ctx, Audit{Actor: "controller", Action: "Cancel", MessageID: messageID, Outcome: "ok"}, func(context.Context, *sql.Tx) error {
				called = true
				if tc.bodyFail {
					return errors.New("body failed")
				}
				return nil
			})
			if (e != nil) != (tc.refuse || tc.bodyFail || tc.auditFail) || called == tc.refuse {
				t.Fatalf("called=%v err=%v", called, e)
			}
			if e := m.ExpectationsWereMet(); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestDelegatedMessageDirections(t *testing.T) {
	for _, tc := range []struct {
		name, actor, text, verb, class string
		allow                          bool
	}{
		{"capacity grant", "controller", `{"authorize":{"classes":["capacity"]}}`, "WorkflowLimitSet", "capacity", true},
		{"runtime cannot set capacity", "controller", `{"authorize":{"classes":["runtime"]}}`, "WorkflowLimitSet", "capacity", false},
		{"exact verb cannot replace capacity grant", "controller", `{"authorize":{"verbs":["WorkflowLimitSet"]}}`, "WorkflowLimitSet", "capacity", false},
		{"capacity cannot authorize runtime", "controller", `{"authorize":{"classes":["capacity"]}}`, "Drain", "runtime", false},
		{"controller plain", "controller", "please act", "Cancel", "task", false},
		{"controller exact", "controller", `{"authorize":{"verbs":["Cancel"]}}`, "Cancel", "task", true},
		{"controller wrong verb", "controller", `{"authorize":{"verbs":["Admit"]}}`, "Cancel", "task", false},
		{"controller class", "controller", `{"authorize":{"classes":["task"]}}`, "Cancel", "task", true},
		{"prophet class insufficient", "prophet", `{"authorize":{"classes":["task"]}}`, "RestartWithInformation", "task", false},
		{"prophet exact", "prophet", `{"authorize":{"verbs":["RestartWithInformation"]}}`, "RestartWithInformation", "task", true},
		{"operator plain", "operator", "please act", "Cancel", "task", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, m := fixture(t, 1)
			raw := []byte(tc.text)
			digest := Digest(raw)
			blob := func() {
				m.ExpectQuery("SELECT size,chunks").WithArgs(digest).WillReturnRows(sqlmock.NewRows([]string{"size", "chunks"}).AddRow(len(raw), 1))
				m.ExpectQuery("SELECT chunk_index,data").WithArgs(digest).WillReturnRows(sqlmock.NewRows([]string{"index", "data"}).AddRow(0, raw))
			}
			m.ExpectQuery("SELECT m.text_digest").WithArgs("m").WillReturnRows(sqlmock.NewRows([]string{"digest", "size", "author"}).AddRow(digest, len(raw), "operator"))
			blob()
			m.ExpectQuery("SELECT created_at").WithArgs("m").WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(time.Now()))
			m.ExpectQuery("SELECT m.text_digest").WithArgs("m").WillReturnRows(sqlmock.NewRows([]string{"digest", "size"}).AddRow(digest, len(raw)))
			blob()
			_, e := s.AuthorizeAction(context.Background(), "m", tc.actor, tc.verb, tc.class, false)
			if (e == nil) != tc.allow {
				t.Fatal(e)
			}
			if e := m.ExpectationsWereMet(); e != nil {
				t.Fatal(e)
			}
		})
	}
}
