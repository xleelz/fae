package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/workspace"
)

// FAE-ARC-023 acceptance: a workspace planted as a package of a Brazil build
// workspace pins that identity and selects the build-workspace binding
// provider; live execution refuses rather than creating or syncing one; a
// full scripted drive invokes no build-workspace tool; a removed marker
// refuses the next start.
func TestBuildWorkspaceDetectedPinnedAndNeverSynced(t *testing.T) {
	cfg, ctx := fixture(t)
	root := filepath.Dir(cfg.Workspace)
	ws := filepath.Join(root, "src", "Pkg")
	if err := os.MkdirAll(ws, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packageInfo"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Workspace = ws
	t.Chdir(ws)
	tools, trace := filepath.Join(root, "tools"), filepath.Join(root, "trace")
	os.Mkdir(tools, 0700)
	for _, name := range []string{"brazil", "brazil-build", "brazil-recursive-cmd", "cargo", "pnpm", "npm", "bazel"} {
		os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\necho \"$0 $*\" >> "+trace+"\n"), 0700)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			stop(t, d)
		}
	}()
	raw, err := os.ReadFile(d.layout.Binding)
	if err != nil {
		t.Fatal(err)
	}
	var binding struct {
		Workspace      string                    `json:"workspace"`
		BuildWorkspace *workspace.BuildWorkspace `json:"build_workspace"`
	}
	if err = json.Unmarshal(raw, &binding); err != nil || binding.BuildWorkspace == nil || binding.BuildWorkspace.Kind != "brazil" || binding.BuildWorkspace.Provider != workspace.ProviderBuildBinding || binding.BuildWorkspace.Root != root {
		t.Fatalf("binding %s %v", raw, err)
	}
	if err = d.liveBuildWorkspace(); err == nil || !strings.Contains(err.Error(), "pre-provisioned build-workspace-binding") {
		t.Fatal("live execution in a build-workspace package admitted:", err)
	}
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "pkg-task", Tree: "tree", AuthorizingMessage: "message"}, "pkg-task"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "pkg-task", "Done")
	stop(t, d)
	stopped = true
	if out, _ := os.ReadFile(trace); len(out) != 0 {
		t.Fatalf("build-workspace tools invoked during the drive:\n%s", out)
	}
	// A member-preserving edit only refreshes the digest; removing the marker
	// changes the identity and refuses the start.
	if err = os.WriteFile(filepath.Join(root, "packageInfo"), []byte("{\"changed\":true}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	stop(t, d)
	if err = os.Remove(filepath.Join(root, "packageInfo")); err != nil {
		t.Fatal(err)
	}
	if d, err = Start(ctx, cfg); err == nil {
		stop(t, d)
		t.Fatal("changed build-workspace identity accepted at start")
	} else if !strings.Contains(err.Error(), "identity changed") {
		t.Fatal(err)
	}
}
