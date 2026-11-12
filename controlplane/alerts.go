package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AlertDetailLimit bounds an alert body so its raise event stays inline.
const AlertDetailLimit = 16 << 10

// Alert is one row of the plane alerts log (FAE-OBS-005). Detail is stored by
// content reference; Task, when set, scopes the alert-raised event to that
// task's history. Every writer, including ControllerAction, shares the table.
type Alert struct {
	ID             int64           `json:"id"`
	Severity       string          `json:"severity"`
	Source         string          `json:"source"`
	Task           string          `json:"task,omitempty"`
	Detail         json.RawMessage `json:"detail"`
	CreatedAt      time.Time       `json:"created_at"`
	AcknowledgedAt *time.Time      `json:"acknowledged_at,omitempty"`
}

var errAlertExists = errors.New("alert already recorded")

func normalAlert(a Alert) (Alert, error) {
	if a.Severity == "" {
		a.Severity = "error"
	}
	switch a.Severity {
	case "critical", "error", "warning", "info":
	default:
		return a, errors.New("alert severity must be critical, error, warning or info")
	}
	if a.Source == "" || len(a.Source) > 255 || len(a.Task) > MaxTaskIDBytes {
		return a, errors.New("alert source required (at most 255 bytes)")
	}
	if len(a.Detail) == 0 {
		a.Detail = json.RawMessage(`{}`)
	}
	canonical, e := CanonicalJSON(a.Detail)
	if e != nil {
		return a, fmt.Errorf("alert detail: %w", e)
	}
	if len(canonical) > AlertDetailLimit {
		return a, errors.New("alert detail exceeds 16 KiB")
	}
	a.Detail = canonical
	return a, nil
}

// InsertAlert writes the alert row, its detail blob and an alert-raised event
// inside a caller's Mutate body, so a decision and its alert commit together.
func (s *Store) InsertAlert(ctx context.Context, tx *sql.Tx, a Alert) (int64, error) {
	a, e := normalAlert(a)
	if e != nil {
		return 0, e
	}
	if e = lockEvents(ctx, tx); e != nil {
		return 0, e
	}
	if e = s.putBlob(ctx, tx, a.Detail); e != nil {
		return 0, e
	}
	result, e := tx.ExecContext(ctx, `INSERT INTO alerts(severity,source,text_digest,created_at) VALUES(?,?,?,?)`, a.Severity, a.Source, Digest(a.Detail), time.Now().UTC())
	if e != nil {
		return 0, e
	}
	id, e := result.LastInsertId()
	if e != nil {
		return 0, e
	}
	payload, _ := json.Marshal(map[string]any{"id": id, "severity": a.Severity, "source": a.Source, "detail": a.Detail})
	return id, s.appendEvents(ctx, tx, a.Task, []Event{{Kind: "alert-raised", JobID: a.Task, Payload: payload}}, "")
}

