package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
)

// FAE-VCS-009: a task resting in a commit stage with no lease or owner beyond
// the bound raises exactly one audited alert; a running commit never does.
func TestStuckCommitStageRaisesOneAlert(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.StuckCommitBound = 300 * time.Millisecond
	script := defaults.HappyScript()
	stuck := append([]engine.ScriptStep(nil), script["commit"]...)
	stuck[0].Class = "uncertain"
	script["parked/commit"] = stuck
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, id := range []string{"parked", "flows"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	awaitState(t, d, "flows", "Done")
	// The shared alert path writes the alert row and its alert-raised event atomically.
	alerts := func() []string {
		events, err := d.store.Events(ctx, 0, 100000)
		if err != nil {
			t.Fatal(err)
		}
		var bodies []string
		for _, e := range events {
			if e.Kind == "alert-raised" && strings.Contains(string(e.Payload), `"source":"stuck-commit-stage"`) {
				bodies = append(bodies, string(e.Payload))
			}
		}
		return bodies
	}
	deadline := time.Now().Add(30 * time.Second)
	var found []string
	for len(found) == 0 && time.Now().Before(deadline) {
		d.signal()
		time.Sleep(200 * time.Millisecond)
		for _, body := range alerts() {
			if strings.Contains(body, `"task":"parked"`) && strings.Contains(body, `"stage":"commit"`) {
				found = append(found, body)
			}
		}
	}
	if len(found) != 1 {
		t.Fatal("stuck commit stage not alerted once", found)
	}
	for i := 0; i < 3; i++ {
		d.signal()
		time.Sleep(200 * time.Millisecond)
	}
	count := 0
	for _, body := range alerts() {
		if strings.Contains(body, `"task":"flows"`) {
			t.Fatal("flowing task alerted", body)
		}
		if strings.Contains(body, `"task":"parked"`) {
			count++
		}
	}
	var payload struct {
		Detail map[string]any `json:"detail"`
	}
	if count != 1 || json.Unmarshal([]byte(found[0]), &payload) != nil || payload.Detail["parked"] != true {
		t.Fatal("alert repeated or lacks its record", count, found)
	}
}

// FAE-VCS-009: the detector's rest clock and its one alert are durable: a
// daemon that lost its memory (a restart) resumes the recorded episode
// instead of re-arming the clock, and never repeats the alert. (A scripted
// restart re-dispatches the uncertain commit, which rightly starts a new
// episode, so the restart's memory loss is simulated here.)
func TestStuckCommitClockSurvivesRestart(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.StuckCommitBound = time.Hour
	script := defaults.HappyScript()
	stuck := append([]engine.ScriptStep(nil), script["commit"]...)
	stuck[0].Class = "uncertain"
	script["parked/commit"] = stuck
	cfg.Executor = engine.NewScripted(script)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "parked", Tree: "tree", AuthorizingMessage: "message"}, "parked"); err != nil {
		t.Fatal(err)
	}
	// Wait for the episode of the parked commit dispatch itself.
	var first commitRest
	for deadline := time.Now().Add(time.Minute); ; {
		if time.Now().After(deadline) {
			t.Fatal("resting episode never recorded")
		}
		d.signal()
		time.Sleep(100 * time.Millisecond)
		raw, e := d.store.GetMarker(ctx, stuckCommitKey("parked"))
		s, g := d.store.GetJob(ctx, "parked")
		var env envelope
		if e == nil && g == nil && json.Unmarshal(raw, &first) == nil && json.Unmarshal(s.Payload, &env) == nil && env.Job.Parked && first.Dispatch == env.Job.Sequence {
			break
		}
	}
	alerts := func() int {
		events, err := d.store.Events(ctx, 0, 100000)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range events {
			if e.Kind == "alert-raised" && strings.Contains(string(e.Payload), `"source":"stuck-commit-stage"`) && strings.Contains(string(e.Payload), first.Since.Format(time.RFC3339)[:16]) {
				n++
			}
		}
		return n
	}
	forget := func(now time.Time) {
		d.mu.Lock()
		defer d.mu.Unlock()
		d.commitRest = nil
		if err := d.detectStuckCommits(ctx, now); err != nil {
			t.Fatal(err)
		}
	}
	forget(first.Since.Add(time.Hour - time.Second))
	if alerts() != 0 {
		t.Fatal("alerted before the bound")
	}
	forget(first.Since.Add(time.Hour + time.Second))
	if alerts() != 1 {
		t.Fatal("durable rest clock re-armed: no alert one bound after the recorded start")
	}
	forget(first.Since.Add(3 * time.Hour))
	if alerts() != 1 {
		t.Fatal("restart repeated the stuck-commit alert")
	}
}
