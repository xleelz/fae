package commandshim

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// The refusals of account mode's go allowlist and Go environment (shim.go),
// in the census corpus (refusal_corpus_test.go).
func init() {
	for k, v := range map[string]struct {
		reason string
		run    func(t *testing.T, p Policy) error
	}{
		`"unsupported nested go flag " + a + "; nested calls may pass only the go build, test, vet and list flags the policy lists"`:                                          {"unsupported nested go flag -linkshared", accounted(nil, "go", "build", "-linkshared", ".")},
		`"nested go -" + name + " passes " + bad + ", which may run another program or write a file; nested calls may pass only listed compiler, linker and assembler flags"`: {"nested go -ldflags passes -extld=/bin/sh", accounted(nil, "go", "build", "-ldflags=-s -extld=/bin/sh", ".")},
		`"nested go -" + name + "=" + value + " is refused; nested builds use the gc compiler and install no shared libraries"`:                                               {"nested go -compiler=gccgo is refused", accounted(nil, "go", "build", "-compiler", "gccgo", ".")},
		`"nested go with " + kv + ": nested go runs with the run's GOROOT mirror, GOTOOLCHAIN=local and GOENV=off, so the go its tests start meets the run's wrappers and no Go env file sets flags the policy never sees"`: {"nested go with GOTOOLCHAIN=go1.99.0", func(t *testing.T, p Policy) error {
			_, e := p.accountedGoEnv([]string{"go", "version"}, []string{"GOTOOLCHAIN=go1.99.0"}, nil)
			return e
		}},
		`"nested go with " + key + "=" + d + ": " + why + " outside the run's worktree and scratch directory"`: {"under a RAM-backed root outside the run's worktree", func(t *testing.T, p Policy) error {
			p.RAMRoots = []string{"/dev/shm"}
			_, e := p.accountedGoEnv([]string{"go", "build", "-p=2", "."}, []string{"GOCACHE=/dev/shm/fae-nested-cache"}, nil)
			return e
		}},
		`"nested go with " + kv + ": " + key + " may name a program go runs or that program's flags, or is a Go setting the policy does not know; nested calls keep the run's own value or leave it unset"`: {"nested go with CC=/bin/sh: CC may name a program go runs", func(t *testing.T, p Policy) error {
			_, e := p.accountedGoEnv([]string{"go", "build", "-p=2", "."}, []string{"CC=/bin/sh"}, nil)
			return e
		}},
		`"nested go with " + kv + ": more threads than the call's clamp, -p=" + strconv.Itoa(threads) + "; nested calls keep the run's own value or set at most the clamp"`: {"nested go with GOMAXPROCS=64: more threads than the call's clamp, -p=2", func(t *testing.T, p Policy) error {
			_, e := p.accountedGoEnv([]string{"go", "build", "-p=2", "."}, []string{"GOMAXPROCS=64"}, nil)
			return e
		}},
		`"nested go test -cpu " + list + " runs tests at more threads than the call's clamp, -p=" + strconv.Itoa(threads) + "; nested calls may list only values from 1 to the clamp"`:                                               {"nested go test -cpu 1,64 runs tests at more threads than the call's clamp, -p=2", accounted(nil, "go", "test", "-cpu", "1,64", ".")},
		`"nested go test passes " + a + " to the test binary after -args, unchecked; nested calls may pass there only test binary flags that write no file (give a profile or output flag before -args, where its path is checked)"`: {"nested go test passes -test.coverprofile=/dev/shm/c to the test binary after -args", accounted(nil, "go", "test", ".", "-args", "-test.coverprofile=/dev/shm/c")},
	} {
		shimRefusals[k] = v
	}
}

