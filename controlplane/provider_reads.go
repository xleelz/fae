package controlplane

import (
	"context"
	"errors"
	"time"
)

// ProviderLogLine reads the transactional operator log. There is no independent
// append-only file whose write could race the health/event commit.
type ProviderLogLine struct {
	Sequence int64     `json:"sequence"`
	Session  string    `json:"session"`
	Line     string    `json:"line"`
	Created  time.Time `json:"created"`
}

func (s *Store) ProviderObservations(ctx context.Context, after int64, limit int) ([]ProviderLogLine, error) {
	if after < 0 || limit < 1 || limit > 500 {
		return nil, errors.New("provider log cursor must be nonnegative and limit 1..500")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,message_id,outcome,created_at FROM audit WHERE action='provider observation' AND id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []ProviderLogLine{}
	for rows.Next() {
		var line ProviderLogLine
		if err := rows.Scan(&line.Sequence, &line.Session, &line.Line, &line.Created); err != nil {
			return nil, err
		}
		result = append(result, line)
	}
	return result, rows.Err()
}
