package controlplane

import (
	"context"
	"encoding/json"
	"errors"
)

// ReplicationMarkers returns the complete bounded outbox set, never a truncated list.
func (s *Store) ReplicationMarkers(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,payload_digest,payload_size FROM daemon_markers WHERE name LIKE 'backup-replication/%' ORDER BY name LIMIT 10001`)
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
		if ref.Size < 1 || ref.Size > 65536 {
			rows.Close()
			return nil, errors.New("replication outbox payload exceeds bound")
		}
		refs[name] = ref
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(refs) > 10000 {
		return nil, errors.New("replication outbox enumeration bound exceeded")
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
