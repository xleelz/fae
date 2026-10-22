package daemon

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
)

func TestWatchdogDurableScopedAuthorityAndRevocation(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	if _, e = call(t, d, "Message", map[string]string{"id": "unregistered", "text": "old general authority"}, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "watchdog-register", AuthorizingMessage: "message"}); e != nil {
		t.Fatal(e)
	}
	e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "age watchdog authority", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE operator_messages SET created_at=?", time.Now().UTC().Add(-48*time.Hour))
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"watchdog-register", "rearm", "alert"} {
		if _, e = controllerCall(t, d, controller.Params{Action: action, AuthorizingMessage: "message", Cause: "test"}); e != nil {
			t.Fatal(action, e)
		}
		if _, e = controllerCall(t, d, controller.Params{Action: action, AuthorizingMessage: "unregistered", Cause: "test"}); e == nil {
			t.Fatal("unregistered aged authority", action)
		}
	}
	for _, action := range []string{"stop-drive", "prepare", "watchdog-revoke"} {
		if _, e = controllerCall(t, d, controller.Params{Action: action, AuthorizingMessage: "message", GrantMessage: "message"}); e == nil {
			t.Fatal("grant widened", action)
		}
	}
	if _, e = call(t, d, "Drain", map[string]string{"authorizing_message": "message"}, ""); e == nil {
		t.Fatal("grant widened to general mutation")
	}
	if _, e = call(t, d, "Message", map[string]string{"id": "revoke", "text": "Revoke persistent watchdog authority"}, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "watchdog-revoke", AuthorizingMessage: "revoke", GrantMessage: "message"}); e != nil {
		t.Fatal(e)
	}
	for _, action := range []string{"watchdog-register", "rearm", "alert"} {
		if _, e = controllerCall(t, d, controller.Params{Action: action, AuthorizingMessage: "message"}); e == nil {
			t.Fatal("revoked authority reused", action)
		}
	}
	// Revocation also prevents a still-fresh watchdog from registering again.
	if _, e = controllerCall(t, d, controller.Params{Action: "watchdog-register", AuthorizingMessage: "revoke"}); e != nil {
		t.Fatal(e)
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "watchdog-revoke", AuthorizingMessage: "revoke", GrantMessage: "revoke"}); e != nil {
		t.Fatal(e)
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "watchdog-register", AuthorizingMessage: "revoke"}); e == nil {
		t.Fatal("fresh revoked citation resurrected")
	}
}
