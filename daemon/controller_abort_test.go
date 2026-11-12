package daemon

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
)

// FAE-CTL-001 fast-forward-rejected abort: post-start acceptance refuses a
// deployed tip the integration reference cannot fast-forward to, marks the
// maintenance failed with the next action, and raises an alert.
func TestRollAcceptRejectsDivergedFastForward(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", cfg.Workspace}, args...)...)
		cmd.Env = append(cleanGitEnv(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@localhost", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@localhost")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(args, err)
		}
		return strings.TrimSpace(string(out))
	}
	if exec.Command("git", "-C", cfg.Workspace, "rev-parse", "--git-dir").Run() != nil {
		git("init", "-q")
	}
	empty := git("hash-object", "-t", "tree", "-w", "--stdin")
	integrated := git("commit-tree", empty, "-m", "integrated")
	diverged := git("commit-tree", empty, "-m", "diverged")
	ref := "refs/heads/fae/abort-integration"
	git("update-ref", ref, integrated)
	deadline := time.Now().Add(30 * time.Second)
	for d.controllerHealth().DriveRounds == 0 && time.Now().Before(deadline) {
		d.signal()
		time.Sleep(20 * time.Millisecond)
	}
	h := d.controllerHealth()
	marker := controller.Marker{ID: "ff", Message: "message", State: "stopped", OldPID: 1, OldBirth: "replaced", Launch: controller.LaunchSpec{Binary: "/old/fae", SHA256: "old-build"},
		Replacement: &controller.LaunchSpec{Binary: d.binaryPath, SHA256: d.binaryHash, EnvironmentDigest: h.EnvironmentHash}, DeployedTip: diverged, IntegrationRef: ref}
	raw, _ := json.Marshal(marker)
	// The plane marker is the gate's authority (FAE-GEN-011); the mirror
	// follows it, as a committed prepare leaves them.
	if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "model replacement identity", Outcome: "ok"}, "controller-maintenance", raw); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(d.controllerMarkerPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	d.mu.Lock()
	err = d.adoptMaintenance(raw)
	d.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	_, err = controllerCall(t, d, controller.Params{Action: "accept", AuthorizingMessage: "message", Marker: &controller.Marker{ID: "ff"}})
	if err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatal("diverged deployed tip accepted", err)
	}
	after, err := controller.ReadMarker(d.controllerMarkerPath())
	if err != nil || after == nil || after.State != "failed" || !strings.Contains(after.NextAction, "reconcile divergent integration reference") {
		t.Fatalf("marker after rejected fast-forward: %+v %v", after, err)
	}
	if git("rev-parse", ref) != integrated {
		t.Fatal("rejected fast-forward moved the integration reference")
	}
	r, err := call(t, d, "Alerts", AlertsParams{}, "")
	if err != nil || !strings.Contains(string(r.Result), `"severity":"error","source":"controller"`) || !strings.Contains(string(r.Result), "reconcile divergent integration reference") {
		t.Fatalf("no alert for the rejected fast-forward: %s %v", r.Result, err)
	}
	_ = os.Remove(d.controllerMarkerPath())
}
