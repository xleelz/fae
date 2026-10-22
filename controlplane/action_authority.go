package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

type Direction struct {
	Verbs   []string `json:"verbs"`
	Classes []string `json:"classes"`
}
type DirectedMessage struct {
	Text      string    `json:"text"`
	Authorize Direction `json:"authorize"`
}
type actionAuthority struct {
	Message, Class string
	Direction      Direction
}
type actionAuthorityKey struct{}

func includes(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// AuthorizeAction validates delegation and installs a transaction-local consumption
// requirement. Only a successful audited mutation consumes the message.
func (s *Store) AuthorizeAction(ctx context.Context, id, actor, verb, class string, continuation bool, verbClasses ...map[string]string) (context.Context, error) {
	var err error
	if continuation {
		err = s.RequireScheduleAuthority(ctx, id)
	} else {
		err = s.RequireMessage(ctx, id)
	}
	if err != nil {
		return ctx, err
	}
	var ref BlobRef
	if err = s.db.QueryRowContext(ctx, `SELECT m.text_digest,b.size FROM operator_messages m JOIN blobs b ON b.digest=m.text_digest WHERE m.message_id=?`, id).Scan(&ref.Digest, &ref.Size); err != nil {
		return ctx, err
	}
	raw, err := s.GetBlob(ctx, ref)
	if err != nil {
		return ctx, err
	}
	var msg DirectedMessage
	_ = json.Unmarshal(raw, &msg)
	directed := includes(msg.Authorize.Verbs, verb)
	if actor == "controller" {
		directed = directed || includes(msg.Authorize.Classes, class)
	}
	if actor != "operator" && !directed {
		return ctx, errors.New("operator message does not explicitly direct this action")
	}
	consumption := msg.Authorize
	if len(verbClasses) > 0 {
		for _, v := range msg.Authorize.Verbs {
			if c, ok := verbClasses[0][v]; ok {
				consumption.Classes = append(consumption.Classes, c)
			}
		}
	}
	return context.WithValue(ctx, actionAuthorityKey{}, actionAuthority{id, class, consumption}), nil
}
func consumeActionAuthority(ctx context.Context, tx *sql.Tx, audit Audit) error {
	a, ok := ctx.Value(actionAuthorityKey{}).(actionAuthority)
	if !ok {
		return nil
	}
	if audit.MessageID != a.Message {
		return errors.New("mutation citation differs from authorized message")
	}
	var consumed sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT consumed_by FROM operator_messages WHERE message_id=? FOR UPDATE`, a.Message).Scan(&consumed); err != nil {
		return err
	}
	var classes []string
	if consumed.Valid && consumed.String != "" {
		if err := json.Unmarshal([]byte(consumed.String), &classes); err != nil {
			return errors.New("invalid message consumption record")
		}
	}
	for _, prior := range classes {
		if prior != a.Class && !(includes(a.Direction.Classes, prior) && includes(a.Direction.Classes, a.Class)) {
			return fmt.Errorf("operator message already consumed by incompatible class %s", prior)
		}
	}
	if includes(classes, a.Class) {
		return nil
	}
	classes = append(classes, a.Class)
	raw, _ := json.Marshal(classes)
	_, err := tx.ExecContext(ctx, `UPDATE operator_messages SET consumed_by=? WHERE message_id=?`, string(raw), a.Message)
	return err
}
