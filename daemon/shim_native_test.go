package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// FAE-MODE-002, FAE-HOST-011/-020, FAE-VER-002: a live HostCommand reaches the
// command broker's shim policy through the daemon drive. A VCS vector in a Local
// task and a build output under a RAM-backed root (the fixture worktree lives
// under /var/tmp) are refused before anything runs; each refusal is recorded on
// the task, charges nothing (FAE-RT-004), and the repeated refusal escalates
// SameGate to the operator instead of reporting success.
func TestLiveHostCommandShimPolicyRefusals(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	vectors := map[string]struct {
		argv   []string
		reason string
	}{
		"vcs": {[]string{"git", "status"}, "VCS commands in role command paths are disabled in Local mode; use the dedicated VCS broker"},
		"ram": {[]string{"go", "build", "-o", "shim-output", "."}, "RAM-backed temporary build output: "},
	}
	for _, v := range vectors {
		cfg.Live.AllowedCommands = append(cfg.Live.AllowedCommands, v.argv)
	}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	authorize(t, d)
	for id, v := range vectors {
		doc := defaults.Workflow()
		doc.ID = "shim-" + id
		s := doc.Stages["format"]
		s.Command = v.argv
		doc.Stages["format"] = s
		// The content declares the command vectors it needs (FAE-WF-001).
		req := *doc.TargetRequirements
		req.Commands = [][]string{defaults.TestCommand(), v.argv}
		doc.TargetRequirements = &req
		if issues := workflow.Validate(doc); len(issues) != 0 {
			t.Fatal(issues)
		}
		if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
			t.Fatal(err)
		}
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, RunID: id, WorkflowID: doc.ID, AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(id, err)
		}
	}
	for id, v := range vectors {
		_, e := awaitJob(t, d, id, 3*time.Minute, func(_ controlplane.JobSnapshot, e envelope) bool {
			return e.Job.State == "Escalated" || e.Job.Parked && !deferredPark(e) || e.Job.State.Terminal()
		})
		j := e.Job
		if !j.Escalated("SameGate") || j.Counts["format"] != 0 || !strings.Contains(j.EscalationDetail, "refused before launch each time, charging no attempt") || !strings.Contains(j.EscalationDetail, v.reason) {
			t.Fatalf("%s: refused host command not escalated SameGate at format: state=%s reason=%s kind=%s %q visits=%d", id, j.State, j.Reason, j.EscalationKind, j.EscalationDetail, j.Counts["format"])
		}
		// Each refusal is recorded before the next try; a refusal is a
		// non-execution, so nothing parks uncertain (a deferral for host
		// pressure parks before any run is asked for).
		refusals := 0
		for _, h := range j.History {
			if h.Kind == "failure" && h.Stage == "format" && strings.HasPrefix(h.Cause, "RefusedBeforeLaunch: refused before exec: ") && strings.Contains(h.Cause, v.reason) {
				refusals++
			}
			if h.Kind == "park" && h.Cause != "infrastructure-deferred" {
				t.Fatalf("%s: a refused host command parked: %+v", id, h)
			}
		}
		if refusals != engine.SameGateRepeats+1 {
			t.Fatalf("%s: %d shim refusals %q recorded, want %d: %+v", id, refusals, v.reason, engine.SameGateRepeats+1, j.History)
		}
		for _, p := range e.Job.Provenance {
			if p.Stage == "format" {
				t.Fatalf("%s: refused host command produced evidence %+v", id, p)
			}
		}
	}
	// Nothing ran: no build output exists anywhere under the fixture root.
	_ = filepath.WalkDir(filepath.Dir(cfg.Root), func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.Name() == "shim-output" {
			t.Errorf("refused build wrote its output: %s", path)
		}
		return nil
	})
}
