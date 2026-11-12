package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/hostguard"
)

// FAE-CTL-030 with derived scope: a Done task of an instance with no pinned
// files lands through `fae land` with the scope the resolver derives from the
// task itself. The landing's filter gate runs the RED test its RedReport
// named, and the operator branch advances to the gated tip.
func TestDaemonLandDerivedScopeTask(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	cfg.Live.Files, cfg.Live.TestFile, cfg.Live.TestID, cfg.Live.ExpectedFailure = nil, "", "", ""
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "derived-land", Title: "Make answer return 42", Content: "answer() must return 42", AuthorizingMessage: "message"}, "derived-land"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "derived-land", "Done")
	branch := liveGit(t, cfg.Workspace, "symbolic-ref", "--short", "HEAD")
	rec, err := startLanding(t, d, LandParams{Tasks: []string{"derived-land"}, Target: branch, AuthorizingMessage: "message"})
	if err != nil {
		t.Fatal(err)
	}
	rec = awaitLanding(t, d, rec.ID)
	if rec.State != "finished" || len(rec.Results) != 1 || rec.Results[0].Outcome != controller.LandLanded {
		t.Fatalf("%+v", rec)
	}
	filtered := false
	for _, g := range rec.Results[0].Gates {
		filtered = filtered || g.Gate == "filter" && g.Applicable && g.Passed && g.Count == 1
	}
	if !filtered {
		t.Fatal("the derived RED test did not gate the landing", rec.Results[0].Gates)
	}
	landed := rec.Results[0].Landed
	if liveGit(t, cfg.Workspace, "rev-parse", "HEAD") != landed || liveGit(t, cfg.Workspace, "rev-parse", landed+"^") != base || !strings.Contains(liveGit(t, cfg.Workspace, "show", landed+":answer.go"), "return 42") {
		t.Fatal("operator branch not advanced to the derived task's change")
	}
}
