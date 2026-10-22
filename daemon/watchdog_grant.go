package daemon

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
)

type watchdogGrant struct {
	Message    string    `json:"message"`
	Registered time.Time `json:"registered"`
	Revoked    bool      `json:"revoked"`
}

func watchdogGrantKey(message string) string {
	h := sha256.Sum256([]byte(message))
	return "watchdog-grant/" + hex.EncodeToString(h[:])
}
func (d *Daemon) watchdogAuthorized(ctx context.Context, message string) (bool, error) {
	raw, e := d.store.GetMarker(ctx, watchdogGrantKey(message))
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	var grant watchdogGrant
	if e = decode(raw, &grant); e != nil {
		return false, e
	}
	if grant.Revoked {
		return false, errors.New("watchdog grant revoked")
	}
	return grant.Message == message && !grant.Registered.IsZero(), nil
}

// A registration grants only ongoing alert/rearm authority until explicit
// revocation. A revoked citation cannot automatically register itself again.
func (d *Daemon) watchdogGrantAction(ctx context.Context, p controller.Params, audit controlplane.Audit) error {
	message := p.AuthorizingMessage
	if p.Action == "watchdog-revoke" {
		if p.GrantMessage == "" {
			return errors.New("grant_message required")
		}
		message = p.GrantMessage
	}
	raw, e := d.store.GetMarker(ctx, watchdogGrantKey(message))
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	grant := watchdogGrant{Message: message, Registered: time.Now().UTC()}
	if e == nil {
		if e = decode(raw, &grant); e != nil {
			return e
		}
		if grant.Message != message {
			return errors.New("watchdog grant identity mismatch")
		}
		if p.Action == "watchdog-register" {
			if grant.Revoked {
				return errors.New("watchdog grant revoked; register with a new authorizing message")
			}
			return nil
		}
	}
	grant.Revoked = p.Action == "watchdog-revoke"
	raw, _ = json.Marshal(grant)
	audit.Action = p.Action
	if e = d.store.PutMarker(ctx, audit, watchdogGrantKey(message), raw); e != nil {
		return e
	}
	if e = d.store.ControllerAction(ctx, audit, raw, false); e != nil {
		return e
	}
	d.notifyEvents()
	return nil
}
