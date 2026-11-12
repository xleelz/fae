package commandshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func rustPolicy(t *testing.T) Policy {
	t.Helper()
	d := t.TempDir()
	if e := os.WriteFile(filepath.Join(d, "Cargo.toml"), []byte("[package]\nname = \"fixture\"\nversion = \"0.1.0\"\nedition = \"2021\"\n"), 0600); e != nil {
		t.Fatal(e)
	}
	return Policy{Worktree: d, CWD: d, CodeRole: true, LeaseHeld: true}
}
func TestRustPolicyScopesAndClamps(t *testing.T) {
	p := rustPolicy(t)
	got, e := p.prepareRust([]string{"cargo", "test", "-j", "900", "--lib"})
	if e != nil {
		t.Fatal(e)
	}
	s := strings.Join(got, " ")
	if !strings.Contains(s, "--jobs=1 --offline --locked") || strings.Contains(s, "900") {
		t.Fatal(s)
	}
	for _, a := range [][]string{{"cargo", "test", "--target-dir", "/tmp/escape"}, {"cargo", "build", "--config", "build.rustc=bad"}, {"cargo", "test", "--", "--nocapture"}, {"cargo", "test", "--manifest-path", "../Cargo.toml"}, {"cargo", "--version"}} {
		if _, e = p.prepareRust(a); e == nil {
			t.Fatalf("accepted %v", a)
		}
	}
	if e = os.WriteFile(filepath.Join(p.Worktree, "build.rs"), []byte("fn main(){}"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = p.prepareRust([]string{"cargo", "check"}); e == nil {
		t.Fatal("accepted build script")
	}
}
func TestRustfmtLeafNative(t *testing.T) {
	if _, e := exec.LookPath("rustfmt"); e != nil {
		t.Skip(e)
	}
	p := rustPolicy(t)
	f := filepath.Join(p.Worktree, "leaf.rs")
	if e := os.WriteFile(f, []byte("fn main(){println!(\"ok\");}\n"), 0600); e != nil {
		t.Fatal(e)
	}
	a, e := p.prepareRust([]string{"rustfmt", "--edition", "2021", f})
	if e != nil {
		t.Fatal(e)
	}
	if b, e := exec.Command(a[0], a[1:]...).CombinedOutput(); e != nil {
		t.Fatalf("%s: %v", b, e)
	}
	if e = os.WriteFile(f, []byte("mod external;\n"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = p.prepareRust([]string{"rustfmt", f}); e == nil {
		t.Fatal("accepted external module traversal")
	}
}
func TestRustEnvironmentPrivate(t *testing.T) {
	if _, e := exec.LookPath("rustc"); e != nil {
		t.Skip(e)
	}
	env, e := RustEnvironment([]string{"cargo", "check"}, []string{"RUSTC_WRAPPER=bad", "CARGO_BUILD_TARGET_DIR=/escape", "PATH=" + os.Getenv("PATH")}, t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range env {
		if strings.Contains(s, "bad") || strings.Contains(s, "/escape") {
			t.Fatal(env)
		}
	}
}

func TestRustManifestNonliteralPathsRefused(t *testing.T) {
	for _, entry := range []string{"[dependencies]\nother={path=\"\"\"/outside\"\"\"}\n", "[dependencies]\nother={\"path\"=\"/outside\"}\n", "\"build\" = \"outside.rs\"\n"} {
		p := rustPolicy(t)
		f := filepath.Join(p.Worktree, "Cargo.toml")
		b, e := os.ReadFile(f)
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(f, append(b, []byte(entry)...), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e = p.prepareRust([]string{"cargo", "check"}); e == nil {
			t.Fatalf("accepted %s", entry)
		}
	}
}
