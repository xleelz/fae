package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var errRetentionUnchanged = errors.New("retention floor already advanced")

func (s *Store) EventFloor(ctx context.Context) (int64, error) {
	var floor int64
	err := s.db.QueryRowContext(ctx, `SELECT floor_sequence FROM event_retention WHERE id=1`).Scan(&floor)
	return floor, err
}

// EventWindow advances the logical floor through the audited mutation seam.
// It is called by daemon maintenance, never by event read handlers. No event is
// emitted for maintenance: doing so would recursively advance count retention.
// Archival rows remain append-only; this is not physical compaction or backup.
func (s *Store) EventWindow(ctx context.Context, age time.Duration, count int, now time.Time) (int64, error) {
	if age <= 0 || count < 1 {
		return 0, errors.New("invalid event retention window")
	}
	floor, err := s.EventFloor(ctx)
	if err != nil {
		return 0, err
	}
	var oldest sql.NullInt64
	if err = s.db.QueryRowContext(ctx, `SELECT MIN(sequence) FROM events WHERE created_at>=?`, now.Add(-age)).Scan(&oldest); err != nil {
		return 0, err
	}
	var first int64
	err = s.db.QueryRowContext(ctx, `SELECT sequence FROM events ORDER BY sequence DESC LIMIT 1 OFFSET ?`, count-1).Scan(&first)
	if errors.Is(err, sql.ErrNoRows) {
		first = 1
		err = nil
	}
	if err != nil {
		return 0, err
	}
	if oldest.Valid && oldest.Int64 < first {
		first = oldest.Int64
	}
	proposed := first - 1
	if proposed <= floor {
		return floor, nil
	}
	inputs, _ := json.Marshal(map[string]any{"floor": proposed, "count": count, "age_ns": int64(age)})
	err = s.Mutate(ctx, Audit{Actor: "daemon", Action: "event retention floor", Outcome: "advanced", Inputs: inputs}, func(ctx context.Context, tx *sql.Tx) error {
		if e := tx.QueryRowContext(ctx, `SELECT floor_sequence FROM event_retention WHERE id=1 FOR UPDATE`).Scan(&floor); e != nil {
			return e
		}
		if proposed <= floor {
			return errRetentionUnchanged
		}
		_, e := tx.ExecContext(ctx, `UPDATE event_retention SET floor_sequence=? WHERE id=1`, proposed)
		return e
	})
	if errors.Is(err, errRetentionUnchanged) {
		return floor, nil
	}
	if err != nil { // A committed acknowledgement can be lost; exact monotonic readback proves adoption.
		durable, e := s.EventFloor(ctx)
		if e == nil && durable >= proposed {
			return durable, nil
		}
		return 0, err
	}
	return proposed, nil
}
