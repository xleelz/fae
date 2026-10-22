package controlplane

import (
	"context"
	"errors"
	"strings"
)

// Live effects require an explicit phrase, not merely a general admission grant.
func (s *Store) RequireExternalAuthority(ctx context.Context, id string) error {
	var ref BlobRef
	var author string
	if e := s.db.QueryRowContext(ctx, `SELECT m.text_digest,b.size,m.author FROM operator_messages m JOIN blobs b ON b.digest=m.text_digest WHERE m.message_id=?`, id).Scan(&ref.Digest, &ref.Size, &author); e != nil {
		return e
	}
	raw, e := s.GetBlob(ctx, ref)
	if e != nil {
		return e
	}
	if author != "operator" || !strings.Contains(string(raw), "I approve live external effects") {
		return errors.New("authorizing operator message must contain: I approve live external effects")
	}
	return nil
}
