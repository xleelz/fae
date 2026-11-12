package controller

import (
	"context"
	"strings"
	"testing"
)

// FAE-CTL-030: a failing listed flake does not explain the suite's exit when
// something else failed the run too. A package's TestMain exits 1 after its
// tests pass (a leak check that TestLeaks trips) beside a listed flake that
// fails in the same package: go test's stream is the same as for the flake
// alone, so on the base the suite gate passed and the task landed. The suite
// is now run again without the listed flake, the TestMain exit shows there,
// and the landing holds at the suite gate with the target untouched. The same
// flake failing alone is tolerated: the run without it passes and the task
// lands.
func TestLandSuiteListedFlakeBesideATestMainExitHolds(t *testing.T) {
	flaky := "package extra\n\nimport \"testing\"\n\nfunc TestFlaky(t *testing.T) { t.Fatal(\"planted flake\") }\n"
	leaky := "package extra\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"testing\"\n)\n\nvar leaked bool\n\nfunc TestMain(m *testing.M) {\n\tcode := m.Run()\n\tif leaked {\n\t\tfmt.Println(\"leak check: TestLeaks left a goroutine\")\n\t\tcode = 1\n\t}\n\tos.Exit(code)\n}\n\nfunc TestLeaks(t *testing.T) { leaked = true }\n"
	for _, c := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"beside a TestMain exit", map[string]string{"extra/flaky_test.go": flaky, "extra/main_test.go": leaky}, LandHeld},
		{"alone", map[string]string{"extra/flaky_test.go": flaky}, LandLanded},
	} {
		f := newLandFixture(t)
		files := map[string]string{"answer.go": "package land\n\nfunc answer() int { return 42 }\n", "answer_test.go": landTest}
		for name, body := range c.files {
			files[name] = body
		}
		own := f.commitOn(f.base, "flake", files)
		r := f.request("flake", own)
		for name := range c.files {
			r.Files = append(r.Files, name)
		}
		r.QA = [][]string{GoFilterCommand("TestBehavior")}
		r.Policy.Flakes = []string{"TestFlaky"}
		start := f.git("rev-parse", "main")
		res, e := Land(context.Background(), r)
		if e != nil || res.Outcome != c.want {
			t.Fatalf("%s: %v %+v", c.name, e, res)
		}
		suite := gate(res, "suite")
		if c.want == LandLanded {
			if !suite.Passed || f.git("rev-parse", "main") != res.Landed {
				t.Fatalf("%s: %+v %+v", c.name, suite, res)
			}
			continue
		}
		if res.FailingGate != "suite" || suite.Passed || !strings.Contains(suite.Reason, "suite exited 1 with only listed flakes failing, but its run without them: package failed with no failed test: example.invalid/land/extra, exited 1") || f.git("rev-parse", "main") != start {
			t.Fatalf("%s: %+v %+v", c.name, suite, res)
		}
	}
}

// FAE-CTL-030: a flake listed for its own package is counted skipped only
// there. Package other fails only its own listed flake, TestOtherFlaky, and
// its TestFlaky passes but trips its TestMain's leak check; TestFlaky is
// listed only for package extra. The suite run again without the listed
// flakes skips TestFlaky in every package (go test's -skip names no
// package), so other's leak never shows and on the base the task landed.
// Now the landing holds at the suite gate, naming other's TestFlaky, with
// the target untouched. A namesake in a package the first run passed is
// shown by that run, and the task lands.
func TestLandSuiteFlakeNamesakeInAFailedPackageHolds(t *testing.T) {
	flaky := "package extra\n\nimport \"testing\"\n\nfunc TestFlaky(t *testing.T) { t.Fatal(\"planted flake\") }\n"
	leaky := "package other\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"testing\"\n)\n\nvar leaked bool\n\nfunc TestMain(m *testing.M) {\n\tcode := m.Run()\n\tif leaked {\n\t\tfmt.Println(\"leak check: TestFlaky left a goroutine\")\n\t\tcode = 1\n\t}\n\tos.Exit(code)\n}\n\nfunc TestFlaky(t *testing.T) { leaked = true }\n\nfunc TestOtherFlaky(t *testing.T) { t.Fatal(\"planted flake\") }\n"
	namesake := "package other\n\nimport \"testing\"\n\nfunc TestFlaky(t *testing.T) {}\n"
	const extra, other = "example.invalid/land/extra", "example.invalid/land/other"
	for _, c := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"in a failed package", map[string]string{"extra/flaky_test.go": flaky, "other/other_test.go": leaky}, LandHeld},
		{"in a passing package", map[string]string{"extra/flaky_test.go": flaky, "other/other_test.go": namesake}, LandLanded},
	} {
		f := newLandFixture(t)
		files := map[string]string{"answer.go": "package land\n\nfunc answer() int { return 42 }\n", "answer_test.go": landTest}
		for name, body := range c.files {
			files[name] = body
		}
		own := f.commitOn(f.base, "namesake", files)
		r := f.request("namesake", own)
		for name := range c.files {
			r.Files = append(r.Files, name)
		}
		r.QA = [][]string{GoFilterCommand("TestBehavior")}
		r.Policy.Flakes = []string{extra + ":TestFlaky", other + ":TestOtherFlaky"}
		start := f.git("rev-parse", "main")
		res, e := Land(context.Background(), r)
		if e != nil || res.Outcome != c.want {
			t.Fatalf("%s: %v %+v", c.name, e, res)
		}
		suite := gate(res, "suite")
		if c.want == LandLanded {
			if !suite.Passed || f.git("rev-parse", "main") != res.Landed {
				t.Fatalf("%s: %+v %+v", c.name, suite, res)
			}
			continue
		}
		if res.FailingGate != "suite" || suite.Passed || !strings.Contains(suite.Reason, "skipped 1 test(s) that are not listed flakes in their package, in a package the suite failed or did not finish: "+other+":TestFlaky") || f.git("rev-parse", "main") != start {
			t.Fatalf("%s: %+v %+v", c.name, suite, res)
		}
	}
}
