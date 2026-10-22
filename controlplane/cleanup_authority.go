package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// AuthorizeReadOnlyEffect preserves normal message freshness and consumed-class
// rules without writing a consumption/audit row. Explicit restrictions remain
// binding even for the instance operator. CTL-042 requires plane reads only.
func (s *Store) AuthorizeReadOnlyEffect(ctx context.Context, id, actor, verb, class string, classes map[string]string) error {
	authorized, err := s.AuthorizeAction(ctx, id, actor, verb, class, false, classes)
	if err != nil {
		return err
	}
	a, ok := authorized.Value(actionAuthorityKey{}).(actionAuthority)
	if !ok {
		return errors.New("missing action authority")
	}
	// Use original directions, before the canonical helper expands verb classes.
	raw, err := s.MessageText(ctx, id)
	if err != nil {
		return err
	}
	var message DirectedMessage
	_ = json.Unmarshal([]byte(raw), &message)
	if (len(message.Authorize.Verbs) > 0 || len(message.Authorize.Classes) > 0) && !includes(message.Authorize.Verbs, verb) && !includes(message.Authorize.Classes, class) {
		return errors.New("operator message restricts this cleanup action")
	}
	var consumed sql.NullString
	if err = s.db.QueryRowContext(ctx, `SELECT consumed_by FROM operator_messages WHERE message_id=?`, id).Scan(&consumed); err != nil {
		return err
	}
	var prior []string
	if consumed.Valid && consumed.String != "" {
		if err = json.Unmarshal([]byte(consumed.String), &prior); err != nil {
			return errors.New("invalid message consumption record")
		}
	}
	for _, used := range prior {
		if used != class && !(includes(a.Direction.Classes, used) && includes(a.Direction.Classes, class)) {
			return fmt.Errorf("operator message already consumed by incompatible class %s", used)
		}
	}
	return nil
}
