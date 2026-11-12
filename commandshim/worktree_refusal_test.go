package commandshim

import (
	"os"
	"strings"
	"testing"
)

// FAE-VCS-002: no role session can create, remove or prune a worktree. Each
// worktree-management form, including the help variants that once performed
// a removal, is refused through a role shell before git runs; an absolute
// path that would bypass the shim is denied by the pre-tool hook.
func TestRoleShellRefusesWorktreeManagement(t *testing.T) {
	f := roleShims(t, RoleShims{Build: true, Test: true, Format: true, Local: true})
	for _, line := range []string{
		"git worktree add ../other",
		"git worktree add -b x ../other HEAD",
		"git worktree remove .",
		"git worktree remove --force .",
		"git worktree prune",
		"git worktree prune --expire now",
		"git worktree --help",
		"git worktree add --help",
		"git worktree remove --help",
		"git worktree prune --help",
		"git worktree remove -h",
		"git help worktree",
		"git -C . worktree remove .",
		"git -c core.hooksPath=/dev/null worktree prune",
	} {
		code, out := f.run(t, f.worktree, nil, "/bin/sh", "-c", line)
		if code != Refused || !strings.Contains(out, "VCS broker") {
			t.Fatalf("%q: exit %d %s", line, code, out)
		}
		if _, deny := BypassesShim("/usr/bin/"+line, f.shims); !deny {
			t.Fatalf("%q: absolute-path bypass not denied", line)
		}
	}
	if entries, _ := os.ReadDir(f.out); len(entries) != 0 {
		t.Fatal("a refused worktree command reached git")
	}
	if got := f.audit(t); len(got) != 0 {
		t.Fatal("refusal touched the lease", got)
	}
}
