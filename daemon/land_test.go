package daemon

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/workflow"
)

func awaitLanding(t *testing.T, d *Daemon, id string) LandingRecord {
	t.Helper()
	// A hang guard: a changed landing record is progress (see hangGuard).
	guard := newHangGuard(3 * time.Minute)
	last := ""
	for {
		response, e := call(t, d, "LandStatus", map[string]string{"id": id}, "")
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(response.Result)
		var rec LandingRecord
		if e = json.Unmarshal(raw, &rec); e != nil {
			t.Fatal(e)
		}
		if rec.State != "running" {
			return rec
		}
		if string(raw) != last {
			last = string(raw)
			guard.progressed()
		}
		if !guard.observe(false) {
			t.Fatalf("landing did not finish: %+v", rec)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func startLanding(t *testing.T, d *Daemon, p LandParams) (LandingRecord, error) {
	t.Helper()
	response, e := call(t, d, "Land", p, "")
	if e != nil {
		return LandingRecord{}, e
	}
	raw, _ := json.Marshal(response.Result)
	var rec LandingRecord
	e = json.Unmarshal(raw, &rec)
	return rec, e
}

func TestDaemonLandGatesAdvanceOperatorBranchAndSalvageRetires(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	// Tasks fork from the integration tip: the salvaged task changes its own
	// files, so land-one's landing never contains it (FAE-VCS-001/-007).
	base := independentTask(t, cfg, "salvage-one")
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	defer func() { stop(t, d) }()
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }
	authorize(t, d)
	doc := defaults.Workflow()
	for id, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && stage.TestRun {
			stage.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[id] = stage
		}
	}
	if _, e = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	// FAE-CTL-030: the pinned target declares the landing's flakes and frozen
	// checks; a landing request cannot.
	target := TargetConfig{SchemaVersion: 4, Workspace: cfg.Workspace, IntegrationRef: cfg.Live.IntegrationRef, ProtectedBranches: []string{cfg.Live.IntegrationRef}, AllowedCommands: [][]string{{"gofmt", "-l", "."}}, Landing: &LandingPolicy{Flakes: []string{"TestNeverRuns"}, Frozen: [][]string{{"gofmt", "-l", "."}}}}
	for _, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && !slices.ContainsFunc(target.AllowedCommands, func(v []string) bool { return slices.Equal(v, stage.Command) }) {
			target.AllowedCommands = append(target.AllowedCommands, stage.Command)
		}
	}
	if _, e = call(t, d, "TargetConfigPut", TargetPutParams{ID: "land-target", Document: target, AuthorizingMessage: "message"}, ""); e != nil {
		t.Fatal(e)
	}
	for _, id := range []string{"land-one", "salvage-one"} {
		if _, e = call(t, d, "Admit", AdmitParams{ID: id, TargetConfig: "land-target", AuthorizingMessage: "message"}, id); e != nil {
			t.Fatal(e)
		}
	}
	awaitState(t, d, "land-one", "Done")
	awaitState(t, d, "salvage-one", "Done")
	branch := liveGit(t, cfg.Workspace, "symbolic-ref", "--short", "HEAD")
	if _, e = startLanding(t, d, LandParams{Tasks: []string{"land-one"}, Target: branch, AuthorizingMessage: "missing"}); e == nil {
		t.Fatal("unauthorized landing accepted")
	}
	if _, e = startLanding(t, d, LandParams{Tasks: []string{"land-one"}, Target: cfg.Live.IntegrationRef, AuthorizingMessage: "message"}); e == nil {
		t.Fatal("landing onto the instance integration branch accepted")
	}
	for _, p := range []LandParams{{Tasks: []string{"land-one"}, Target: branch, Frozen: [][]string{{"gofmt", "-l", "."}}, AuthorizingMessage: "message"}, {Tasks: []string{"land-one"}, Target: branch, Flakes: []string{"TestBehavior"}, AuthorizingMessage: "message"}} {
		if _, e = startLanding(t, d, p); e == nil || !strings.Contains(e.Error(), "pinned target") {
			t.Fatal("a landing request widened the target's landing policy", e)
		}
	}
	rec, e := startLanding(t, d, LandParams{Tasks: []string{"land-one"}, Target: branch, AuthorizingMessage: "message"})
	if e != nil || rec.State != "running" {
		t.Fatal(rec, e)
	}
	rec = awaitLanding(t, d, rec.ID)
	if rec.State != "finished" || len(rec.Results) != 1 || rec.Results[0].Outcome != controller.LandLanded {
		t.Fatalf("%+v", rec)
	}
	landed := rec.Results[0].Landed
	if liveGit(t, cfg.Workspace, "rev-parse", "HEAD") != landed || liveGit(t, cfg.Workspace, "rev-parse", "refs/fae/gated/"+branch) != landed || liveGit(t, cfg.Workspace, "rev-parse", landed+"^") != base {
		t.Fatal("operator branch or gated reference not advanced to the gated tip")
	}
	if status := liveGit(t, cfg.Workspace, "status", "--porcelain"); status != "" {
		t.Fatal("operator checkout left dirty", status)
	}
	frozenRan := false
	for _, g := range rec.Results[0].Gates {
		if !g.Passed || g.Applicable && g.Count < 1 {
			t.Fatalf("gate without positive count: %+v", g)
		}
		frozenRan = frozenRan || g.Gate == "frozen" && g.Applicable && g.Count == 1
	}
	if !frozenRan {
		t.Fatal("the target's declared frozen check did not run", rec.Results[0].Gates)
	}
	if e = d.verifyGatedTip(ctx, "refs/fae/gated/"+branch, landed); e != nil {
		t.Fatal("landing recorded no gated proof", e)
	}
	again, e := startLanding(t, d, LandParams{Tasks: []string{"land-one"}, Target: branch, AuthorizingMessage: "message"})
	if e != nil {
		t.Fatal(e)
	}
	if again = awaitLanding(t, d, again.ID); again.Results[0].Outcome != controller.LandNothingOwn {
		t.Fatalf("relanding is nothing of its own, never a success: %+v", again)
	}
	events, e := d.store.Events(ctx, 0, 1000)
	if e != nil {
		t.Fatal(e)
	}
	kinds := map[string]bool{}
	for _, event := range events {
		kinds[event.Kind] = true
	}
	if !kinds["landing-landed"] || !kinds["landing-nothing-own"] || kinds["landing-admission-wait"] {
		t.Fatal("landing events missing", kinds)
	}

	// Salvage: the task passed its gates but is escalated for infrastructure.
	liveGit(t, cfg.Workspace, "branch", "salvage-target", base)
	d.mu.Lock()
	snapshot := d.jobs["salvage-one"]
	var env envelope
	if e = decodeEnvelope(snapshot.Payload, &env); e != nil {
		d.mu.Unlock()
		t.Fatal(e)
	}
	source := ""
	for _, a := range env.Job.Evidence {
		if a.Type == "EngineRun" && evidenceNumber(a.Fields["exit_code"]) == 0 && evidenceNumber(a.Fields["executed_count"]) >= 1 {
			source = a.Tree
		}
	}
	env.Job.History = append(env.Job.History, engine.Event{Kind: "transition", Stage: string(env.Job.State), From: env.Job.State, To: domain.Escalated, Attempt: 1, Cause: "planted"})
	env.Job.State, env.Job.Reason = domain.State("Escalated"), "infrastructure: integration host unavailable"
	snapshot.Payload, _ = json.Marshal(env)
	snapshot.State = "Escalated"
	snapshot.Events = nil
	saved, e := d.store.SaveJob(ctx, controlplane.Audit{Actor: "test", Action: "plant infrastructure escalation", Outcome: "ok"}, snapshot, snapshot.Version)
	if e == nil {
		d.jobs["salvage-one"] = saved
	}
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
	if source == "" {
		t.Fatal("fixture has no passing engine run tree")
	}
	if _, e = startLanding(t, d, LandParams{Tasks: []string{"salvage-one"}, Target: "salvage-target", Salvage: true, SourceRef: base, AuthorizingMessage: "message"}); e == nil || !strings.Contains(e.Error(), "not recorded as passing") {
		t.Fatal("salvage of an unrecorded tree accepted", e)
	}
	rec, e = startLanding(t, d, LandParams{Tasks: []string{"salvage-one"}, Target: "salvage-target", Salvage: true, SourceRef: source, AuthorizingMessage: "message"})
	if e != nil {
		t.Fatal(e)
	}
	rec = awaitLanding(t, d, rec.ID)
	if rec.State != "finished" || rec.Results[0].Outcome != controller.LandLanded || rec.Evidence == "" || rec.Retired != "contained "+rec.Results[0].Landed || rec.Error != "" {
		t.Fatalf("%+v", rec)
	}
	if rec.Results[0].SourceTree != source || liveGit(t, cfg.Workspace, "rev-parse", "salvage-target") != rec.Results[0].Landed {
		t.Fatal("salvage did not land the recorded passing tree")
	}
	d.mu.Lock()
	state := d.jobs["salvage-one"].State
	d.mu.Unlock()
	if state != "Retired" {
		t.Fatal("salvaged task not retired as contained", state)
	}
}
