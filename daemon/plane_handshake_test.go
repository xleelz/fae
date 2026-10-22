package daemon

import (
	"context"
	"github.com/go-sql-driver/mysql"
	"net"
	"testing"
	"time"
)

func TestPlaneHandshakeBounded(t *testing.T) {
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, e := listener.Accept()
		if e == nil {
			accepted <- c
		}
	}()
	cfg := mysql.NewConfig()
	cfg.Net = "tcp"
	cfg.Addr = listener.Addr().String()
	cfg.Timeout = time.Second
	started := time.Now()
	store, e := openPlaneAttempt(context.Background(), cfg.FormatDSN(), 100*time.Millisecond, 24*time.Hour)
	if store != nil {
		store.Close()
		t.Fatal("silent listener accepted as SQL")
	}
	if e == nil || time.Since(started) > time.Second {
		t.Fatal("unbounded silent MySQL handshake", e, time.Since(started))
	}
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(time.Second):
		t.Fatal("did not test connected handshake")
	}
}
func TestPlaneAttemptContextDoesNotTimeoutLaterQueries(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer stop(t, d)
	time.Sleep(1100 * time.Millisecond)
	if _, e := d.store.GetMarker(ctx, "clean-stop"); e != nil {
		t.Fatal("startup context leaked into runtime queries", e)
	}
}
