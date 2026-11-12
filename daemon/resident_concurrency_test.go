package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FAE-SCH-005, -009 with resident sessions on: the shipped default's intake
// turns use the workflow-global spec role, cap 2. Two tasks' turns of that
// role run at once, each on its own resident identity, and a third task's turn
// waits for the role's cap (withheld as cap), not behind a shared resident.
func TestDaemonResidentRoleCapConcurrency(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	cfg.MaxInFlight = -1
	cfg.Live.Adapter = "multiplexer"
	cfg.Live.HerdrBinary = testHerdr(t)
	cfg.Live.ResidentSessions = true
	cfg.Live.Environment["LIVE_DAEMON_WAIT"] = "1"
	// Waiting turns must outlast the observation window, however slow the host.
	cfg.Live.TimeoutSeconds = 600
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	tasks := []string{"shared-a", "shared-b", "shared-c"}
	for _, id := range tasks {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	key := roleKey("default", "global", "spec")
	sessions := filepath.Join(d.layout.Root, "sessions")
	residents := func() map[string]string {
		out := map[string]string{}
		audits, _ := filepath.Glob(filepath.Join(sessions, "session-*", "resident-session.json"))
		for _, path := range audits {
			var audit struct {
				Resident string `json:"resident_id"`
				Session  string `json:"session_id"`
			}
			if raw, err := os.ReadFile(path); err == nil && json.Unmarshal(raw, &audit) == nil {
				out[audit.Session] = audit.Resident
			}
		}
		return out
	}
	state := ""
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(100 * time.Millisecond) {
		live := d.live.resident.Live()
		pool, perr := d.store.LeaseSnapshot(ctx, key)
		withheld := map[string]string{}
		if w := d.withheld.Load(); w != nil {
			withheld = w.Tasks
		}
		cap := 0
		for _, id := range tasks {
			if withheld[id] == "cap" {
				cap++
			}
		}
		owners := residents()
		running := map[string]bool{}
		for _, s := range live {
			if owners[s.ID] != "" {
				running[owners[s.ID]] = true
			}
		}
		state = fmt.Sprintf("live=%d residents=%v holders=%d waiting=%d withheld=%v err=%v", len(live), running, len(pool.Holders), len(pool.Waiting), withheld, perr)
		if len(live) == 2 && len(running) == 2 && perr == nil && len(pool.Holders) == 2 && len(pool.Waiting) == 1 && cap == 1 {
			break
		}
		if time.Now().After(deadline) {
			for _, id := range tasks {
				if snapshot, err := d.store.GetJob(ctx, id); err == nil {
					var e envelope
					if json.Unmarshal(snapshot.Payload, &e) == nil {
						t.Logf("%s: state=%s parked=%v reason=%s", id, e.Job.State, e.Job.Parked, e.Job.Reason)
					}
				}
			}
			t.Fatal("two turns of a cap-2 workflow-global role did not run at once on distinct residents with the third waiting for the cap:", state)
		}
	}
}
