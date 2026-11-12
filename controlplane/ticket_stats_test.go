package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// FAE-CP-009: TicketStats counts from the outbox projection's pending rows
// and reads no emission payload. The counts match the emissions' own states:
// queued, started (uncertain), failed and emitted. An emitted emission whose
// stored payload no longer decodes changes nothing, since it is never read.
func TestTicketStatsReadsNoEmissionPayload(t *testing.T) {
	s := testPlane(t, SchemaVersion)
	ctx := context.Background()
	enqueue := func(i int) TicketEmission {
		r := TicketEmission{Key: fmt.Sprintf("%064x", i), Task: fmt.Sprintf("task-%d", i), Room: "room", Step: "Pending"}
		if e := s.EnqueueTicket(ctx, r); e != nil {
			t.Fatal(e)
		}
		return r
	}
	save := func(before TicketEmission, change func(*TicketEmission)) {
		after := before
		change(&after)
		if e := s.SaveTicket(ctx, before, after); e != nil {
			t.Fatal(e)
		}
	}
	enqueue(1)
	save(enqueue(2), func(r *TicketEmission) {
		r.Started, r.Attempts, r.Intent = true, 1, json.RawMessage(`{"id":2}`)
	})
	save(enqueue(3), func(r *TicketEmission) { r.Attempts, r.LastError = 1, "refused" })
	emitted := enqueue(4)
	save(emitted, func(r *TicketEmission) {
		r.Started, r.Emitted, r.Attempts, r.TicketID, r.Intent, r.LastError = true, true, 2, "T-4", json.RawMessage(`{"id":4}`), "earlier failure"
	})
	want := TicketOutboxStats{Depth: 3, Uncertain: 1, Failures: 1}
	if stats, e := s.TicketStats(ctx); e != nil || stats != want {
		t.Fatalf("stats %+v %v, want %+v", stats, e, want)
	}
	if _, e := s.db.ExecContext(ctx, `UPDATE ticket_emissions SET payload='{' WHERE outbox_key=?`, emitted.Key); e != nil {
		t.Fatal(e)
	}
	if stats, e := s.TicketStats(ctx); e != nil || stats != want {
		t.Fatalf("stats read an emission payload: %+v %v", stats, e)
	}
}