// RaiseAlert records one alert in its own audited transaction. With dedupe, a
// row with the same source and detail (acknowledged or not) is returned
// instead and nothing is written; callers make detail name one episode.
func (s *Store) RaiseAlert(ctx context.Context, audit Audit, a Alert, dedupe bool) (Alert, bool, error) {
	a, e := normalAlert(a)
	if e != nil {
		return a, false, e
	}
	var id int64
	e = s.Mutate(ctx, audit, func(ctx context.Context, tx *sql.Tx) error {
		if dedupe {
			e := tx.QueryRowContext(ctx, `SELECT id FROM alerts WHERE source=? AND text_digest=? ORDER BY id LIMIT 1`, a.Source, Digest(a.Detail)).Scan(&id)
			if e == nil {
				return errAlertExists
			}
			if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		var e error
		id, e = s.InsertAlert(ctx, tx, a)
		return e
	})
	raised := e == nil
	if errors.Is(e, errAlertExists) {
		e = nil
	}
	if e != nil {
		return a, false, e
	}
	row, e := s.alert(ctx, id)
	if e != nil {
		return a, raised, e
	}
	row.Task = a.Task
	return row, raised, nil
}

// AcknowledgeAlert is the audited acknowledgement verb; a second
// acknowledgement of the same alert is refused and writes nothing.
func (s *Store) AcknowledgeAlert(ctx context.Context, audit Audit, id int64) (Alert, error) {
	if id <= 0 {
		return Alert{}, errors.New("alert id required")
	}
	e := s.Mutate(ctx, audit, func(ctx context.Context, tx *sql.Tx) error {
		if e := lockEvents(ctx, tx); e != nil {
			return e
		}
		var acknowledged sql.NullTime
		if e := tx.QueryRowContext(ctx, `SELECT acknowledged_at FROM alerts WHERE id=? FOR UPDATE`, id).Scan(&acknowledged); errors.Is(e, sql.ErrNoRows) {
			return fmt.Errorf("unknown alert %d", id)
		} else if e != nil {
			return e
		}
		if acknowledged.Valid {
			return fmt.Errorf("alert %d already acknowledged at %s", id, acknowledged.Time.UTC().Format(time.RFC3339Nano))
		}
		if _, e := tx.ExecContext(ctx, `UPDATE alerts SET acknowledged_at=? WHERE id=? AND acknowledged_at IS NULL`, time.Now().UTC(), id); e != nil {
			return e
		}
		payload, _ := json.Marshal(map[string]any{"id": id, "actor": audit.Actor, "message": audit.MessageID})
		return s.appendEvents(ctx, tx, "", []Event{{Kind: "alert-acknowledged", Payload: payload}}, "")
	})
	if e != nil {
		return Alert{}, e
	}
	return s.alert(ctx, id)
}

const alertColumns = `SELECT a.id,a.severity,a.source,a.created_at,a.acknowledged_at,a.text_digest,b.size FROM alerts a JOIN blobs b ON b.digest=a.text_digest`

func (s *Store) scanAlerts(ctx context.Context, query string, args ...any) ([]Alert, error) {
	rows, e := s.db.QueryContext(ctx, query, args...)
	if e != nil {
		return nil, e
	}
	type entry struct {
		alert Alert
		ref   BlobRef
	}
	var entries []entry
	for rows.Next() {
		var v entry
		var acknowledged sql.NullTime
		if e = rows.Scan(&v.alert.ID, &v.alert.Severity, &v.alert.Source, &v.alert.CreatedAt, &acknowledged, &v.ref.Digest, &v.ref.Size); e != nil {
			rows.Close()
			return nil, e
		}
		if acknowledged.Valid {
			at := acknowledged.Time.UTC()
			v.alert.AcknowledgedAt = &at
		}
		v.alert.CreatedAt = v.alert.CreatedAt.UTC()
		entries = append(entries, v)
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return nil, e
	}
	rows.Close()
	out := make([]Alert, 0, len(entries))
	for _, v := range entries {
		if v.alert.Detail, e = s.GetBlob(ctx, v.ref); e != nil {
			return nil, e
		}
		out = append(out, v.alert)
	}
	return out, nil
}
func (s *Store) alert(ctx context.Context, id int64) (Alert, error) {
	rows, e := s.scanAlerts(ctx, alertColumns+` WHERE a.id=?`, id)
	if e != nil {
		return Alert{}, e
	}
	if len(rows) != 1 {
		return Alert{}, fmt.Errorf("unknown alert %d", id)
	}
	return rows[0], nil
}

// Alerts pages alert rows in id order after a cursor, optionally only those
// not yet acknowledged.
func (s *Store) Alerts(ctx context.Context, after int64, limit int, unacknowledged bool) ([]Alert, error) {
	if after < 0 || limit < 1 || limit > 500 {
		return nil, errors.New("alert cursor must be nonnegative and limit 1..500")
	}
	query := alertColumns + ` WHERE a.id>?`
	if unacknowledged {
		query += ` AND a.acknowledged_at IS NULL`
	}
	return s.scanAlerts(ctx, query+` ORDER BY a.id LIMIT ?`, after, limit)
}

// AlertsChanged returns rows raised after a cursor or acknowledged after a
// time, in id order, for mirroring the plane log into the alerts file.
func (s *Store) AlertsChanged(ctx context.Context, after int64, acknowledgedAfter time.Time, limit int) ([]Alert, error) {
	if after < 0 || limit < 1 || limit > 500 {
		return nil, errors.New("alert cursor must be nonnegative and limit 1..500")
	}
	return s.scanAlerts(ctx, alertColumns+` WHERE a.id>? OR a.acknowledged_at>? ORDER BY a.id LIMIT ?`, after, acknowledgedAfter.UTC(), limit)
}

// RecordTaskEvent appends one audited event to a task's history without
// changing its job, for example a lease wait (FAE-OBS-003).
func (s *Store) RecordTaskEvent(ctx context.Context, audit Audit, task string, event Event) error {
	if task == "" || len(task) > MaxTaskIDBytes {
		return errors.New("task id required")
	}
	event.JobID = task
	return s.Mutate(ctx, audit, func(ctx context.Context, tx *sql.Tx) error {
		if e := lockEvents(ctx, tx); e != nil {
			return e
		}
		return s.appendEvents(ctx, tx, task, []Event{event}, "")
	})
}

// LastJobEvent returns a task's most recent event of one kind.
func (s *Store) LastJobEvent(ctx context.Context, task, kind string) (Event, error) {
	var ev Event
	e := s.db.QueryRowContext(ctx, `SELECT sequence,kind,job_id,payload,created_at FROM events WHERE job_id=? AND kind=? ORDER BY sequence DESC LIMIT 1`, task, kind).Scan(&ev.Sequence, &ev.Kind, &ev.JobID, &ev.Payload, &ev.Timestamp)
	ev.Timestamp = ev.Timestamp.UTC()
	if e==nil {var restored []Event;restored,e=s.restoreEventMetadata(ctx,[]Event{ev});if e==nil{ev=restored[0]}}
	return ev, e
}
