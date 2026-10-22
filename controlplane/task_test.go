package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/DATA-DOG/go-sqlmock"
	"strings"
	"testing"
)

func taskRows(state string) *sqlmock.Rows {
	return sqlmock.NewRows(strings.Split(taskColumns, ",")).AddRow("t", "r", strings.Repeat("a", 64), "target", 1, state, 0, 20, 10, `{"mode":"Local"}`)
}
func TestTransitionFaultRollbackAtEveryWrite(t *testing.T) {
	for fail := 0; fail < 4; fail++ {
		s, m := fixture(t, 1)
		m.ExpectBegin()
		m.ExpectQuery("SELECT .* FROM tasks").WillReturnRows(taskRows("Pending"))
		m.ExpectQuery("SELECT COUNT.* FROM artifacts").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
		queries := []string{"INSERT INTO transitions", "UPDATE tasks", "UPDATE backlog_items", "INSERT INTO audit"}
		for i, q := range queries {
			if i == fail {
				m.ExpectExec(q).WillReturnError(errors.New("injected"))
				break
			}
			m.ExpectExec(q).WillReturnResult(sqlmock.NewResult(1, 1))
		}
		m.ExpectRollback()
		e := s.Transition(context.Background(), audit(), Transition{TaskID: "t", From: "Pending", To: "stage", Owner: "test", EvidenceDigest: strings.Repeat("a", 64)}, func(Task, Transition) error { return nil })
		if e == nil {
			t.Fatalf("write %d did not fail", fail)
		}
	}
}
func TestTransitionRejectsStaleTerminalMissingEvidence(t *testing.T) {
	for _, state := range []string{"Done", "Retired", "Cancelled", "different", "Pending"} {
		t.Run(state, func(t *testing.T) {
			s, m := fixture(t, 1)
			m.ExpectBegin()
			m.ExpectQuery("SELECT .* FROM tasks").WillReturnRows(taskRows(state))
			from := state
			if state == "different" {
				from = "Pending"
			}
			if state == "Pending" {
				m.ExpectQuery("SELECT COUNT.* FROM artifacts").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
			}
			m.ExpectRollback()
			e := s.Transition(context.Background(), audit(), Transition{TaskID: "t", From: from, To: "stage", Owner: "test", EvidenceDigest: strings.Repeat("a", 64)}, func(Task, Transition) error { return nil })
			if e == nil {
				t.Fatal("unsafe transition")
			}
		})
	}
}
func TestTaskDispositionRefusals(t *testing.T) {
	for _, raw := range []string{`{"mode":"Wrong"}`, `{"mode":"Local","live":true}`, `{"mode":"Normal","live":true}`, `{"mode":"Local","unknown":false}`, `null`, `{}`} {
		s, _ := fixture(t, 1)
		task := Task{ID: "t", RunID: "r", MoleculeID: strings.Repeat("a", 64), TargetID: "target", TargetRevision: 1, State: "Pending", AttemptLimit: 20, Priority: 10, Disposition: json.RawMessage(raw)}
		if e := s.CreateTask(context.Background(), audit(), task); e == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestDigestMustBeHex(t *testing.T) {
	s, _ := fixture(t, 1)
	if e := s.Transition(context.Background(), audit(), Transition{TaskID: "t", From: "Pending", To: "stage", Owner: "test", EvidenceDigest: strings.Repeat("z", 64)}, func(Task, Transition) error { return nil }); e == nil {
		t.Fatal("invalid digest accepted")
	}
}
func TestWorkflowValidationBeforeWrite(t *testing.T) {
	s, _ := fixture(t, 1)
	sentinel := errors.New("invalid workflow")
	if _, e := s.WriteWorkflow(context.Background(), audit(), "w", "n", json.RawMessage(`{}`), func([]byte) error { return sentinel }); !errors.Is(e, sentinel) {
		t.Fatal(e)
	}
}
