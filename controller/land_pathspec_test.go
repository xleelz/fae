package controller

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// FAE-SEC-005: a task's file names reach git as literal paths in a landing.
// A task adds a Go file named ":(exclude)*.go" with new formatting debt
// beside a formatted one: as a pathspec its name excluded every Go file from
// its own diff, so the format gate examined none of its lines and passed on
// the base. A salvage declaring ":(top)answer.go" or ":/answer.go" took
// answer.go's blob under that name, a file the source tree doesn't have, on
// the base; now the name is absent from the source, so the restriction
// changes nothing.
func TestLandGitTakesTaskFileNamesLiterally(t *testing.T) {
	f := newLandFixture(t)
	weird := ":(exclude)*.go"
	own := f.commitOn(f.base, "pathspec", map[string]string{
		"ok.go": "package land\n\nfunc ok() int { return 1 }\n",
		weird:   "package land\n\nfunc weird( ) int {  return 2 }\n",
	})
	tree := filepath.Join(f.root, "tree")
	f.git("worktree", "add", "--detach", tree, own)
	g := landGates{ctx: context.Background(), r: LandRequest{Workspace: f.repo}, dir: tree, start: f.base, tip: own}
	if r := g.format(); r.Passed || r.Count != 2 || !strings.Contains(r.Reason, "new formatting debt: "+weird+": func weird") {
		t.Errorf("a Go file named %q escaped the format gate: %+v", weird, r)
	}

	source := f.git("rev-parse", f.task()+"^{tree}")
	baseTree := f.git("rev-parse", f.base+"^{tree}")
	for _, name := range []string{":(top)answer.go", ":/answer.go"} {
		restricted, e := restrictTree(context.Background(), f.repo, f.base, source, []string{name})
		if e != nil {
			t.Fatalf("%s: %v", name, e)
		}
		if restricted != baseTree {
			t.Errorf("salvage declaring %q took another file's content: %s", name, f.git("ls-tree", "-r", "--name-only", restricted))
		}
	}
	if restricted, e := restrictTree(context.Background(), f.repo, f.base, source, []string{"answer.go"}); e != nil || restricted == baseTree {
		t.Fatalf("the declared file itself was not taken: %v", e)
	}
}
