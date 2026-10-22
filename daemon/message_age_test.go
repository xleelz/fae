package daemon

import (
	"context"
	"database/sql"
	"github.com/xleelz/fae/controlplane"
	"testing"
	"time"
)

func TestConfiguredMessageAgeSurvivesPlaneRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.MessageMaxAge = 72 * time.Hour
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	e = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "age authorization", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		_, e := tx.ExecContext(ctx, "UPDATE operator_messages SET created_at=? WHERE message_id='message'", time.Now().UTC().Add(-48*time.Hour))
		return e
	})
	if e != nil {
		t.Fatal(e)
	}
	if e = d.store.RequireMessage(ctx, "message"); e != nil {
		t.Fatal("configured age ignored", e)
	}
	stop(t, d)
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = d.store.RequireMessage(ctx, "message"); e != nil {
		t.Fatal("configured age lost on restart", e)
	}
	stop(t, d)
	cfg.MessageMaxAge = 0
	d, e = Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if d.config.MessageMaxAge != 24*time.Hour {
		t.Fatal(d.config.MessageMaxAge)
	}
	if e = d.store.RequireMessage(ctx, "message"); e == nil {
		t.Fatal("default age did not refuse stale authority")
	}
}
func TestRejectNegativeMessageAge(t *testing.T) {
	if d, e := Start(context.Background(), Config{MessageMaxAge: -time.Second}); e == nil || d != nil {
		t.Fatal("negative age accepted", e)
	}
}
