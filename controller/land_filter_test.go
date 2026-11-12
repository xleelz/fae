package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAE-CTL-030, FAE-CLI-014: a landing hands the task's required test filter
// to go test -run as the regular expression admission accepted, and counts
// the top-level tests it matches, so an anchored name lands. A derived-scope
// RED identity, quoted and anchored (ExactTestFilter), lands the same way. On
// the base the filter was quoted again and compared as one literal name, so
// `^TestBehavior$` matched zero tests and the landing was held.
func TestLandFilterIsARegularExpression(t *testing.T) {
	f := newLandFixture(t)
	own := f.task()
	f.git("checkout", "--detach")
	for i, filter := range []string{"^TestBehavior$", "TestBehavior|TestNeither"} {
		r := f.request("pattern"+string(rune('0'+i)), own)
		r.Filter = filter
		res, e := Land(context.Background(), r)
		if e != nil || res.Outcome != LandLanded {
			t.Fatalf("%q: %v %+v", filter, e, res)
		}
		if g := gate(res, "filter"); !g.Passed || g.Count != 1 {
			t.Fatalf("%q: %+v", filter, g)
		}
		if f.git("rev-parse", "main") != res.Landed {
			t.Fatal("target not advanced")
		}
		f.git("update-ref", "refs/heads/main", f.base)
		f.git("update-ref", "-d", "refs/fae/gated/main")
	}
	// A matched test that skips is counted in Matched (and not MatchedPassed)
	// even when another matched test passes, holding the filter gate.
	withSkip := f.commitOn(f.base, "skip", map[string]string{
		"answer.go":      "package land\n\nfunc answer() int { return 42 }\n",
		"answer_test.go": landTest + "\nfunc TestBehaviorSkipped(t *testing.T) { t.Skip(\"skipped\") }\n",
	})
	r := f.request("skip-held", withSkip)
	r.Filter = "TestBehavior$|TestBehaviorSkipped"
	res, e := Land(context.Background(), r)
	if e != nil || res.Outcome != LandHeld || res.FailingGate != "filter" || !strings.Contains(res.Reason, "failed 1 of 2") {
		t.Fatalf("skipped match must hold landing: %v %+v", e, res)
	}
}

// FAE-CTL-030: the format gate examines a Go file whose type changed, such
// as a symlink replaced by a Go source file, as it examines an added one,
// so new formatting debt there holds the landing; and the own-hunk gate
// checks the type change's two sections (the symlink's deletion and the
// file's addition) as one unit, so a formatted type change lands. On the
// base the format gate's `--diff-filter=AM` left type changes out, and the
// own-hunk gate reverse-applied the deletion half alone, which never fits
// the landed tree: every type change was held as "own hunk missing".
func TestLandFormatGateExaminesATypeChange(t *testing.T) {
	f := newLandFixture(t)
	if e := os.Symlink("answer.go", filepath.Join(f.repo, "link.go")); e != nil {
		t.Fatal(e)
	}
	f.git("add", "link.go")
	f.git("commit", "-m", "link")
	f.base = f.git("rev-parse", "HEAD")
	f.git("checkout", "--detach")
	files := func(link string) map[string]string {
		return map[string]string{"answer.go": "package land\n\nfunc answer() int { return 42 }\n", "answer_test.go": landTest, "link.go": link}
	}
	debt := f.commitOn(f.base, "debt", files("package land\n\nfunc linked( ) int { return 1 }\n"))
	r := f.request("typechange", debt)
	r.Files = append(r.Files, "link.go")
	res, e := Land(context.Background(), r)
	if e != nil || res.Outcome != LandHeld || res.FailingGate != "format" || !strings.Contains(res.Reason, "link.go: func linked") {
		t.Fatalf("%v %+v", e, res)
	}
	if g := gate(res, "own-hunk"); !g.Passed || g.Count != 4 {
		t.Fatalf("the type change's sections as one unit: %+v", g)
	}
	if g := gate(res, "format"); g.Count < 2 || len(g.Failures) != 1 {
		t.Fatalf("the replaced symlink's items and the modified file's: %+v", g)
	}
	if f.git("rev-parse", "main") != f.base {
		t.Fatal("held landing moved the target")
	}
	formatted := f.commitOn(f.base, "formatted", files("package land\n\nfunc linked() int { return 1 }\n"))
	r = f.request("typechange-formatted", formatted)
	r.Files = append(r.Files, "link.go")
	if res, e = Land(context.Background(), r); e != nil || res.Outcome != LandLanded || f.git("rev-parse", "main") != res.Landed {
		t.Fatalf("%v %+v", e, res)
	}
	if out := f.git("ls-tree", res.Landed, "--", "link.go"); !strings.HasPrefix(out, "100644 blob") {
		t.Fatalf("the landed entry: %s", out)
	}
}
