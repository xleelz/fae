package effects

import (
	"context"
	"github.com/xleelz/fae/commandshim"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRustSelectedNative(t *testing.T) {
	if _, e := exec.LookPath("cargo"); e != nil {
		t.Skip(e)
	}
	d := t.TempDir()
	if e := os.MkdirAll(filepath.Join(d, "src"), 0700); e != nil {
		t.Fatal(e)
	}
	for n, s := range map[string]string{"Cargo.toml": "[package]\nname=\"fixture\"\nversion=\"0.1.0\"\nedition=\"2021\"\n", "Cargo.lock": "version = 3\n\n[[package]]\nname = \"fixture\"\nversion = \"0.1.0\"\n", "src/lib.rs": "#[cfg(test)] mod tests { #[test] fn selected(){panic!(\"expected defect\");} #[test] fn other(){panic!(\"must not execute\");} }"} {
		if e := os.WriteFile(filepath.Join(d, n), []byte(s), 0600); e != nil {
			t.Fatal(e)
		}
	}
	a, e := rustShapeVector([]string{"cargo", "test", "--lib"}, "tests::selected")
	if e != nil {
		t.Fatal(e)
	}
	a, e = (commandshim.Policy{Worktree: d, CWD: d, CodeRole: true, LeaseHeld: true}).Prepare(a)
	if e != nil {
		t.Fatal(e)
	}
	env, e := commandshim.RustEnvironment(a, os.Environ(), t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command(a[0], a[1:]...)
	cmd.Dir = d
	cmd.Env = env
	b, e := cmd.Output()
	r := RunEvidence{Output: string(b), ExitCode: 0}
	if e != nil {
		r.ExitCode = 1
	}
	if e = parseRustTests(&r, Command{TestID: "tests::selected", ExpectedFailure: "expected defect"}); e != nil {
		t.Fatalf("%s: %v", b, e)
	}
	if r.MatchedCount != 1 || r.ExecutedCount != 1 || !r.ExpectedFailure || !r.FreshBuild {
		t.Fatalf("%+v", r)
	}
}
func TestRustEvidenceRefusesNoExecution(t *testing.T) {
	for _, s := range []string{"error: compilation failed expected defect", "running 0 tests\n\ntest result: ok. 0 passed; 0 failed; 0 ignored; 0 measured; 1 filtered out; finished in 0.00s\n", "running 1 test\ntest tests::selected ... ignored\ntest result: ok. 0 passed; 0 failed; 1 ignored; 0 measured; 0 filtered out; finished in 0.00s\n"} {
		r := RunEvidence{Output: s, ExitCode: 1}
		if e := parseRustTests(&r, Command{TestID: "tests::selected", ExpectedFailure: "expected defect"}); e == nil || r.ExpectedFailure {
			t.Fatalf("accepted %q: %+v", s, r)
		}
	}
}

func TestRustBrokerNativeSelected(t *testing.T) {
	if _, e := exec.LookPath("cargo"); e != nil {
		t.Skip(e)
	}
	b, w, _ := setup(t)
	if e := os.MkdirAll(filepath.Join(w.Path, "src"), 0700); e != nil {
		t.Fatal(e)
	}
	write(t, w.Path, "Cargo.toml", "[package]\nname=\"broker_fixture\"\nversion=\"0.1.0\"\nedition=\"2021\"\n")
	write(t, w.Path, "Cargo.lock", "version = 3\n\n[[package]]\nname = \"broker_fixture\"\nversion = \"0.1.0\"\n")
	write(t, w.Path, "src/lib.rs", "#[cfg(test)] mod tests { #[test] fn selected(){assert!(true);} #[test] fn other(){panic!(\"unselected\");} }")
	b.Allowed = [][]string{{"cargo", "test", "--lib"}}
	r, e := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Shape: "rust-unit", TestID: "tests::selected", Timeout: 30 * time.Second})
	if e != nil || r.ExitCode != 0 || r.ExecutedCount != 1 || r.MatchedCount != 1 || !r.RedPassed || !r.FreshBuild {
		t.Fatalf("%+v %v", r, e)
	}
	for _, verb := range []string{"build", "check"} {
		b.Allowed = [][]string{{"cargo", verb}}
		got, err := b.Run(context.Background(), w, Command{Argv: b.Allowed[0], Timeout: 30 * time.Second})
		if err != nil || got.ExitCode != 0 {
			t.Fatalf("%s: %+v %v", verb, got, err)
		}
	}
	if _, e = os.Stat(filepath.Join(w.Path, "target")); !os.IsNotExist(e) {
		t.Fatalf("Cargo target leaked into worktree: %v", e)
	}
}

func TestRustFailureEvidenceSelectedSectionOnly(t *testing.T) {
	output := "expected defect from compiler diagnostic\nrunning 1 test\ntest tests::selected ... FAILED\n\nfailures:\n\n---- tests::selected stdout ----\nthread 'tests::selected' panicked: different failure\n\nfailures:\n    tests::selected\n\ntest result: FAILED. 0 passed; 1 failed; 0 ignored; 0 measured; 0 filtered out; finished in 0.00s\n"
	r := RunEvidence{Output: output, ExitCode: 101}
	if e := parseRustTests(&r, Command{TestID: "tests::selected", ExpectedFailure: "expected defect"}); e != nil {
		t.Fatal(e)
	}
	if r.ExpectedFailure {
		t.Fatal("accepted expected failure outside selected failure section")
	}
}
