package controlplane

import (
	"context"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"strings"
	"testing"
)

func TestBacklogGraphValidation(t *testing.T) {
	ids := map[string]bool{"a": true, "b": true, "c": true}
	cases := []struct {
		edges   []BacklogEdge
		ok      bool
		message string
	}{
		{[]BacklogEdge{{"a", "b", "blocking"}, {"b", "c", "blocking"}}, true, ""},
		{[]BacklogEdge{{"a", "b", "relates"}, {"b", "a", "relates"}}, true, ""},
		{[]BacklogEdge{{"a", "b", "blocking"}, {"b", "a", "blocking"}}, false, "cycle"},
		{[]BacklogEdge{{"a", "a", "relates"}}, false, "invalid"},
		{[]BacklogEdge{{"a", "absent", "blocking"}}, false, "invalid"},
		{[]BacklogEdge{{"a", "b", "unknown"}}, false, "invalid"},
	}
	for _, c := range cases {
		err := ValidateBacklogEdges(ids, c.edges)
		if (err == nil) != c.ok || (err != nil && !strings.Contains(err.Error(), c.message)) {
			t.Fatal(c, err)
		}
	}
}

func TestBacklogBatchRollbackIncludesEdgesAndEveryItem(t *testing.T) {
	for _, failAudit := range []bool{false, true} {
		t.Run(map[bool]string{false: "second-item", true: "audit"}[failAudit], func(t *testing.T) {
			s, m := fixture(t, 1)
			one := jobFixture()
			two := jobFixture()
			two.ID, two.Payload = "second", taskPayload("second", "Pending")
			m.ExpectBegin()
			expectClock(m)
			m.ExpectExec("INSERT INTO backlog_edges").WithArgs("job", "second", "blocking", audit().Actor, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(1, 1))
			for n, j := range []JobSnapshot{one, two} {
				expectBind(m, n == 1)
				m.ExpectQuery("SELECT size FROM blobs").WillReturnRows(sqlmock.NewRows([]string{"size"}).AddRow(len(j.Payload)))
				if n == 1 && !failAudit {
					m.ExpectExec("INSERT INTO jobs").WillReturnError(errors.New("injected second insert"))
					break
				}
				m.ExpectExec("INSERT INTO jobs").WillReturnResult(sqlmock.NewResult(1, 1))
				expectProjection(m)
				m.ExpectExec("INSERT INTO backlog_items").WillReturnResult(sqlmock.NewResult(1, 1))
				m.ExpectQuery("SELECT sequence FROM event_clock").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(n))
				expectTaskEventMetadata(m)
				m.ExpectExec("INSERT INTO events").WillReturnResult(sqlmock.NewResult(1, 1))
				m.ExpectExec("UPDATE event_clock").WillReturnResult(sqlmock.NewResult(1, 1))
			}
			if failAudit {
				m.ExpectExec("INSERT INTO audit").WillReturnError(errors.New("audit failed"))
			}
			m.ExpectRollback()
			_, err := s.AdmitBacklog(context.Background(), audit(), []BacklogAdmission{{Job: one, Item: BacklogItem{Priority: 10}}, {Job: two, Item: BacklogItem{Priority: 10}}}, []BacklogEdge{{"job", "second", "blocking"}})
			if err == nil {
				t.Fatal("partial mutation accepted")
			}
			if err = m.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestBacklogEdgeAbsentRollsBack(t *testing.T) {
	s, m := fixture(t, 1)
	m.ExpectBegin()
	expectClock(m)
	m.ExpectExec("DELETE FROM backlog_edges").WillReturnResult(sqlmock.NewResult(0, 0))
	m.ExpectRollback()
	if err := s.EditBacklogEdge(context.Background(), audit(), BacklogEdge{"a", "b", "blocking"}, true); err == nil {
		t.Fatal("absent edge accepted")
	}
}
