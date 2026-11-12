package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// EventsOfKindSince pages committed events of one kind at or after a time,
// oldest first (FAE-API-001 time series); more reports a truncated page.
func (s *Store) EventsOfKindSince(ctx context.Context, kind string, since time.Time, limit int) (events []Event, more bool, err error) {
	if kind == "" || limit < 1 || limit > 10000 {
		return nil, false, errors.New("event kind and a limit of 1..10000 required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,kind,job_id,payload,created_at FROM events WHERE kind=? AND created_at>=? ORDER BY sequence LIMIT ?`, kind, since.UTC(), limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	events = []Event{}
	for rows.Next() {
		var ev Event
		if err = rows.Scan(&ev.Sequence, &ev.Kind, &ev.JobID, &ev.Payload, &ev.Timestamp); err != nil {
			return nil, false, err
		}
		events = append(events, ev)
	}
	if len(events) > limit {
		events, more = events[:limit], true
	}
	if err:=errors.Join(rows.Err(),rows.Close());err!=nil{return nil,false,err}
	events,err=s.restoreEventMetadata(ctx,events)
	return events,more,err
}

// MarkersWithPrefix returns every daemon marker under a name prefix, failing
// rather than truncating past the bound.
func (s *Store) MarkersWithPrefix(ctx context.Context, prefix string, bound int) (map[string]json.RawMessage, error) {
	if prefix == "" || strings.ContainsAny(prefix, "%_\\") || bound < 1 || bound > 10000 {
		return nil, errors.New("literal marker prefix and a bound of 1..10000 required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT name,payload_digest,payload_size FROM daemon_markers WHERE name LIKE ? ORDER BY name LIMIT ?`, prefix+"%", bound+1)
	if err != nil {
		return nil, err
	}
	refs := map[string]BlobRef{}
	for rows.Next() {
		var name string
		var ref BlobRef
		if err = rows.Scan(&name, &ref.Digest, &ref.Size); err != nil {
			rows.Close()
			return nil, err
		}
		refs[name] = ref
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(refs) > bound {
		return nil, errors.New("marker enumeration bound exceeded")
	}
	out := map[string]json.RawMessage{}
	for name, ref := range refs {
		raw, e := s.GetBlob(ctx, ref)
		if e != nil {
			return nil, e
		}
		out[name] = raw
	}
	return out, nil
}

// LatestRunID is the run of the most recently admitted task (FAE-CLI-013);
// found is false on an empty plane.
func (s *Store) LatestRunID(ctx context.Context) (run string, found bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT run_id FROM tasks ORDER BY created_at DESC, task_id DESC LIMIT 1`).Scan(&run)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return run, err == nil, err
}
