package daemon

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/external"
)

func TestEnvironmentDeltaNamesOnly(t *testing.T) {
	parent := []string{"PATH=/usr/bin", "HOME=/home/x", "FAE_HOME=/secret/home", "DOLT_ROOT_PATH=/dolt", "GOFLAGS=-daemon", "LANG=en_US.UTF-8"}
	child := []string{"PATH=/shims:/usr/bin", "HOME=/home/x", "TMPDIR=/work/tmp", "LANG=C", "GOFLAGS=-mod=mod"}
	got := environmentDelta(parent, child, []string{"GOFLAGS=-mod=mod"})
	want := controlplane.EnvironmentDelta{Removed: []string{"DOLT_ROOT_PATH", "FAE_HOME"}, Set: []string{"GOFLAGS"}, Replaced: []string{"LANG", "PATH", "TMPDIR"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delta %+v, want %+v", got, want)
	}
	for _, names := range [][]string{got.Removed, got.Set, got.Replaced} {
		for _, name := range names {
			if strings.Contains(name, "=") || strings.Contains(name, "secret") {
				t.Fatal("a value leaked into the delta", name)
			}
		}
	}
}

// FAE-ROB-030, FAE-CP-011: every engine run of a live task is recorded with
// its environment delta. A planted daemon variable is listed as removed by
// name, and its value is never stored.
func TestLiveEngineRunsRecordEnvironmentDelta(t *testing.T) {
	t.Setenv("FAE_PLANTED_SELECTION", "planted-secret-value")
	cfg, ctx, _ := liveDaemonFixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "runs-one", AuthorizingMessage: "message"}, "runs-one"); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitState(t, d, "runs-one", "Done")
	var e envelope
	if err = decodeEnvelope(snapshot.Payload, &e); err != nil {
		t.Fatal(err)
	}
	commands := 0
	for _, p := range e.Job.Provenance {
		if p.Primitive == "HostCommand" {
			commands++
		}
	}
	runs, err := d.store.EngineRuns(ctx, "runs-one", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if commands == 0 || len(runs) < commands {
		t.Fatalf("%d engine runs for %d host-command artifacts", len(runs), commands)
	}
	for _, r := range runs {
		delta := string(r.Environment)
		if !strings.Contains(delta, `"FAE_PLANTED_SELECTION"`) || strings.Contains(delta, "planted-secret-value") || r.Tree == "" || len(r.OutputDigest) != 64 {
			t.Fatalf("engine run %s: %s", r.RunID, delta)
		}
	}
}

// assertOneEffectRowPerCall checks FAE-EXT-006 over a live run: every vendor
// mutation the run made is exactly one external_effects row naming its tool,
// argument digest, pinned values, returned id, outcome and role session.
func assertOneEffectRowPerCall(t *testing.T, d *Daemon, task string, calls []external.Intent) {
	t.Helper()
	keys := map[string]external.Intent{}
	for _, c := range calls {
		keys[c.Key] = c
	}
	rows, err := d.store.ExternalEffects(context.Background(), task, 1000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		call, ok := keys[r.EffectID]
		if !ok {
			continue
		}
		if seen[r.EffectID] {
			t.Fatalf("external call %s has more than one row", r.EffectID)
		}
		seen[r.EffectID] = true
		if r.Tool != call.Tool || r.ArgumentDigest != call.Digest || r.Session != call.Session || r.Session == "" || r.ExternalID == "" || r.Outcome == "" || r.Reconciliation != "validated" || len(r.Pinned) == 0 {
			t.Fatalf("incomplete effect row %+v for call %+v", r, call)
		}
	}
	if len(seen) != len(keys) {
		t.Fatalf("%d of %d external calls audited: %+v", len(seen), len(keys), rows)
	}
}

// assertReviewProjected checks FAE-CP-011's code_reviews row of a merged review.
func assertReviewProjected(t *testing.T, d *Daemon, task, review string, revisions int) {
	t.Helper()
	q := fmt.Sprintf(`SELECT COUNT(*) FROM code_reviews WHERE review_id='%s' AND task_id='%s' AND revisions=%d AND publish_state='published' AND merge_state='merged' AND branch<>'' AND tree_hash<>''`, review, task, revisions)
	if n := countRows(t, d, q); n != 1 {
		t.Fatal("merged review not projected", n)
	}
}
