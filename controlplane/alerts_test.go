package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// doltStore starts a private Dolt child and returns a migrated store.
func doltStore(t *testing.T) (context.Context, *Store) {
	t.Helper()
	binary := os.Getenv("FAE_TEST_DOLT_BINARY")
	if binary == "" {
		t.Skip("set FAE_TEST_DOLT_BINARY to enable isolated Dolt integration")
	}
	root := t.TempDir()
	data := filepath.Join(root, "plane")
	if e := os.Mkdir(data, 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("DOLT_ROOT_PATH", filepath.Join(root, "config"))
	t.Setenv("DOLT_DISABLE_EVENT_FLUSH", "1")
	init := exec.Command(binary, "init", "--name", "Fae Tests", "--email", "tests@localhost")
	init.Dir = data
	if output, e := init.CombinedOutput(); e != nil {
		t.Fatalf("initialize isolated Dolt: %v: %s", e, output)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	server, e := StartServer(ctx, ServerConfig{Binary: binary, DataDir: data, StopWait: 2 * time.Second})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { server.Stop() })
	dsn := fmt.Sprintf("root@tcp(127.0.0.1:%d)/plane?parseTime=true", server.Port())
	var s *Store
	for i := 0; i < 200; i++ {
		if s, e = Open(ctx, dsn, Options{}); e == nil {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if e != nil {
		t.Fatalf("Dolt readiness: %v", e)
	}
	t.Cleanup(func() { s.Close() })
	if e = s.Migrate(ctx); e != nil {
		t.Fatal(e)
	}
	return ctx, s
}

func TestAlertValidationBeforeSQL(t *testing.T) {
	s, _ := fixture(t, 1)
	for _, a := range []Alert{{Detail: json.RawMessage(`{}`)}, {Source: "x", Detail: json.RawMessage(`{`)}, {Source: strings.Repeat("s", 256), Detail: json.RawMessage(`{}`)}, {Source: "x", Severity: "loud", Detail: json.RawMessage(`{}`)}, {Source: "x", Detail: json.RawMessage(`"` + strings.Repeat("d", AlertDetailLimit) + `"`)}} {
		if _, _, e := s.RaiseAlert(context.Background(), Audit{Actor: "daemon", Action: "alert", Outcome: "alert"}, a, false); e == nil {
			t.Fatalf("invalid alert accepted: %+v", a.Source)
		}
	}
	if _, e := s.AcknowledgeAlert(context.Background(), audit(), 0); e == nil {
		t.Fatal("alert id zero accepted")
	}
	if _, e := s.Alerts(context.Background(), -1, 10, false); e == nil {
		t.Fatal("negative cursor accepted")
	}
	if _, e := s.Alerts(context.Background(), 0, 501, false); e == nil {
		t.Fatal("oversized page accepted")
	}
}

func TestAlertsRaiseDedupeListAndAcknowledge(t *testing.T) {
	ctx, s := doltStore(t)
	raise := Audit{Actor: "daemon", Action: "alert-raised", Outcome: "alert"}
	leak, raised, e := s.RaiseAlert(ctx, raise, Alert{Source: "process-leak", Task: "task-1", Detail: json.RawMessage(`{"command":"sleep","processes":201}`)}, false)
	if e != nil || !raised || leak.ID <= 0 || leak.Severity != "error" || leak.CreatedAt.IsZero() {
		t.Fatalf("raise %+v %v %v", leak, raised, e)
	}
	edge := Alert{Severity: "warning", Source: "dead-edge", Detail: json.RawMessage(`{"from":"a","to":"b"}`)}
	first, raised, e := s.RaiseAlert(ctx, raise, edge, true)
	if e != nil || !raised {
		t.Fatal(raised, e)
	}
	again, raised, e := s.RaiseAlert(ctx, raise, edge, true)
	if e != nil || raised || again.ID != first.ID {
		t.Fatalf("dedupe raised a second row: %+v %v %v", again, raised, e)
	}
	// The pre-existing controller writer remains readable through the same list.
	if e = s.ControllerAction(ctx, Audit{Actor: "daemon", Action: "drive-exited", Outcome: "stopped"}, json.RawMessage(`{"cause":"drive-failure"}`), true); e != nil {
		t.Fatal(e)
	}
	rows, e := s.Alerts(ctx, 0, 10, false)
	if e != nil || len(rows) != 3 || rows[0].ID != leak.ID || rows[1].ID != first.ID || rows[2].Source != "controller" || string(rows[1].Detail) != `{"from":"a","to":"b"}` {
		t.Fatalf("alerts %+v %v", rows, e)
	}
	ack := Audit{Actor: "operator", Action: "AlertAcknowledge", MessageID: "m", Outcome: "ok"}
	acked, e := s.AcknowledgeAlert(ctx, ack, leak.ID)
	if e != nil || acked.AcknowledgedAt == nil || acked.ID != leak.ID {
		t.Fatalf("ack %+v %v", acked, e)
	}
	if _, e = s.AcknowledgeAlert(ctx, ack, leak.ID); e == nil || !strings.Contains(e.Error(), "already acknowledged") {
		t.Fatalf("second acknowledgement %v", e)
	}
	if _, e = s.AcknowledgeAlert(ctx, ack, 9999); e == nil {
		t.Fatal("unknown alert acknowledged")
	}
	open, e := s.Alerts(ctx, 0, 10, true)
	if e != nil || len(open) != 2 || open[0].ID != first.ID {
		t.Fatalf("unacknowledged %+v %v", open, e)
	}
	after, e := s.Alerts(ctx, first.ID, 10, false)
	if e != nil || len(after) != 1 || after[0].Source != "controller" {
		t.Fatalf("cursor %+v %v", after, e)
	}
	events, e := s.Events(ctx, 0, 100)
	if e != nil {
		t.Fatal(e)
	}
	kinds := map[string]int{}
	for _, ev := range events {
		kinds[ev.Kind]++
		if ev.Kind == "alert-raised" && ev.JobID == "task-1" && !strings.Contains(string(ev.Payload), `"process-leak"`) {
			t.Fatalf("raise event %s", ev.Payload)
		}
	}
	if kinds["alert-raised"] != 2 || kinds["alert-acknowledged"] != 1 {
		t.Fatalf("events %v", kinds)
	}
	history, e := s.JobEvents(ctx, "task-1", 0, 10)
	if e != nil || len(history) != 1 {
		t.Fatalf("task alert missing from task history: %+v %v", history, e)
	}
	var audits int
	if e = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE action IN ('alert-raised','AlertAcknowledge')`).Scan(&audits); e != nil || audits != 3 {
		t.Fatalf("audits %d %v", audits, e)
	}
	if e = s.RecordTaskEvent(ctx, Audit{Actor: "daemon", Action: "lease-wait", Outcome: "ok"}, "task-1", Event{Kind: "lease-waiting", Payload: json.RawMessage(`{"lease":"heavy-op"}`)}); e != nil {
		t.Fatal(e)
	}
	last, e := s.LastJobEvent(ctx, "task-1", "lease-waiting")
	if e != nil || last.Kind != "lease-waiting" || last.JobID != "task-1" || last.Timestamp.IsZero() {
		t.Fatalf("last event %+v %v", last, e)
	}
	if _, e = s.LastJobEvent(ctx, "task-1", "park"); e == nil {
		t.Fatal("missing event found")
	}
	all, truncated, e := s.EventsSince(ctx, time.Time{}, 1000)
	if e != nil || truncated || len(all) < 4 {
		t.Fatalf("events since %d %v %v", len(all), truncated, e)
	}
	newest, truncated, e := s.EventsSince(ctx, time.Time{}, 2)
	if e != nil || !truncated || len(newest) != 2 || newest[0].Sequence != all[len(all)-2].Sequence || newest[1].Sequence != all[len(all)-1].Sequence {
		t.Fatalf("newest events %+v %v %v", newest, truncated, e)
	}
	if none, truncated, e := s.EventsSince(ctx, time.Now().Add(time.Hour), 10); e != nil || truncated || len(none) != 0 {
		t.Fatalf("future window %+v %v %v", none, truncated, e)
	}
	changed, e := s.AlertsChanged(ctx, first.ID, time.Time{}, 10)
	if e != nil || len(changed) != 2 || changed[0].ID != leak.ID || changed[1].Source != "controller" {
		t.Fatalf("changed %+v %v", changed, e)
	}
}
