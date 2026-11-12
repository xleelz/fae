package controller

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// landScratchFiles reads every regular file a landing left in its scratch.
func landScratchFiles(t *testing.T, scratch string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(scratch, func(path string, entry os.DirEntry, err error) error {
		if err != nil || !entry.Type().IsRegular() {
			return err
		}
		b, err := os.ReadFile(path)
		files[path] = b
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// FAE-SEC-003: a gate command writes only to unnamed files, and the landing
// scratch keeps their copies through RedactOutput, one per stdout and stderr
// of each gate command. The gate results are parsed from the copy: a failing
// subtest named after a planted secret is held under its redacted name, and
// no kept file carries the secret. When RedactOutput fails (the secrets could
// not be read), nothing is kept and the landing stops as infrastructure, not
// as a failing gate, with RedactOutput's reason; the attempt is kept on the
// held branch and the target is untouched.
func TestLandKeepsOnlyRedactedGateOutput(t *testing.T) {
	f := newLandFixture(t)
	const planted = "plantedLandingSecret0123456789"
	plantedTest := "package land\n\nimport \"testing\"\n\nfunc TestPlanted(t *testing.T) {\n\tt.Log(\"printed " + planted + "\")\n\tt.Run(\"" + planted + "\", func(t *testing.T) { t.Fatal(\"planted failure\") })\n}\n"
	own := f.commitOn(f.base, "planted", map[string]string{"answer.go": "package land\n\nfunc answer() int { return 42 }\n", "answer_test.go": landTest, "planted_test.go": plantedTest})
	r := f.request("redacted", own)
	r.Files = append(r.Files, "planted_test.go")
	r.QA = [][]string{GoFilterCommand("TestBehavior")}
	copies := 0
	r.RedactOutput = func(dst io.Writer, src io.Reader) error {
		copies++
		raw, e := io.ReadAll(src)
		if e != nil {
			return e
		}
		_, e = dst.Write(bytes.ReplaceAll(raw, []byte(planted), []byte("[redacted]")))
		return e
	}
	res, e := Land(context.Background(), r)
	if e != nil || res.Outcome != LandHeld || res.FailingGate != "suite" || !strings.Contains(res.Reason, "TestPlanted/[redacted]") || strings.Contains(res.Reason, planted) {
		t.Fatalf("%v %+v", e, res)
	}
	outputs, shown := 0, false
	for path, b := range landScratchFiles(t, r.Scratch) {
		name := filepath.Base(path)
		if bytes.Contains(b, []byte(planted)) || strings.HasPrefix(name, "gate-capture-") {
			t.Errorf("the landing scratch keeps %s:\n%s", path, b)
		}
		if strings.HasPrefix(name, "gate-output-") {
			outputs++
			shown = shown || bytes.Contains(b, []byte("printed [redacted]"))
		}
	}
	if outputs == 0 || copies != 2*outputs || !shown {
		t.Fatalf("%d copies for %d kept gate outputs; redaction shown: %v", copies, outputs, shown)
	}

	r = f.request("withheld", own)
	r.RedactOutput = func(io.Writer, io.Reader) error { return errors.New("planted: the secrets could not be read") }
	start := f.git("rev-parse", "main")
	res, e = Land(context.Background(), r)
	if e != nil || res.Outcome != LandInfrastructure || res.FailingGate != "" || res.Reason != "planted: the secrets could not be read" || res.Held != r.Held || f.git("rev-parse", r.Held) != res.Tip || f.git("rev-parse", "main") != start {
		t.Fatalf("%v %+v", e, res)
	}
	if last := res.Gates[len(res.Gates)-1]; last.Gate != "compile" || last.Passed || !strings.Contains(last.Reason, "planted: the secrets could not be read") {
		t.Fatalf("the gate the output was withheld at: %+v", last)
	}
	for path, b := range landScratchFiles(t, r.Scratch) {
		if len(b) != 0 || strings.HasPrefix(filepath.Base(path), "gate-capture-") {
			t.Errorf("a withheld gate output was kept: %s %q", path, b)
		}
	}
}

// FAE-CTL-030: a redaction cannot turn a failure into a pass. Two packages
// whose import paths differ only inside planted secrets become one name in
// the redacted copy; the first fails and the second, under the same test
// name, passes (in wave 9, the suite took the last result and the task
// landed). The landing stops at the first go test gate, compile: the raw
// capture, parsed in memory, has one more package than the copy, and the
// reason gives the counts but not the secrets. Nothing about the change
// failed, so it stops as infrastructure, as when the secrets cannot be read,
// keeping the attempt on its held branch (on wave 11's base it held as a
// failing compile gate). Two passing packages merged the same way stop too,
// and a secret that only renames a passing package (counts unchanged) lands.
func TestLandRedactionCannotMergeAFailingPackageIntoAPass(t *testing.T) {
	const one, two = "plantedsecretalpha", "plantedsecretbeta"
	redact := func(dst io.Writer, src io.Reader) error {
		raw, e := io.ReadAll(src)
		if e != nil {
			return e
		}
		_, e = dst.Write(bytes.ReplaceAll(bytes.ReplaceAll(raw, []byte(one), []byte("[redacted]")), []byte(two), []byte("[redacted]")))
		return e
	}
	pkgTest := func(name, outcome string) string {
		body := ""
		if outcome == "fail" {
			body = "\tt.Fatal(\"planted package failure\")\n"
		}
		return "package " + name + "\n\nimport \"testing\"\n\nfunc TestShared(t *testing.T) {\n" + body + "}\n"
	}
	for _, c := range []struct{ name, first, second string }{{"failing", "fail", "pass"}, {"passing", "pass", "pass"}} {
		f := newLandFixture(t)
		own := f.commitOn(f.base, "merged-"+c.name, map[string]string{
			"answer.go": "package land\n\nfunc answer() int { return 42 }\n", "answer_test.go": landTest,
			"x" + one + "/a_test.go": pkgTest("x"+one, c.first), "x" + two + "/a_test.go": pkgTest("x"+two, c.second),
		})
		r := f.request("merged-"+c.name, own)
		r.Files = append(r.Files, "x"+one+"/a_test.go", "x"+two+"/a_test.go")
		r.QA = [][]string{GoFilterCommand("TestBehavior")}
		r.RedactOutput = redact
		start := f.git("rev-parse", "main")
		res, e := Land(context.Background(), r)
		if e != nil || res.Outcome != LandInfrastructure || res.FailingGate != "" || !strings.Contains(res.Reason, "redaction changed the go test results (0 executed, 0 matched, 0 matched passing, 3 packages") || !strings.Contains(res.Reason, "2 packages, 0 failed of which 0 listed flakes, 0 build failures redacted") || strings.Contains(res.Reason, one) || strings.Contains(res.Reason, two) || res.Held != r.Held || f.git("rev-parse", r.Held) != res.Tip || f.git("rev-parse", "main") != start {
			t.Fatalf("%s: %v %+v", c.name, e, res)
		}
		if last := res.Gates[len(res.Gates)-1]; last.Gate != "compile" || last.Passed || !strings.Contains(last.Reason, "redaction changed the go test results") {
			t.Fatalf("%s: the gate the redaction changed: %+v", c.name, last)
		}
	}
	f := newLandFixture(t)
	own := f.commitOn(f.base, "renamed", map[string]string{
		"answer.go": "package land\n\nfunc answer() int { return 42 }\n", "answer_test.go": landTest,
		"x" + one + "/a_test.go": pkgTest("x"+one, "pass"),
	})
	r := f.request("renamed", own)
	r.Files = append(r.Files, "x"+one+"/a_test.go")
	r.RedactOutput = redact
	if res, e := Land(context.Background(), r); e != nil || res.Outcome != LandLanded {
		t.Fatalf("a renamed passing package: %v %+v", e, res)
	}
	// A redaction that leaves the copy unparseable, where the raw stream
	// parses, stops the landing as infrastructure too.
	f = newLandFixture(t)
	r = f.request("unparseable", f.task())
	r.RedactOutput = func(dst io.Writer, src io.Reader) error {
		raw, e := io.ReadAll(src)
		if e != nil {
			return e
		}
		_, e = dst.Write(bytes.ReplaceAll(raw, []byte(`"Action"`), []byte(`[redacted]`)))
		return e
	}
	if res, e := Land(context.Background(), r); e != nil || res.Outcome != LandInfrastructure || !strings.Contains(res.Reason, "redaction changed the go test results: the redacted copy does not parse") {
		t.Fatalf("an unparseable redacted copy: %v %+v", e, res)
	}
}
