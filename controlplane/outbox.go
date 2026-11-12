package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// TicketEmission is the durable mutation boundary. Intent is immutable once set;
// Started survives ambiguous replies and forbids another mutation.
type TicketEmission struct {
	Key       string          `json:"key"`
	Task      string          `json:"task"`
	Room      string          `json:"room"`
	Step      string          `json:"step"`
	Position  int64           `json:"position"`
	TicketID  string          `json:"ticket_id"`
	Intent    json.RawMessage `json:"intent,omitempty"`
	Started   bool            `json:"started"`
	Emitted   bool            `json:"emitted"`
	Attempts  int64           `json:"attempts"`
	LastError string          `json:"last_error,omitempty"`
}
type TicketOutboxStats struct {
	Depth     int `json:"depth"`
	Failures  int `json:"failures"`
	Uncertain int `json:"uncertain"`
}

var Migration5 = []string{`CREATE TABLE IF NOT EXISTS ticket_emissions (outbox_key CHAR(64) PRIMARY KEY, task_id VARCHAR(255) NOT NULL, room VARCHAR(255) NOT NULL, position BIGINT NOT NULL, payload LONGTEXT NOT NULL, UNIQUE KEY ticket_order(task_id,room,position))`}

func (s *Store) EnqueueTicket(ctx context.Context, row TicketEmission) error {
	if len(row.Key) != 64 || row.Task == "" || row.Room == "" || row.Step == "" || row.Position < 0 || row.Started || row.Emitted || row.Attempts != 0 || len(row.Intent) != 0 {
		return errors.New("invalid new ticket emission")
	}
	var existing []byte
	err := s.db.QueryRowContext(ctx, "SELECT payload FROM ticket_emissions WHERE outbox_key=?", row.Key).Scan(&existing)
	if err == nil {
		var prior TicketEmission
		if json.Unmarshal(existing, &prior) != nil || prior.Task != row.Task || prior.Room != row.Room || prior.Step != row.Step || prior.Position != row.Position {
			return errors.New("ticket emission identity changed")
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	descriptor, _ := json.Marshal(row)
	return s.Mutate(ctx, Audit{Actor: "daemon", Action: "ticket enqueue", Inputs: descriptor, Outcome: "queued"}, func(ctx context.Context, tx *sql.Tx) error {
		var old []byte
		err := tx.QueryRowContext(ctx, "SELECT payload FROM ticket_emissions WHERE outbox_key=?", row.Key).Scan(&old)
		if err == nil {
			var prior TicketEmission
			if json.Unmarshal(old, &prior) != nil || prior.Task != row.Task || prior.Room != row.Room || prior.Step != row.Step || prior.Position != row.Position {
				return errors.New("ticket emission identity changed")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		raw, _ := json.Marshal(row)
		_, err = tx.ExecContext(ctx, "INSERT INTO ticket_emissions (outbox_key,task_id,room,position,payload) VALUES (?,?,?,?,?)", row.Key, row.Task, row.Room, row.Position, string(raw))
		if err != nil {
			return err
		}
		if err = s.upsertOutboxTx(ctx, tx, row, time.Now().UTC()); err != nil {
			return err
		}
		if err = lockEvents(ctx, tx); err != nil {
			return err
		}
		return s.appendEvents(ctx, tx, row.Task, []Event{{Kind: "ticket-outbox-queued", JobID: row.Task, Payload: raw}}, "")
	})
}
func (s *Store) TicketEmissions(ctx context.Context, task, room string) ([]TicketEmission, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT payload FROM ticket_emissions WHERE task_id=? AND room=? ORDER BY position", task, room)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TicketEmission{}
	for rows.Next() {
		var raw []byte
		var r TicketEmission
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveTicket uses the entire previous value as CAS; a lost acknowledgement may
// only be adopted when the readback equals the proposed state exactly.
func (s *Store) SaveTicket(ctx context.Context, before, after TicketEmission) error {
	if before.Key != after.Key || before.Task != after.Task || before.Room != after.Room || before.Step != after.Step || before.Position != after.Position || (before.Started && !after.Started) || (before.Emitted && !after.Emitted) || after.Attempts < before.Attempts || (len(before.Intent) > 0 && string(before.Intent) != string(after.Intent)) {
		return errors.New("invalid ticket emission transition")
	}
	if (after.Started && len(after.Intent) == 0) || (after.Emitted && (after.TicketID == "" || len(after.Intent) == 0)) || (before.TicketID != "" && before.TicketID != after.TicketID) {
		return errors.New("invalid ticket outcome proof")
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	err := s.Mutate(ctx, Audit{Actor: "daemon", Action: "ticket checkpoint", Inputs: json.RawMessage(b), Outcome: ticketOutcome(after)}, func(ctx context.Context, tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, "UPDATE ticket_emissions SET payload=? WHERE outbox_key=? AND payload=?", string(b), before.Key, string(a))
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrConflict
		}
		if err = s.upsertOutboxTx(ctx, tx, after, time.Now().UTC()); err != nil {
			return err
		}
		if err = lockEvents(ctx, tx); err != nil {
			return err
		}
		return s.appendEvents(ctx, tx, after.Task, []Event{{Kind: "ticket-outbox-" + ticketOutcome(after), JobID: after.Task, Payload: json.RawMessage(b)}}, "")
	})
	if err == nil {
		return nil
	}
	var actual string
	if readErr := s.db.QueryRowContext(ctx, "SELECT payload FROM ticket_emissions WHERE outbox_key=?", before.Key).Scan(&actual); readErr == nil && actual == string(b) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("ticket emission CAS: %w", ErrConflict)
}

// TicketStats counts the emissions not yet emitted (the depth) and, of
// those, the started (uncertain) and the failed ones. It is one aggregate
// over the ticket_outbox projection's pending rows, through its
// (emitted,task_id) index, and reads no emission payload (FAE-CP-009): every
// ticket_emissions write projects its row there in the same transaction
// (upsertOutboxTx), last_error_digest being NULL exactly when the emission
// has no last error.
func (s *Store) TicketStats(ctx context.Context) (TicketOutboxStats, error) {
	var stats TicketOutboxStats
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN started THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN last_error_digest IS NULL THEN 0 ELSE 1 END),0) FROM ticket_outbox WHERE emitted=FALSE`).Scan(&stats.Depth, &stats.Uncertain, &stats.Failures)
	return stats, err
}

func ticketOutcome(r TicketEmission) string {
	if r.Emitted {
		return "emitted"
	}
	if r.LastError != "" {
		return "failed"
	}
	if r.Started {
		return "uncertain"
	}
	if len(r.Intent) > 0 {
		return "prepared"
	}
	return "pending"
}