// fakeGoRoot makes dir look like a GOROOT to cmd/go (it has pkg/tool), with
// bin/go, bin/gofmt and another tool, and returns it.
func fakeGoRoot(t *testing.T, dir string) string {
	t.Helper()
	for _, d := range []string{"pkg/tool/linux_amd64", "src/fmt", "bin"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"bin/go", "bin/gofmt", "bin/vettool-x", "VERSION"} {
		write(t, filepath.Join(dir, name), "#!/bin/sh\n")
		if err := os.Chmod(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// GoRoot finds the GOROOT a go command uses as cmd/go does: the environment's
// GOROOT first, then the directory two up from the binary, as given and with
// symlinks resolved; never a directory without pkg/tool. InstallGoRoot mirrors
// it with go and gofmt linked to the run's wrappers, everything else to the
// real GOROOT; a mirror of the mirror (a broker inside the run) leads back to
// it, and nothing else does.
func TestGoRootMirrorsTheRunsToolchain(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := fakeGoRoot(t, filepath.Join(root, "go"))
	other := fakeGoRoot(t, filepath.Join(root, "other"))
	for _, d := range []string{"usr/bin", "loose", "run", "inner", "notroot/bin"} {
		if err = os.MkdirAll(filepath.Join(root, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Symlink(filepath.Join(real, "bin", "go"), filepath.Join(root, "usr/bin/go")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "loose", "go"), "#!/bin/sh\n")
	if err = os.Chmod(filepath.Join(root, "loose", "go"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		binary string
		env    []string
		want   string
	}{
		{filepath.Join(real, "bin", "go"), nil, real},
		{filepath.Join(root, "usr/bin/go"), nil, real},
		{"", []string{"PATH=" + filepath.Join(root, "usr/bin") + ":/bin"}, real},
		{filepath.Join(root, "usr/bin/go"), []string{"GOROOT=" + other + "/"}, other},
		{filepath.Join(real, "bin", "go"), []string{"GOROOT=" + filepath.Join(root, "notroot")}, ""},
		{filepath.Join(root, "loose", "go"), nil, ""},
		{"", []string{"PATH=" + filepath.Join(root, "notroot/bin")}, ""},
	} {
		if got := GoRoot(c.binary, c.env); got != c.want {
			t.Errorf("GoRoot(%s, %v) = %q, want %q", c.binary, c.env, got, c.want)
		}
	}
	parent := filepath.Join(root, "run")
	mirror, err := InstallGoRoot(parent, real)
	if err != nil || mirror != filepath.Join(parent, "goroot") || !isGoRootDir(mirror) {
		t.Fatal(mirror, err)
	}
	for name, want := range map[string]string{
		"pkg": filepath.Join(real, "pkg"), "src": filepath.Join(real, "src"), "VERSION": filepath.Join(real, "VERSION"),
		"bin/go": filepath.Join(parent, "command-shims", "go"), "bin/gofmt": filepath.Join(parent, "command-shims", "gofmt"),
		"bin/vettool-x": filepath.Join(real, "bin", "vettool-x"),
	} {
		if got, err := os.Readlink(filepath.Join(mirror, name)); err != nil || got != want {
			t.Errorf("mirror %s -> %q %v, want %q", name, got, err, want)
		}
	}
	if st, err := os.Lstat(filepath.Join(mirror, "bin")); err != nil || !st.IsDir() {
		t.Fatal("mirror bin is not its own directory", err)
	}
	// The mirror's go, a wrapper, is what a binary search finds; its GOROOT
	// is the mirror, so a broker inside the run mirrors the mirror.
	if got := GoRoot(filepath.Join(mirror, "bin", "go"), nil); got != mirror {
		t.Fatalf("GoRoot of the mirror's go = %q", got)
	}
	inner, err := InstallGoRoot(filepath.Join(root, "inner"), mirror)
	if err != nil {
		t.Fatal(err)
	}
	for goroot, want := range map[string]bool{mirror: true, mirror + "/": true, inner: true, real: false, other: false, filepath.Join(root, "nowhere"): false} {
		if got := mirrorsGoRoot(goroot, mirror); got != want {
			t.Errorf("mirrorsGoRoot(%s) = %t, want %t", goroot, got, want)
		}
	}
	if mirrorsGoRoot(mirror, inner) {
		t.Error("the run's mirror passes as a mirror of a broker's inside it")
	}
	if _, err = InstallGoRoot(parent, real); err == nil {
		t.Error("a second mirror replaced the first")
	}
	if _, err = InstallGoRoot("relative", real); err == nil {
		t.Error("relative mirror parent accepted")
	}
}

// The real toolchain runs unchanged under a mirror: go env GOROOT reports the
// mirror, which go puts first on its tests' PATH.
func TestRealGoUnderAGoRootMirror(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain unavailable")
	}
	real := GoRoot(gobin, nil)
	if real == "" {
		t.Skip("go is outside any GOROOT")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mirror, err := InstallGoRoot(parent, real)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gobin, "env", "GOROOT", "GOVERSION")
	cmd.Env = GoRunEnv(append(os.Environ(), "GOROOT=/nonexistent"), mirror)
	out, err := cmd.CombinedOutput()
	if fields := strings.Fields(string(out)); err != nil || len(fields) != 2 || fields[0] != mirror || !strings.HasPrefix(fields[1], "go") {
		t.Fatalf("go env under the mirror: %v %s", err, out)
	}
}

// GoRunEnv replaces GOTOOLCHAIN and GOENV, and GOROOT when a mirror is given;
// accountedGoEnv applies it to a nested go: the run's mirror when the call
// has no GOROOT, a mirror a broker inside the run made of it kept, any other
// GOROOT, toolchain or env file refused, and the run's own build locations
// kept wherever they are.
func TestAccountedGoEnvUnderTheRunsMirror(t *testing.T) {
	if got, want := GoRunEnv([]string{"GOROOT=/x", "GOTOOLCHAIN=auto", "A=1", "GOENV=/e", "GOTOOLCHAIN=go1.9"}, "/m"), []string{"A=1", "GOROOT=/m", "GOTOOLCHAIN=local", "GOENV=off"}; !reflect.DeepEqual(got, want) {
		t.Errorf("GoRunEnv = %q, want %q", got, want)
	}
	if got, want := GoRunEnv([]string{"GOROOT=/x", "GOENV=/e"}, ""), []string{"GOROOT=/x", "GOTOOLCHAIN=local", "GOENV=off"}; !reflect.DeepEqual(got, want) {
		t.Errorf("GoRunEnv without a mirror = %q, want %q", got, want)
	}
	p, root := accountedGoRun(t)
	ram := filepath.Join(root, "ram")
	real := fakeGoRoot(t, filepath.Join(root, "go"))
	if err := os.Mkdir(filepath.Join(root, "inner"), 0700); err != nil {
		t.Fatal(err)
	}
	mirror, err := InstallGoRoot(root, real)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := InstallGoRoot(filepath.Join(root, "inner"), mirror)
	if err != nil {
		t.Fatal(err)
	}
	g := &NestedGo{GoRoot: mirror, Env: map[string]string{"GOCACHE": ram + "/own"}}
	build := []string{"go", "build", "-p=2", "."}
	for _, c := range []struct {
		argv, env, want []string
	}{
		{build, []string{"A=1"}, []string{"A=1", "GOROOT=" + mirror, "GOTOOLCHAIN=local", "GOENV=off"}},
		{build, []string{"GOROOT=", "GOTOOLCHAIN=local", "GOENV=off"}, []string{"GOROOT=" + mirror, "GOTOOLCHAIN=local", "GOENV=off"}},
		{build, []string{"GOROOT=" + inner}, []string{"GOROOT=" + inner, "GOTOOLCHAIN=local", "GOENV=off"}},
		{build, []string{"GOCACHE=" + ram + "/own"}, []string{"GOCACHE=" + ram + "/own", "GOROOT=" + mirror, "GOTOOLCHAIN=local", "GOENV=off"}},
		{[]string{"go", "env", "GOCACHE"}, []string{"GOCACHE=" + ram + "/other"}, []string{"GOCACHE=" + ram + "/other", "GOROOT=" + mirror, "GOTOOLCHAIN=local", "GOENV=off"}},
	} {
		got, err := p.accountedGoEnv(c.argv, c.env, g)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v: %q %v, want %q", c.env, got, err, c.want)
		}
	}
	for _, env := range [][]string{{"GOROOT=" + real}, {"GOROOT=" + root}, {"GOROOT=/usr/local/go"}, {"GOCACHE=" + ram + "/other"}, {"GOENV=" + root + "/env"}} {
		if _, err := p.accountedGoEnv(build, env, g); err == nil || !strings.Contains(err.Error(), "nested go with "+env[0]) {
			t.Errorf("%v accepted: %v", env, err)
		}
	}
	// A run without a mirrored toolchain leaves the call's GOROOT alone.
	if got, err := p.accountedGoEnv(build, []string{"GOROOT=" + real}, &NestedGo{}); err != nil || !slices.Contains(got, "GOROOT="+real) {
		t.Errorf("no mirror: %q %v", got, err)
	}
}

// An account-mode run's nested go and gofmt may use an on-disk /var/tmp,
// where test suites (Fae's own) keep short-path fixtures and build: it joins
// the scope (Temp), and only the system temporary directory, /dev/shm and a
// RAM-backed /var/tmp are RAM-backed roots. Outside the worktree, TMPDIR and
// Temp, a nested build is still refused.
func TestAccountedTempTakesAnOnDiskVarTmp(t *testing.T) {
	scope, ram := AccountedTemp()
	varTmp, err := filepath.EvalSymlinks("/var/tmp")
	if err != nil {
		t.Skip("no /var/tmp")
	}
	if onDisk := !ramFileSystem(varTmp); onDisk != slices.Contains(scope, varTmp) || onDisk == slices.Contains(ram, varTmp) {
		t.Fatalf("on disk %t: scope %q ram %q", onDisk, scope, ram)
	}
	for _, r := range []string{"/tmp", "/dev/shm"} {
		if physical, err := filepath.EvalSymlinks(r); err == nil && !slices.Contains(ram, physical) {
			t.Errorf("%s is not RAM-backed: %q", r, ram)
		}
	}
	if !ramFileSystem("/dev/shm") {
		t.Error("/dev/shm is not a RAM file system")
	}
	p, root := accountedGoRun(t)
	fixture := filepath.Join(root, "fixture")
	if err = os.Mkdir(fixture, 0700); err != nil {
		t.Fatal(err)
	}
	p.Temp = []string{fixture}
	if got, err := p.PrepareAccounted([]string{"go", "build", "-o", filepath.Join(fixture, "bin", "x"), "./sub"}, nil); err != nil || got[len(got)-2] != filepath.Join(fixture, "bin", "x") {
		t.Errorf("output in a Temp root: %q %v", got, err)
	}
	p.CWD = fixture
	if _, err = p.PrepareAccounted([]string{"go", "test", "-count=1", "."}, nil); err != nil {
		t.Errorf("go test in a Temp root: %v", err)
	}
	if _, err = p.accountedGoEnv([]string{"go", "test", "-p=2", "."}, []string{"TMPDIR=" + fixture}, nil); err != nil {
		t.Errorf("TMPDIR in a Temp root: %v", err)
	}
	p.CWD = root
	if _, err = p.PrepareAccounted([]string{"go", "build", "."}, nil); err == nil || !strings.Contains(err.Error(), "outside the run's worktree and scratch directory") {
		t.Errorf("build outside the scope and Temp: %v", err)
	}
}

// GoRootGap names the case where a run would get no GOROOT mirror although it
// has a go: a go outside any GOROOT (a version manager's shim, a copied
// binary) or a GOROOT without a toolchain. A go in a GOROOT, through a
// symlink too, and no go at all are no gap.
func TestGoRootGapNamesAGoOutsideAnyGoRoot(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := fakeGoRoot(t, filepath.Join(root, "go"))
	for _, d := range []string{"usr/bin", "shims", "empty"} {
		if err = os.MkdirAll(filepath.Join(root, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Symlink(filepath.Join(real, "bin", "go"), filepath.Join(root, "usr/bin/go")); err != nil {
		t.Fatal(err)
	}
	shim := filepath.Join(root, "shims", "go")
	write(t, shim, "#!/bin/sh\nexec "+filepath.Join(real, "bin", "go")+" \"$@\"\n")
	if err = os.Chmod(shim, 0700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		env       []string
		binary    string
		gap       string
		remediate bool
	}{
		{[]string{"PATH=" + filepath.Join(real, "bin")}, filepath.Join(real, "bin", "go"), "", false},
		{[]string{"PATH=" + filepath.Join(root, "usr/bin")}, filepath.Join(root, "usr/bin/go"), "", false},
		{[]string{"PATH=" + filepath.Join(root, "empty")}, "", "", false},
		{[]string{"PATH=" + filepath.Join(root, "shims") + ":" + filepath.Join(real, "bin")}, shim, "the go on PATH, " + shim + ", sits outside any GOROOT", true},
		{[]string{"PATH=" + filepath.Join(real, "bin"), "GOROOT=" + filepath.Join(root, "empty")}, filepath.Join(real, "bin", "go"), "GOROOT=" + filepath.Join(root, "empty") + " holds no Go toolchain", false},
	} {
		binary, gap := GoRootGap(c.env)
		if binary != c.binary || (c.gap == "") != (gap == "") || !strings.Contains(gap, c.gap) {
			t.Errorf("GoRootGap(%v) = %q, %q; want %q, %q", c.env, binary, gap, c.binary, c.gap)
		}
		if gap != "" && (!strings.Contains(gap, "no GOROOT mirror") || !strings.Contains(gap, "FAE-HOST-014") || c.remediate != strings.Contains(gap, "first on the daemon's PATH")) {
			t.Errorf("gap %q does not say what follows and what to do", gap)
		}
	}
}

// SealGoRoot makes a mirror and its bin/ read-only, so neither an entry nor
// the wrappers' links can be replaced or added by mistake; RemoveGoRoot
// removes a sealed mirror (and does nothing without one).
func TestSealedGoRootIsReadOnlyAndRemovable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := fakeGoRoot(t, filepath.Join(parent, "real"))
	mirror, err := InstallGoRoot(parent, real)
	if err != nil {
		t.Fatal(err)
	}
	if err = SealGoRoot(mirror); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func() error{
		func() error { return os.Symlink("/bin/true", filepath.Join(mirror, "bin", "x")) },
		func() error { return os.Remove(filepath.Join(mirror, "bin", "go")) },
		func() error { return os.Rename(filepath.Join(mirror, "bin"), filepath.Join(mirror, "old")) },
		func() error { return os.Remove(filepath.Join(mirror, "pkg")) },
	} {
		if err = change(); err == nil {
			t.Fatal("a sealed mirror changed")
		}
	}
	if !isGoRootDir(mirror) {
		t.Fatal("sealed mirror lost its toolchain")
	}
	if err = RemoveGoRoot(parent); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(mirror); !os.IsNotExist(err) {
		t.Fatal("sealed mirror not removed", err)
	}
	if err = RemoveGoRoot(parent); err != nil {
		t.Fatal("no mirror to remove:", err)
	}
}
