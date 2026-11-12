package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
)

// scriptedRunEvents runs one scripted task on a fresh instance and returns its
// committed event sequence as kind and job pairs, stopping the daemon so the
// shutdown events are included.
func scriptedRunEvents(t *testing.T) []string {
	t.Helper()
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop(t, d)
		}
	}()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "deterministic", Tree: "tree", AuthorizingMessage: "message"}, "deterministic"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "deterministic", "Done")
	stop(t, d)
	stopped = true
	// Read the closed instance's plane through a second daemon start: its own
	// lifecycle events follow the first run's and are cut at the restart.
	again, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, again)
	events, err := again.store.Events(context.Background(), 0, 100000)
	if err != nil {
		t.Fatal(err)
	}
	out := []string{}
	starts := 0
	for _, ev := range events {
		if ev.Kind == "daemon-starting" {
			if starts++; starts == 2 {
				break
			}
		}
		stage := ""
		var p struct {
			Stage string `json:"stage"`
			Role  string `json:"role"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil {
			stage = p.Stage + "/" + p.Role
		}
		out = append(out, ev.Kind+" "+ev.JobID+" "+stage)
	}
	return out
}

// FAE-API-020: the event stream carries daemon, Dolt-child and session
// lifecycle events, and a scripted run's event sequence is a deterministic
// function of the run. A scripted run takes no lease: lease events
// (lease_granted, lease_released, lease_cancelled) come only from live
// admission's plane lease pools, so this test does not assert them.
func TestScriptedRunEventSequenceIsDeterministic(t *testing.T) {
	first := scriptedRunEvents(t)
	second := scriptedRunEvents(t)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("event sequences differ:\n%s\n---\n%s", strings.Join(first, "\n"), strings.Join(second, "\n"))
	}
	kinds := map[string]int{}
	for _, row := range first {
		kinds[strings.Fields(row)[0]]++
	}
	for _, kind := range []string{"daemon-starting", "dolt-started", "daemon-ready", "task_admitted", "dispatch", "transition", "session-launched", "session-reaped", "daemon-shutting-down", "daemon-stopped"} {
		if kinds[kind] == 0 {
			t.Errorf("no %s event in %v", kind, kinds)
		}
	}
	if kinds["session-launched"] != kinds["session-reaped"] {
		t.Errorf("sessions launched %d, reaped %d", kinds["session-launched"], kinds["session-reaped"])
	}
	for kind := range controlplane.LifecycleKinds {
		if strings.HasPrefix(kind, "dolt-c") || kind == "dolt-restarted" {
			continue // TestDaemonDoltChildCrashRecovers
		}
		if kinds[kind] != 1 {
			t.Errorf("%s recorded %d times", kind, kinds[kind])
		}
	}
}
