package daemon

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/providerhealth"
)

func TestJSONDiffNamesEveryChangedPath(t *testing.T) {
	var a, b any
	_ = json.Unmarshal([]byte(`{"x":1,"y":{"z":[1],"k":"v"},"gone":true}`), &a)
	_ = json.Unmarshal([]byte(`{"x":1,"y":{"z":[1,2],"k":"v"},"new":"n"}`), &b)
	changes := []map[string]any{}
	jsonDiff("", a, b, &changes)
	paths := []string{}
	for _, c := range changes {
		paths = append(paths, c["path"].(string))
	}
	if !reflect.DeepEqual(paths, []string{"/gone", "/new", "/y/z"}) {
		t.Fatal(paths)
	}
}

// FAE-API-001: Board filters by state, target, workflow, mode, molecule,
// run, priority and in-flight.
func TestBoardFilters(t *testing.T) {
	d := &Daemon{}
	row := func(id, state, target, mode string, priority int, inFlight bool) BoardItem {
		return BoardItem{JobIdentity: JobIdentity{ID: id, State: state, TargetID: target, WorkflowID: "w"}, Priority: priority, RunID: "r", Mode: mode, InFlight: inFlight, MoleculeID: "m-" + target}
	}
	d.status.Store(statusSnapshot{Updated: time.Now().UTC(), Tasks: []BoardItem{row("a", "Pending", "t1", "Local", 10, false), row("b", "implement", "t2", "Normal", 40, true), row("c", "Retired", "t2", "Normal", 50, false)}})
	for _, tc := range []struct {
		p    BoardParams
		want []string
	}{
		{BoardParams{}, []string{"a", "b"}},
		{BoardParams{IncludeRetired: true, Target: "t2"}, []string{"b", "c"}},
		{BoardParams{States: []string{"implement", "Pending"}}, []string{"a", "b"}},
		{BoardParams{Mode: "normal"}, []string{"b"}},
		{BoardParams{MinPriority: 20}, []string{"b"}},
		{BoardParams{InFlight: true}, []string{"b"}},
		{BoardParams{Molecule: "m-t1"}, []string{"a"}},
		{BoardParams{Workflow: "other"}, []string{}},
		{BoardParams{RunID: "r", IncludeRetired: true}, []string{"a", "b", "c"}},
	} {
		raw, _ := json.Marshal(tc.p)
		page, err := d.board(context.Background(), raw)
		if err != nil {
			t.Fatal(err)
		}
		got := []string{}
		for _, item := range page.Items {
			got = append(got, item.ID)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%+v: %v want %v", tc.p, got, tc.want)
		}
	}
}

// FAE-API-001 contract: Leases, Panes, the ProviderHealth series and
// TargetConfig validate/diff/simulate against the scripted adapter.
func TestAPIReadContracts(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	result := func(method string, p any) map[string]any {
		t.Helper()
		r, e := call(t, d, method, p, "")
		if e != nil {
			t.Fatal(method, e)
		}
		var out map[string]any
		if e = json.Unmarshal(r.Result, &out); e != nil {
			t.Fatal(method, e)
		}
		return out
	}
	leases := result("Leases", map[string]any{})
	for _, field := range []string{"role_slots", "deployment_holds", "heavy_ops", "pane_reservations", "at"} {
		if _, ok := leases[field]; !ok {
			t.Fatalf("Leases lacks %s: %v", field, leases)
		}
	}
	panes := result("Panes", map[string]any{})
	if panes["adapter"] != "scripted" || panes["sessions"] == nil {
		t.Fatal(panes)
	}
	at := time.Now().UTC()
	for i, outcome := range []providerhealth.Outcome{providerhealth.OK, providerhealth.RateLimited} {
		if e := d.store.ObserveProvider(ctx, controlplane.ProviderObservation{ID: "series-" + string(rune('a'+i)), TaskID: "series", Provider: "claude", Model: "m", Outcome: outcome, At: at}); e != nil {
			t.Fatal(e)
		}
	}
	health := result("ProviderHealth", map[string]any{"window_seconds": 600})
	series, _ := health["series"].(map[string]any)
	points, _ := series["claude/m"].([]any)
	if health["window_seconds"] != float64(600) || len(points) != 2 || points[1].(map[string]any)["outcome"] != string(providerhealth.RateLimited) {
		t.Fatal("provider health series", health)
	}
	if _, e := call(t, d, "ProviderHealth", map[string]any{"window_seconds": 0.5}, ""); e == nil {
		t.Fatal("fractional window accepted")
	}
	target := targetFixture(cfg.Workspace)
	if v := result("TargetConfigValidate", map[string]any{"document": target}); v["valid"] != true {
		t.Fatal(v)
	}
	broken := target
	broken.ProtectedBranches = []string{"refs/heads/other"}
	if v := result("TargetConfigValidate", map[string]any{"document": broken}); v["valid"] != false || len(v["errors"].([]any)) != 1 {
		t.Fatal("invalid target validated", v)
	}
	for _, c := range []TargetConfig{target, func() TargetConfig {
		c := target
		c.ProtectedBranches = append([]string{}, target.ProtectedBranches...)
		c.ProtectedBranches = append(c.ProtectedBranches, "refs/heads/release")
		return c
	}()} {
		if _, e := call(t, d, "TargetConfigPut", TargetPutParams{ID: "api-target", Document: c, AuthorizingMessage: "message"}, ""); e != nil {
			t.Fatal(e)
		}
	}
	diff := result("TargetConfigDiff", map[string]any{"id": "api-target", "from": 1, "to": 2})
	changes, _ := diff["changes"].([]any)
	if diff["identical"] != false || len(changes) != 1 || changes[0].(map[string]any)["path"] != "/protected_branches" {
		t.Fatal(diff)
	}
	sim := result("TargetConfigSimulate", map[string]any{"id": "api-target", "changed": []string{"a.go"}, "commands": [][]string{target.AllowedCommands[0], {"rm", "-rf", "/"}}})
	commands, _ := sim["commands"].([]any)
	if len(commands) != 2 || commands[0].(map[string]any)["allowed"] != true || commands[1].(map[string]any)["allowed"] != false || sim["integration_ref"] != "refs/heads/main" {
		t.Fatal(sim)
	}
	if _, e := call(t, d, "TargetConfigSimulate", map[string]any{"id": "api-target", "document": target}, ""); e == nil || !strings.Contains(e.Error(), "exactly one") {
		t.Fatal("ambiguous simulate accepted", e)
	}
}
