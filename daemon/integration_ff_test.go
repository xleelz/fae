package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
)

// FAE-VCS-011: the workspace's integration reference fast-forwards to the
// deployed tip only from an ancestor; a diverged reference stays unchanged
// and the rejection is a durable alert.
func TestDeployedTipFastForwardOrAlert(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	repo := t.TempDir()
	liveGit(t, repo, "init", "-q")
	commit := func(name string) string {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
		liveGit(t, repo, "add", name)
		liveGit(t, repo, "commit", "-qm", name)
		return liveGit(t, repo, "rev-parse", "HEAD")
	}
	a := commit("a")
	b := commit("b")
	liveGit(t, repo, "checkout", "-q", "-b", "side", a)
	c := commit("c")
	ref := "refs/heads/fae/integration"
	liveGit(t, repo, "update-ref", ref, a)
	var recorded []string
	record := func(kind string, m *controller.Marker, alert bool) error {
		recorded = append(recorded, kind)
		body, _ := json.Marshal(m)
		return d.store.ControllerAction(context.Background(), controlplane.Audit{Actor: "daemon", Action: kind, Outcome: "rejected"}, body, alert)
	}
	if err = fastForwardDeployed(ctx, repo, &controller.Marker{IntegrationRef: ref, DeployedTip: b}, record); err != nil || liveGit(t, repo, "rev-parse", ref) != b || len(recorded) != 0 {
		t.Fatal("ancestor reference not fast-forwarded", err, recorded)
	}
	m := &controller.Marker{IntegrationRef: ref, DeployedTip: c}
	if err = fastForwardDeployed(ctx, repo, m, record); err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatal("diverged reference accepted", err)
	}
	if liveGit(t, repo, "rev-parse", ref) != b || m.State != "failed" || len(recorded) != 1 || recorded[0] != "controller-maintenance-deployment-rejected" {
		t.Fatal("diverged reference moved or rejection unrecorded", recorded, m.State)
	}
	alerts, err := d.store.Alerts(context.Background(), 0, 100, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range alerts {
		found = found || a.Source == "controller" && a.Severity == "error"
	}
	if !found {
		t.Fatal("non-fast-forward raised no alert", alerts)
	}
}
