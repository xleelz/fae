package daemon

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/workflow"
	"strings"
	"testing"
	"time"
)

// Runs the production multiplexer, durable admission and host/VCS effect paths;
// the provider subprocess is explicitly a protocol fixture, not dogfood.
func TestDaemonMultiplexerEffectsLifecycle(t *testing.T) {
	cfg, ctx, base := liveDaemonFixture(t)
	herdr := testHerdr(t)
	cfg.Live.Adapter = "multiplexer"
	cfg.Live.HerdrBinary = herdr
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := defaults.Workflow()
	for id, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && stage.TestRun {
			stage.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[id] = stage
		}
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "live-one", AuthorizingMessage: "message"}, "live-one"); err != nil {
		t.Fatal(err)
	}
	snapshot := awaitMultiplexerState(t, d, "live-one", "Done")
	var e envelope
	if err = json.Unmarshal(snapshot.Payload, &e); err != nil {
		t.Fatal(err)
	}
	if e.Job.ExecutionKind != "live" || len(e.Job.Provenance) < 20 {
		t.Fatal("missing live chain")
	}
	for _, p := range e.Job.Provenance {
		if p.ExecutionKind != "live" {
			t.Fatal("mixed scripted evidence")
		}
	}
	tip := liveGit(t, cfg.Workspace, "rev-parse", cfg.Live.IntegrationRef)
	if tip == base {
		t.Fatal("integration never advanced")
	}
	if got := liveGit(t, cfg.Workspace, "show", tip+":answer.go"); !strings.Contains(got, "return 42") {
		t.Fatal("real implementation absent", got)
	}
	if got := liveGit(t, cfg.Workspace, "rev-parse", "HEAD"); got != base {
		t.Fatal("primary head changed")
	}
	if got := liveGit(t, cfg.Workspace, "status", "--porcelain"); got != "" {
		t.Fatal("primary checkout modified", got)
	}
	panes, paneErr := d.live.guard.panes.Snapshot(ctx)
	if paneErr != nil || len(panes) != 0 {
		t.Fatal("leaked pane reservations", panes, paneErr)
	}
	pool, err := d.live.guard.host.Snapshot(ctx)
	if err != nil || len(pool.Holders) != 0 || len(pool.Waiting) != 0 {
		t.Fatal("leaked host leases", pool, err)
	}
	role, err := d.store.LeaseSnapshot(ctx, fmt.Sprintf("role/%x", sha256.Sum256([]byte("default\x00global\x00spec"))))
	if err != nil || len(role.Holders) != 0 || len(role.Waiting) != 0 {
		t.Fatal("leaked role leases", role, err)
	}
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	again := awaitMultiplexerState(t, d, "live-one", "Done")
	if again.Version != snapshot.Version {
		t.Fatal("completed live job replayed")
	}
}

// FAE-ROB-011/-012: a graceful stop lists the interrupted Herdr turn. Close
// joins its retirement, so restart deterministically replays it without charge.
func TestDaemonMultiplexerPlannedStopNeverChargesInterruptedTurn(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	herdr := testHerdr(t)
	cfg.Live.Adapter = "multiplexer"
	cfg.Live.HerdrBinary = herdr
	cfg.Live.Environment["LIVE_DAEMON_WAIT"] = "1"
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	doc := defaults.Workflow()
	for id, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && stage.TestRun {
			stage.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[id] = stage
		}
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{ID: "interrupted", AuthorizingMessage: "message"}, "interrupted"); err != nil {
		t.Fatal(err)
	}
	awaitLaunchedTurn(t, d)
	stop(t, d)
	d, err = Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := d.store.GetJob(ctx, "interrupted")
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err = json.Unmarshal(snapshot.Payload, &e); err != nil {
		t.Fatal(err)
	}
	assertPlannedReplay(t, e.Job)
}

func awaitMultiplexerState(t *testing.T, d *Daemon, id, state string) controlplane.JobSnapshot {
	t.Helper()
	return awaitThroughDeferred(t, d, id, state, 120*time.Second)
}
