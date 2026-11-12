package daemon

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTreeNativeStatsFiltersAndRetention(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(defaults.HappyScript())
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, id := range []string{"tree-root", "tree-child", "unrelated"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
		awaitState(t, d, id, "Done")
	}
	for _, id := range []string{"tree-root", "tree-child"} {
		row, err := d.store.GetJob(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		var e envelope
		if err = decodeEnvelope(row.Payload, &e); err != nil {
			t.Fatal(err)
		}
		if id == "tree-child" {
			e.Job.TreeRoot = "tree-root"
			e.Job.TreeDepth = 2
			e.Job.Detached = true
			e.Job.Parent = &engine.ParentRef{Task: "tree-root", Stage: "children", Visit: 1, Detached: true}
		}
		turns := 7
		cost := 0.42
		stat := domain.NewTurnStat(&domain.ProviderSessionStats{NumTurns: &turns, CostUSD: &cost}, time.Now().Add(-time.Second), time.Now())
		stat.Task = id
		stat.Stage = "results"
		stat.Attempt = 1
		e.Job.TurnStats = []domain.TurnStat{stat}
		row.Payload, _ = json.Marshal(e)
		if _, err = d.store.SaveJob(ctx, controlplane.Audit{Actor: "daemon", Action: "tree-fixture", Outcome: "ok"}, row, row.Version); err != nil {
			t.Fatal(err)
		}
	}
	readTree := func() TreePage {
		t.Helper()
		response, err := call(t, d, "Tree", TreeParams{Task: "tree-child", Stats: true}, "")
		if err != nil {
			t.Fatal(err)
		}
		var page TreePage
		if err = json.Unmarshal(response.Result, &page); err != nil {
			t.Fatal(err)
		}
		return page
	}
	page := readTree()
	if page.Root != "tree-root" || page.Tasks != 2 || page.Descendants != 1 || page.MaxDepth != 2 || page.Stats.Sessions != 2 || page.Stats.Cost.Value == nil || *page.Stats.Cost.Value != 0.84 || page.Sequence <= 0 {
		t.Fatalf("tree stats %+v", page)
	}
	cursor, err := d.store.Sequence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"tree-root", "tree-child", "unrelated"} {
		if err = d.store.RecordTaskEvent(ctx, controlplane.Audit{Actor: "daemon", Action: "tool-fixture", Outcome: "ok"}, id, controlplane.Event{Kind: "fixture-tool-call", Payload: json.RawMessage(`{"tool":"read","paths":["a.go"],"allowed":true}`)}); err != nil {
			t.Fatal(err)
		}
	}
	response, err := call(t, d, "Events", map[string]any{"after": cursor, "limit": 100, "tree_root": "tree-root", "kind": "fixture-tool-call"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var events []controlplane.Event
	if err = json.Unmarshal(response.Result, &events); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].ParentTask != "tree-root" || events[1].TreeRoot != "tree-root" {
		t.Fatalf("tree filter %s", response.Result)
	}
	token, err := os.ReadFile(d.layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", d.layout.Socket)
	}}
	defer transport.CloseIdleConnections()
	client := http.Client{Transport: transport, Timeout: 5 * time.Second}
	stream := func(after int64) *http.Response {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://unix/v1/events?after="+strconv.FormatInt(after, 10)+"&tree_root=tree-root&kind=fixture-tool-call", nil)
		req.Header.Set("Authorization", hex.EncodeToString(token))
		res, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := stream(cursor)
	decoder := json.NewDecoder(res.Body)
	for i := 0; i < 2; i++ {
		var e controlplane.Event
		if err = decoder.Decode(&e); err != nil || e.TreeRoot != "tree-root" {
			t.Fatal("stream filter", e, err)
		}
	}
	res.Body.Close()
	if _, err = d.store.EventWindow(ctx, time.Nanosecond, 1, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	page = readTree()
	if page.Stats.Sessions != 2 || page.Stats.Cost.Value == nil || *page.Stats.Cost.Value != 0.84 {
		t.Fatal("event retention erased stats", page)
	}
	res = stream(0)
	defer res.Body.Close()
	var gap EventGap
	if err = json.NewDecoder(res.Body).Decode(&gap); err != nil || gap.Kind != "gap" {
		t.Fatal("filtered reconnect omitted gap", gap, err)
	}
}

// The two scoped policies are validated before a workflow revision is stored.
func TestTreeNativeValidatorScopes(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(defaults.HappyScript())
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, name := range []string{"cap_scope", "run_stats"} {
		doc := defaults.Workflow()
		doc.ID = "invalid-" + name
		want := ""
		if name == "cap_scope" {
			role := doc.Roles["qa"]
			role.CapScope = "host"
			doc.Roles["qa"] = role
			want = "roles.qa.cap_scope"
		} else {
			stage := doc.Stages[doc.Start]
			stage.RunStats = "instance"
			doc.Stages[doc.Start] = stage
			want = "stages." + doc.Start + ".run_stats"
		}
		if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s accepted or wrong refusal: %v", name, err)
		}
		doc.ID = "valid-" + name
		if name == "cap_scope" {
			role := doc.Roles["qa"]
			role.CapScope = "tree"
			doc.Roles["qa"] = role
		} else {
			stage := doc.Stages[doc.Start]
			stage.RunStats = "task"
			doc.Stages[doc.Start] = stage
		}
		if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatalf("valid %s refused: %v", name, err)
		}
	}
}

func TestFanInNewWritesRequireDeclaredListNative(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.Executor = engine.NewScripted(defaults.HappyScript())
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, shape := range []string{"missing", "object", "array"} {
		doc := fanWorkflow()
		doc.ID = "join-" + shape
		join := doc.Stages["join"]
		if shape == "missing" {
			join.Output = ""
		} else {
			join.Output = "Joined"
			doc.Artifacts["Joined"] = workflow.ArtifactType{Version: 1, Schema: json.RawMessage(`{"type":"` + shape + `"}`)}
		}
		doc.Stages["join"] = join
		_, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, "")
		if shape == "array" {
			if err != nil {
				t.Fatal("valid list join refused", err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "stages.join.output") {
			t.Fatalf("%s join accepted or wrong refusal: %v", shape, err)
		}
	}
}
