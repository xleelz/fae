package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

// routeBlocked gives a Blocked task a declared remediation route, as a real
// gate failure would, so stage-aware resume has a stage to return to.
func routeBlocked(t *testing.T, d *Daemon, id string, mode string) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.jobs[id]
	var e envelope
	if err := decodeEnvelope(s.Payload, &e); err != nil {
		t.Fatal(err)
	}
	e.Job.BlockedFrom, e.Job.BlockedRoute = "intake-gate", "stop"
	if mode != "keep" {
		e.Job.Disposition.Mode = ""
	}
	saved, err := d.persist(d.ctx, s, e, nil)
	if err != nil {
		t.Fatal(err)
	}
	d.jobs[id] = saved
}

// FAE-CLI-011/-013/-030: admission reports run and disposition; resume works per
// run (default the drain run) or per task; show is a read-only snapshot.
func TestRunLevelResumeShowAndAdmissionResult(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	d.driveStop.Store(true)
	d.signal()
	awaitDrive(t, d, false, 1)
	admitted := map[string]AdmissionResult{}
	for id, run := range map[string]string{"blocked": "r1", "pending": "r1", "other": "r2", "drain": "", "nodisp": "r3"} {
		r, err := call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", RunID: run, AuthorizingMessage: "message"}, id)
		if err != nil {
			t.Fatal(err)
		}
		var out AdmissionResult
		if err = json.Unmarshal(r.Result, &out); err != nil {
			t.Fatal(err)
		}
		admitted[id] = out
	}
	if a := admitted["blocked"]; a.RunID != "r1" || a.Disposition.Mode != "Local" || a.DryRun || a.ID != "blocked" || a.Version != 1 {
		t.Fatalf("admission result %+v", a)
	}
	if a := admitted["drain"]; a.RunID != cfg.RunID && a.RunID != "backlog-drain" {
		t.Fatalf("default run %+v", a)
	}
	for _, id := range []string{"blocked", "other", "nodisp"} {
		for _, method := range []string{"Escalate", "RestartWithInformation", "Block"} {
			if _, err = call(t, d, method, TaskControlParams{ID: id, Reason: "hold", Information: "continue", AuthorizingMessage: "message"}, ""); err != nil {
				t.Fatal(id, method, err)
			}
		}
	}
	routeBlocked(t, d, "blocked", "keep")
	routeBlocked(t, d, "other", "keep")
	routeBlocked(t, d, "nodisp", "clear")
	resume := func(p ResumeRunParams) (map[string]json.RawMessage, error) {
		p.AuthorizingMessage = "message"
		r, err := call(t, d, "ResumeRun", p, "")
		if err != nil {
			return nil, err
		}
		var out map[string]json.RawMessage
		return out, json.Unmarshal(r.Result, &out)
	}
	out, err := resume(ResumeRunParams{RunID: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	var resumed []ResumedTask
	var skipped []SkippedTask
	_ = json.Unmarshal(out["resumed"], &resumed)
	_ = json.Unmarshal(out["skipped"], &skipped)
	if len(resumed) != 1 || resumed[0].ID != "blocked" || resumed[0].Stage != "stop" || len(skipped) != 1 || skipped[0].ID != "pending" || skipped[0].Reason == "" {
		t.Fatalf("run resume %s", out)
	}
	if j, _ := d.store.GetJob(ctx, "other"); j.State != "Blocked" {
		t.Fatal("resume crossed into another run", j.State)
	}
	if out, err = resume(ResumeRunParams{Task: "other"}); err != nil || string(out["run_id"]) != `"r2"` {
		t.Fatalf("task resume %s %v", out, err)
	}
	// FAE-CLI-013: without --run-id, resume takes the latest run.
	if _, err = call(t, d, "Admit", AdmitParams{ID: "latest", Tree: "tree", RunID: "r9", AuthorizingMessage: "message"}, "latest"); err != nil {
		t.Fatal(err)
	}
	if out, err = resume(ResumeRunParams{}); err != nil || string(out["run_id"]) != `"r9"` || string(out["resumed"]) != `[]` {
		t.Fatalf("default run resume %s %v", out, err)
	}
	for _, p := range []ResumeRunParams{{Task: "pending"}, {RunID: "missing"}, {RunID: "r1", Task: "other"}, {Task: "missing"}, {RunID: "r3"}} {
		if _, err = resume(p); err == nil {
			t.Fatalf("%+v accepted", p)
		}
	}
	if _, err = resume(ResumeRunParams{RunID: "r3"}); err == nil || !strings.Contains(err.Error(), "nodisp has no disposition") {
		t.Fatalf("missing disposition did not fail closed: %v", err)
	}
	// show reads the run and the task without writing anything.
	before, _ := d.store.GetJob(ctx, "blocked")
	r, err := call(t, d, "Show", ShowParams{RunID: "r1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	var shown struct {
		RunID string     `json:"run_id"`
		Tasks []ShowTask `json:"tasks"`
	}
	if err = json.Unmarshal(r.Result, &shown); err != nil || shown.RunID != "r1" || len(shown.Tasks) != 2 {
		t.Fatalf("show run %s %v", r.Result, err)
	}
	for _, task := range shown.Tasks {
		if task.ID == "blocked" && (task.Stage != "stop" || task.RunID != "r1" || task.Disposition.Mode != "Local") {
			t.Fatalf("show task %+v", task)
		}
	}
	if _, err = call(t, d, "Show", ShowParams{Task: "blocked", RunID: "r1"}, ""); err == nil {
		t.Fatal("ambiguous show accepted")
	}
	if after, _ := d.store.GetJob(ctx, "blocked"); after.Version != before.Version {
		t.Fatal("show mutated the task")
	}
}

// FAE-CLI-010: serve --default-mode supplies the disposition of submissions that state none.
func TestDefaultModeAppliesToUnstatedSubmissions(t *testing.T) {
	cfg, ctx := fixture(t)
	cfg.DefaultMode = "Normal"
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "unstated", Tree: "tree", AuthorizingMessage: "message"}, "unstated"); err == nil || !strings.Contains(err.Error(), "Normal disposition requires live runtime") {
		t.Fatalf("default Normal not applied: %v", err)
	}
	r, err := call(t, d, "Admit", AdmitParams{ID: "stated", Tree: "tree", Mode: "Local", AuthorizingMessage: "message"}, "stated")
	var out AdmissionResult
	if err != nil || json.Unmarshal(r.Result, &out) != nil || out.Disposition.Mode != "Local" {
		t.Fatalf("explicit mode %s %v", r.Result, err)
	}
}
