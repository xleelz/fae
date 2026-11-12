package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xleelz/fae/commandshim"
)

// FAE-HOST-011, FAE-HOST-020: a role session whose base PATH has a go in a
// GOROOT runs its go test under a GOROOT mirror in the session directory, a
// tree of symlinks; the mirror is removed when the session ends, while the
// session directory, its configuration and its logs are kept.
func TestRoleSessionGoRootMirrorIsRemovedWithTheSession(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil || commandshim.GoRoot(gobin, nil) == "" {
		t.Skip("no go in a GOROOT")
	}
	d, l := directFixture(t, "brokered-shims")
	d.Shims = commandshim.RoleShims{Broker: "/opt/fae/bin/fae", Ledger: "/var/lib/fae-host"}
	d.Environment["PATH"] = filepath.Dir(gobin) + ":/usr/bin:/bin"
	l.Session = "agentic"
	l.Profile.Allowed = []string{"handoff", "shell"}
	l.Profile.Test = true
	out, err := d.Run(context.Background(), l)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(l.Worktree, "shim-evidence"))
	if err != nil {
		t.Fatal(err)
	}
	_, policy, _ := strings.Cut(string(raw), "\n\n")
	var s commandshim.RoleShims
	if err = json.Unmarshal([]byte(policy), &s); err != nil {
		t.Fatal(err, string(raw))
	}
	if s.NestedGo == nil || s.NestedGo.GoRoot != filepath.Join(out.LogDirectory, "goroot") {
		t.Fatalf("the session ran without its GOROOT mirror: %s", raw)
	}
	if _, err = os.Lstat(s.NestedGo.GoRoot); !os.IsNotExist(err) {
		t.Errorf("session GOROOT mirror kept after the session: %v", err)
	}
	for _, kept := range []string{"config.json", "brief.json", "command-shims"} {
		if _, err = os.Lstat(filepath.Join(out.LogDirectory, kept)); err != nil {
			t.Errorf("session %s not kept: %v", kept, err)
		}
	}
}
