package daemon

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controlplane"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

func TestEventWindowNativeResumeRestartAndSlowSubscriber(t *testing.T) {
	c, ctx := fixture(t)
	c.EventRetentionAge = time.Hour
	c.EventRetentionCount = 3
	d, err := Start(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	// Fixture events are isolated and use the production transaction seam.
	add := func(n int, old bool) {
		t.Helper()
		err := d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "fixture events", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
			var seq int64
			if e := tx.QueryRowContext(ctx, `SELECT sequence FROM event_clock WHERE id=1 FOR UPDATE`).Scan(&seq); e != nil {
				return e
			}
			for i := 0; i < n; i++ {
				seq++
				at := time.Now().UTC()
				if old {
					at = at.Add(-2 * time.Hour)
				}
				if _, e := tx.ExecContext(ctx, `INSERT INTO events (sequence,kind,job_id,payload,created_at) VALUES (?,'fixture','','{}',?)`, seq, at); e != nil {
					return e
				}
			}
			_, e := tx.ExecContext(ctx, `UPDATE event_clock SET sequence=? WHERE id=1`, seq)
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
		d.notifyEvents()
	}
	add(10, true)
	// Recent lifecycle events also belong to the union; advance test time so the
	// age window expires while count preservation remains exact.
	floor, err := d.store.EventWindow(ctx, time.Hour, 3, time.Now().Add(3*time.Hour))
	if err != nil || floor == 0 {
		t.Fatalf("floor %d: %v", floor, err)
	}
	rolled, err := d.store.EventWindow(ctx, time.Hour, 10000, time.Now().Add(-24*time.Hour))
	if err != nil || rolled != floor {
		t.Fatalf("floor regressed %d %v", rolled, err)
	}
	_, gap, err := d.retainedEvents(ctx, 0, 100)
	if err != nil || gap == nil || gap.ResumeAfter != floor {
		t.Fatalf("gap %+v %v", gap, err)
	}
	rows, gap, err := d.retainedEvents(ctx, floor, 100)
	if err != nil || gap != nil || len(rows) != 3 {
		t.Fatalf("resume %d %+v %v", len(rows), gap, err)
	}
	response, err := call(t, d, "Events", map[string]any{"after": 0}, "")
	var typed *api.RPCError
	if !errors.As(err, &typed) || typed.Code != "event-gap" {
		t.Fatalf("RPC %+v %v", response, err)
	}
	// Exercise authenticated NDJSON gap and exact retained resumption over the
	// actual Unix socket; stale requests never silently omit the prefix.
	token, e := os.ReadFile(d.layout.Credential)
	if e != nil {
		t.Fatal(e)
	}
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", d.layout.Socket)
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	stream := func(cursor int64) *http.Response {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("http://unix/v1/events?after=%d", cursor), nil)
		req.Header.Set("Authorization", hex.EncodeToString(token))
		res, e := client.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		return res
	}
	res := stream(0)
	var wireGap EventGap
	e = json.NewDecoder(res.Body).Decode(&wireGap)
	res.Body.Close()
	if e != nil || wireGap.Kind != "gap" || wireGap.ResumeAfter != floor {
		t.Fatalf("wire gap %+v %v", wireGap, e)
	}
	res = stream(floor)
	decoder := json.NewDecoder(res.Body)
	for _, expected := range rows {
		var event controlplane.Event
		if e = decoder.Decode(&event); e != nil || event.Sequence != expected.Sequence {
			t.Fatalf("resume %+v %v", event, e)
		}
	}
	res.Body.Close()
	// Read queries do not require the daemon writer mutex. Concurrent identical
	// clients complete while it is held (coalescing itself is unit-asserted).
	d.mu.Lock()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw, _ := json.Marshal(map[string]any{"after": floor})
			_, e := api.Call(ctx, d.layout.Socket, token, api.Request{Version: 1, Method: "Events", Params: raw})
			errs <- e
		}()
	}
	wg.Wait()
	d.mu.Unlock()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	// Start caught up; a subscriber that never drains its private queue overflows
	// while an independent fast plane reader still obtains every committed row.
	latest := rows[len(rows)-1].Sequence
	subctx, cancel := context.WithCancel(ctx)
	queue := make(chan controlplane.Event, 100)
	gaps := make(chan EventGap, 1)
	failure := make(chan error, 1)
	go d.produceEvents(subctx, latest, queue, gaps, failure)
	time.Sleep(100 * time.Millisecond)
	add(250, false)
	select {
	case g := <-gaps:
		if g.Reason != "overflow" {
			t.Fatal(g)
		}
	case e := <-failure:
		t.Fatal(e)
	case <-time.After(5 * time.Second):
		t.Fatal("slow subscriber did not overflow")
	}
	cancel()
	all, err := d.store.Events(ctx, latest, 1000)
	if err != nil || len(all) != 250 {
		t.Fatalf("fast reader %d %v", len(all), err)
	}
	stop(t, d)
	d, err = Start(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	afterRestart, err := d.store.EventWindow(ctx, time.Hour, 10000, time.Now().Add(-24*time.Hour))
	if err != nil || afterRestart < floor {
		t.Fatalf("restart floor %d %v", afterRestart, err)
	}
	archived, err := d.store.Events(ctx, 0, 1000)
	if err != nil || len(archived) < 260 {
		t.Fatalf("archive removed %d %v", len(archived), err)
	}
	// The read result includes a machine-readable resume cursor.
	var detail EventGap
	if json.Unmarshal([]byte(typed.Message), &detail) != nil || detail.ResumeAfter != floor {
		t.Fatal(typed)
	}
}
