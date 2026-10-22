package controlplane

import (
	"context"
	"errors"
)

// RequireScheduleAuthority checks the durable creation citation's integrity,
// without renewing or imposing the age limit for a new operator action. The
// authenticated create operation grants recurrence until the schedule ends or
// is retired; pause/resume/retire continue to require fresh operator authority.
func (s *Store) RequireScheduleAuthority(ctx context.Context, id string) error {
	if id == "" {
		return errors.New("schedule authorizing message required")
	}
	var ref BlobRef
	var author string
	if e := s.db.QueryRowContext(ctx, `SELECT m.text_digest,b.size,m.author FROM operator_messages m JOIN blobs b ON b.digest=m.text_digest WHERE m.message_id=?`, id).Scan(&ref.Digest, &ref.Size, &author); e != nil {
		return e
	}
	if author != "operator" {
		return errors.New("schedule citation is not an operator message")
	}
	raw, e := s.GetBlob(ctx, ref)
	if e != nil {
		return e
	}
	if len(raw) == 0 {
		return errors.New("empty schedule citation")
	}
	return nil
}
