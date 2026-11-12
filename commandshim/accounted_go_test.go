package commandshim

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// accountedGoRun is an account-mode policy over root/wt and root/scratch
// whose RAM-backed roots are root/ram, outside the scope, and wt/shm, inside
// it.
func accountedGoRun(t *testing.T) (p Policy, root string) {
	t.Helper()
	root, worktree, scratch := accountedRun(t)
	for _, d := range []string{filepath.Join(root, "ram"), filepath.Join(worktree, "shm"), filepath.Join(worktree, "bin")} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return Policy{Worktree: worktree, Scratch: scratch, CWD: worktree, Local: true, RAMRoots: []string{filepath.Join(root, "ram"), filepath.Join(worktree, "shm")}}, root
}

// FAE-HOST-014, FAE-HOST-020: account mode's nested go flag policy is an
// allowlist, in the arguments and in GOFLAGS. A flag it does not list is
// refused; every path-valued flag (-o, -pkgdir, -outputdir, -overlay,
// -modfile, -reuse, -pgo, the debug traces and the test profiles, relative to
// -outputdir) must resolve inside the scope and never under a RAM-backed
// root; -gcflags, -ldflags and -asmflags may pass only listed tool flags, so
// none runs another program or writes a file; -compiler is gc and
// -buildmode never shared. GOFLAGS is split as go splits it, quotes included.
func TestAccountedGoFlagsAreAnAllowlist(t *testing.T) {
	p, root := accountedGoRun(t)
	ram, wt := filepath.Join(root, "ram"), p.Worktree
	for _, c := range []struct {
		argv   []string
		env    []string
		reason string
	}{
		{[]string{"go", "build", "-pkgdir", "shm/pkg", "."}, nil, "RAM-backed temporary build output"},
		{[]string{"go", "build", "-pkgdir=" + ram + "/pkg", "."}, nil, "target outside own worktree"},
		{[]string{"go", "test", "-coverprofile=shm/c.out", "."}, nil, "RAM-backed temporary build output"},
		{[]string{"go", "test", "-test.coverprofile=" + ram + "/c.out", "."}, nil, "target outside own worktree"},
		{[]string{"go", "test", "-outputdir", "shm", "-cpuprofile", "cpu.out", "."}, nil, "RAM-backed temporary build output"},
		{[]string{"go", "test", "-cpuprofile", "../../cpu.out", "-outputdir=sub", "."}, nil, "target outside own worktree"},
		{[]string{"go", "test", "-trace=shm/trace.out", "."}, nil, "RAM-backed temporary build output"},
		{[]string{"go", "build", "-overlay", root + "/overlay.json", "."}, nil, "target outside own worktree"},
		{[]string{"go", "build", "-modfile=../go.mod", "."}, nil, "target outside own worktree"},
		{[]string{"go", "list", "-m", "-u", "-reuse=" + ram + "/old.json", "all"}, nil, "target outside own worktree"},
		{[]string{"go", "build", "-pgo=" + ram + "/default.pgo", "."}, nil, "target outside own worktree"},
		{[]string{"go", "build", "-debug-trace=shm/trace", "."}, nil, "RAM-backed temporary build output"},
		{[]string{"go", "build", "-ldflags=-extld=/bin/sh", "."}, nil, "nested go -ldflags passes -extld=/bin/sh, which may run another program"},
		{[]string{"go", "build", "-ldflags", "all=-s -extldflags '-B /tmp'", "."}, nil, "nested go -ldflags passes -extldflags"},
		{[]string{"go", "build", "-ldflags=-tmpdir=/dev/shm", "."}, nil, "nested go -ldflags passes -tmpdir=/dev/shm"},
		{[]string{"go", "build", "-gcflags=-cpuprofile=" + ram + "/cpu", "."}, nil, "nested go -gcflags passes -cpuprofile="},
		{[]string{"go", "build", "-asmflags", "-o x.o", "."}, nil, "nested go -asmflags passes -o"},
		{[]string{"go", "build", "-gcflags=all=-d=checkptr", "."}, nil, "nested go -gcflags passes -d=checkptr"},
		{[]string{"go", "build", "-ldflags=-X 'main.v=1", "."}, nil, "nested go -ldflags passes -X 'main.v=1"},
		{[]string{"go", "build", "-gccgoflags=-x", "."}, nil, "unsupported nested go flag -gccgoflags=-x"},
		{[]string{"go", "build", "-linkshared", "."}, nil, "unsupported nested go flag -linkshared"},
		{[]string{"go", "test", ".", "-update"}, nil, "unsupported nested go flag -update"},
		{[]string{"go", "build", "-compiler=gccgo", "."}, nil, "nested go -compiler=gccgo is refused"},
		{[]string{"go", "build", "-buildmode", "shared", "."}, nil, "nested go -buildmode=shared is refused"},
		{[]string{"go", "build", "."}, []string{`GOFLAGS="-ldflags=-s -extld=/bin/sh"`}, "nested go -ldflags passes -extld=/bin/sh"},
		{[]string{"go", "build", "."}, []string{"GOFLAGS=-pkgdir=" + ram + "/pkg"}, "target outside own worktree"},
		{[]string{"go", "test", "."}, []string{"GOFLAGS=-mod=mod -update"}, "unsupported nested go flag -update"},
		{[]string{"go", "build", "."}, []string{"GOFLAGS='-mod=mod"}, "unsupported nested go flag GOFLAGS='-mod=mod"},
		{[]string{"go", "version"}, []string{"GOFLAGS=-toolexec=/bin/sh"}, "nested go flag -toolexec runs other programs"},
		{[]string{"go", "env", "GOPATH"}, []string{"GOFLAGS=-gccgoflags=-x"}, "unsupported nested go flag -gccgoflags=-x"},
	} {
		_, e := p.PrepareAccounted(c.argv, c.env)
		var refused *Refusal
		if !errors.As(e, &refused) || !strings.Contains(refused.Reason, c.reason) {
			t.Errorf("%v %v: want refusal %q, got %v", c.argv, c.env, c.reason, e)
		}
	}
	for _, c := range []struct {
		argv, env, want []string
	}{
		{[]string{"go", "build", "-ldflags", "-s -w -X main.version=1", "-gcflags=all=-N -l", "-trimpath", "-tags", "x", "-race", "-o", "bin/x", "./sub"}, nil,
			[]string{"go", "build", "-p=2", "-ldflags", "-s -w -X main.version=1", "-gcflags=all=-N -l", "-trimpath", "-tags", "x", "-race", "-o", filepath.Join(wt, "bin", "x"), "./sub"}},
		{[]string{"go", "test", "-outputdir", "sub", "-coverprofile", "c.out", "-cpuprofile=cpu.out", "-test.v", "-count=1", "./sub"}, nil,
			[]string{"go", "test", "-p=2", "-parallel=2", "-outputdir", "sub", "-coverprofile", "c.out", "-cpuprofile=cpu.out", "-test.v", "-count=1", "./sub"}},
		{[]string{"go", "list", "-deps", "-json=ImportPath", "-f", "{{.Dir}}", "-pgo=auto", "-buildvcs=false", "./..."}, nil,
			[]string{"go", "list", "-p=2", "-deps", "-json=ImportPath", "-f", "{{.Dir}}", "-pgo=auto", "-buildvcs=false", "./..."}},
		{[]string{"go", "vet", "-printf.funcs", "Logf", "-unusedresult", "-json", "./sub"}, nil,
			[]string{"go", "vet", "-p=2", "-printf.funcs", "Logf", "-unusedresult", "-json", "./sub"}},
		{[]string{"go", "build", "-buildmode=pie", "-compiler=gc", "-asmflags=-D X=1", "."}, []string{`GOFLAGS=-mod=mod "-ldflags=-s -w" -tags=x -p=64`},
			[]string{"go", "build", "-p=2", "-buildmode=pie", "-compiler=gc", "-asmflags=-D X=1", "."}},
	} {
		got, e := p.PrepareAccounted(c.argv, c.env)
		if e != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%v %v:\ngot  %q %v\nwant %q", c.argv, c.env, got, e, c.want)
		}
	}
}

// FAE-HOST-014: a nested go runs with the run's Go setting. The call's
// GOTOOLCHAIN and GOENV are set to local and off when it has none, and a call
// that names another toolchain or Go env file is refused, since either could
// put a go ahead of the wrappers or add flags the policy never sees. For go
// build, test, vet and list, a Go build location outside the scope under a
// RAM-backed root, or a GOWORK file outside the scope, is refused; probes
// (go version, go env) are not checked for them. Every call is recorded.
func TestAccountedGoRunsWithTheRunsGoEnvironment(t *testing.T) {
	p, root := accountedGoRun(t)
	ram := filepath.Join(root, "ram")
	tools := filepath.Join(root, "tools")
	if err := os.Mkdir(tools, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(tools, "go"), "#!/bin/sh\nprintf 'ran go %s GOTOOLCHAIN=%s GOENV=%s\\n' \"$1\" \"$GOTOOLCHAIN\" \"$GOENV\"\nexit 5\n")
	if err := os.Chmod(filepath.Join(tools, "go"), 0700); err != nil {
		t.Fatal(err)
	}
	shims, records := installAccountedForTest(t, root, RoleShims{Worktree: p.Worktree, Scratch: p.Scratch, Local: true, RAMRoots: p.RAMRoots}, tools+":/usr/bin:/bin")
	run := func(script string) (int, string) {
		t.Helper()
		cmd := exec.Command("/bin/sh", "-c", script)
		cmd.Dir = p.Worktree
		cmd.Env = []string{"PATH=" + shims + ":/usr/bin:/bin", "S=" + p.Scratch, "R=" + ram, "ROOT=" + root}
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return cmd.ProcessState.ExitCode(), string(out)
	}
	calls := 0
	for _, c := range []struct {
		script, want string
	}{
		{"go version", "ran go version GOTOOLCHAIN=local GOENV=off\n"},
		{"GOTOOLCHAIN=local GOENV=off go build .", "ran go build GOTOOLCHAIN=local GOENV=off\n"},
		{"GOTOOLCHAIN= GOENV= go build .", "ran go build GOTOOLCHAIN=local GOENV=off\n"},
		{`GOCACHE="$S/cache" GOTMPDIR="$S" GOWORK=off GOPATH="$ROOT/gopath" go build .`, "ran go build GOTOOLCHAIN=local GOENV=off\n"},
		{`GOCACHE="$R/cache" go env GOCACHE`, "ran go env GOTOOLCHAIN=local GOENV=off\n"},
	} {
		code, out := run(c.script)
		calls++
		if code != 5 || out != c.want {
			t.Errorf("%s: exit %d %q, want %q", c.script, code, out, c.want)
		}
	}
	for _, c := range []struct {
		script, reason string
	}{
		{`GOENV="$S/goenv" go build .`, "nested go with GOENV="},
		{"GOENV=/dev/null go version", "nested go with GOENV=/dev/null"},
		{"GOTOOLCHAIN=go1.99.0 go version", "nested go with GOTOOLCHAIN=go1.99.0"},
		{"GOTOOLCHAIN=auto go test .", "nested go with GOTOOLCHAIN=auto"},
		{`GOCACHE="$R/cache" go build .`, "nested go with GOCACHE=" + ram + "/cache: under a RAM-backed root outside the run's worktree and scratch directory"},
		{`GOTMPDIR="$R" go test .`, "nested go with GOTMPDIR=" + ram + ": under a RAM-backed root"},
		{`TMPDIR="$R/t" go vet .`, "nested go with TMPDIR=" + ram + "/t: under a RAM-backed root"},
		{`GOMODCACHE="$R/mod" go list .`, "nested go with GOMODCACHE="},
		{`GOCOVERDIR="$R/cover" go test .`, "nested go with GOCOVERDIR="},
		{`GOPATH="$ROOT/gopath:$R/gopath" go build .`, "nested go with GOPATH=" + ram + "/gopath: under a RAM-backed root"},
		{`GOWORK="$ROOT/go.work" go build .`, "nested go with GOWORK=" + root + "/go.work: a workspace file outside the run's worktree and scratch directory"},
	} {
		code, out := run(c.script)
		calls++
		if code != Refused || !strings.Contains(out, c.reason) || strings.Contains(out, "ran go") {
			t.Errorf("%s: exit %d %s", c.script, code, out)
		}
	}
	if count, _, err := ReadAccounted(records, 1); err != nil || count != calls {
		t.Errorf("%d calls recorded, made %d: %v", count, calls, err)
	}
}

// FAE-HOST-011/-014 for role sessions: a role's go test runs under the
// session's GOROOT mirror, so the go and gofmt its test starts (go test puts
// $GOROOT/bin first on the test's PATH) are the session's wrappers, which
// refuse a nested call with 77, instead of the real toolchain.
func TestRoleShimGoTestStartsTheSessionsWrapper(t *testing.T) {
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain unavailable")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(root, "wt")
	if err = os.Mkdir(wt, 0700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt, "go.mod"), "module example.com/role\n\ngo 1.23\n")
	write(t, filepath.Join(wt, "role_test.go"), `package role

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestStartsGo(t *testing.T) {
	for _, argv := range [][]string{{"go", "version"}, {"gofmt", "-l", "role_test.go"}} {
		out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
		code := 0
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
		fmt.Printf("nested %s exit=%d out=[%s]\n", argv[0], code, strings.TrimSpace(string(out)))
	}
}
`)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shims, err := InstallRoleShims(root, RoleShims{Broker: exe, Ledger: heavyLedger(t, 2), Worktree: wt, Build: true, Test: true, Local: true}, filepath.Dir(gobin)+":/usr/bin:/bin")
	if err != nil {
		t.Fatal(err)
	}
	env := PrependPath([]string{"PATH=/usr/bin:/bin", testBroker + "=1", "HOME=" + os.Getenv("HOME")}, shims)
	for _, key := range []string{"GOCACHE", "GOMODCACHE", "GOPATH"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, key+"="+v)
		}
	}
	cmd := exec.Command(filepath.Join(shims, "go"), "test", "-count=1", "-v", ".")
	cmd.Dir, cmd.Env = wt, env
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "nested go exit=77 out=[fae: command refused (exit 77): nested go inside a leased command is refused (FAE-HOST-014)") || !strings.Contains(string(out), "nested gofmt exit=77 out=[fae: command refused (exit 77): nested gofmt inside a leased command is refused") {
		t.Fatalf("role go test: %v\n%s", err, out)
	}
}

// nestedGoCase is one nested go call: its arguments and its own environment
// entries, and the vector the policy runs (want) or a refusal's reason.
// policyOnly marks a case exec cannot reproduce (a duplicated key, which exec
// passes once).
type nestedGoCase struct {
	argv, env, want []string
	reason          string
	policyOnly      bool
}

// checkNestedGoCases runs each case through the policy (PrepareAccounted,
// then accountedGoEnv under the run's Go setting g) and through a real
// accounting wrapper with the same policy, whose go is a fake that prints the
// vector it is given and exits 5: both must run want, or refuse with reason
// (the wrapper with 77, the fake never running). Every wrapper call is
// recorded.
func checkNestedGoCases(t *testing.T, p Policy, root string, g *NestedGo, cases []nestedGoCase) {
	t.Helper()
	tools, parent := filepath.Join(root, "fake-go"), filepath.Join(root, "fake-go-policy")
	for _, d := range []string{tools, parent} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	write(t, filepath.Join(tools, "go"), "#!/bin/sh\nprintf '%s\\n' \"$@\"\nexit 5\n")
	if err := os.Chmod(filepath.Join(tools, "go"), 0700); err != nil {
		t.Fatal(err)
	}
	shims, records := installAccountedForTest(t, parent, RoleShims{Worktree: p.Worktree, Scratch: p.Scratch, Local: true, RAMRoots: p.RAMRoots, NestedGo: g}, tools+":/usr/bin:/bin")
	wrapped := 0
	for _, c := range cases {
		got, err := p.PrepareAccounted(c.argv, c.env)
		if err == nil {
			_, err = p.accountedGoEnv(got, c.env, g)
		}
		var refused *Refusal
		switch {
		case c.reason == "" && (err != nil || !reflect.DeepEqual(got, c.want)):
			t.Errorf("policy %q %q: got %q %v, want %q", c.argv, c.env, got, err, c.want)
		case c.reason != "" && (!errors.As(err, &refused) || !strings.Contains(refused.Reason, c.reason)):
			t.Errorf("policy %q %q: want refusal %q, got %q %v", c.argv, c.env, c.reason, got, err)
		}
		if c.policyOnly {
			continue
		}
		cmd := exec.Command(filepath.Join(shims, "go"), c.argv[1:]...)
		cmd.Dir, cmd.Env = p.CWD, append([]string{"PATH=" + shims + ":/usr/bin:/bin"}, c.env...)
		out, err := cmd.CombinedOutput()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		wrapped++
		// Under scripts/test.sh the wrapper is a coverage-built test binary,
		// which warns on its own exit (a refusal) that it wrote no coverage.
		out = []byte(strings.ReplaceAll(string(out), "warning: GOCOVERDIR not set, no coverage data emitted\n", ""))
		code := cmd.ProcessState.ExitCode()
		if c.reason == "" && (code != 5 || string(out) != strings.Join(c.want[1:], "\n")+"\n") {
			t.Errorf("wrapper %q %q: exit %d\n%s\nwant %q", c.argv, c.env, code, out, c.want)
		}
		if c.reason != "" && (code != Refused || !strings.Contains(string(out), c.reason) || strings.Count(string(out), "\n") != 1) {
			t.Errorf("wrapper %q %q: exit %d %s, want refusal %q", c.argv, c.env, code, out, c.reason)
		}
	}
	if count, _, err := ReadAccounted(records, 1); err != nil || count != wrapped {
		t.Errorf("%d calls recorded, made %d: %v", count, wrapped, err)
	}
}

// FAE-HOST-014: a nested go call's environment is an allowlist
// (nestedGoEnv). It may set freely only what changes the build's target,
// where modules come from and the runtime's settings. The programs go and
// cgo run (CC, CXX, PKG_CONFIG, GOCACHEPROG, GOAUTH's command, ...), the C
// compiler's and linker's flags, GORACE and any Go setting the policy does
// not know keep the run's own value or stay unset; GOMAXPROCS may also be at
// most the call's clamp. Probes are checked too (go env runs the C compiler
// for GOGCCFLAGS), and so is every entry of a duplicated key.
func TestAccountedGoEnvironmentIsAnAllowlist(t *testing.T) {
	p, root := accountedGoRun(t)
	g := &NestedGo{Env: map[string]string{"CC": "/opt/own/cc", "GOMAXPROCS": "8", "CGO_LDFLAGS": "-L/opt/own/lib"}}
	build, suite := []string{"go", "build", "."}, []string{"go", "test", "./..."}
	built, ran := []string{"go", "build", "-p=2", "."}, []string{"go", "test", "-p=1", "-parallel=1", "./..."}
	cases := []nestedGoCase{
		{argv: build, env: []string{"CC=/opt/own/cc", "CGO_LDFLAGS=-L/opt/own/lib", "GOMAXPROCS=8"}, want: built},
		{argv: build, env: []string{"CC=", "CXX=", "GOAUTH=off", "GOMAXPROCS=2"}, want: built},
		{argv: suite, env: []string{"GOMAXPROCS=1", "GOOS=linux", "GOARCH=arm64", "GOAMD64=v2", "CGO_ENABLED=0", "GO_EXTLINK_ENABLED=0", "GOEXPERIMENT=nosynchashtriemap", "GOFIPS140=off"}, want: ran},
		{argv: build, env: []string{"GO111MODULE=on", "GOPROXY=off", "GONOPROXY=example.com", "GOPRIVATE=example.com", "GONOSUMDB=example.com", "GOSUMDB=off", "GOINSECURE=example.com", "GOBIN=/opt/bin"}, want: built},
		{argv: build, env: []string{"GODEBUG=gocachehash=1", "GOGC=50", "GOMEMLIMIT=1GiB", "GOTRACEBACK=all", "CGO_CFLAGS_DISALLOW=-B.*", "CGO_LDFLAGS_DISALLOW=-fuse-ld=.*", "LD_FAE_PROBE=1", "FAE_PROBE=1"}, want: built},
		{argv: []string{"go", "version"}, env: []string{"GOMAXPROCS=2", "CC=/opt/own/cc"}, want: []string{"go", "version"}},
	}
	for key, value := range map[string]string{
		"CC": "/bin/sh", "CXX": "/bin/sh", "FC": "/bin/sh", "AR": "/bin/sh", "GCCGO": "/bin/sh", "GCCGOTOOLDIR": root, "PKG_CONFIG": "/bin/sh",
		"CGO_CFLAGS": "-B" + root, "CGO_CPPFLAGS": "-fplugin=" + root + "/p.so", "CGO_CXXFLAGS": "-wrapper,/bin/sh", "CGO_FFLAGS": "-B" + root,
		"CGO_LDFLAGS": "-fuse-ld=" + root + "/ld", "CGO_CFLAGS_ALLOW": ".*", "CGO_LDFLAGS_ALLOW": ".*", "GOCACHEPROG": "/bin/sh",
		"GOAUTH": "command /bin/sh", "GOVCS": "*:all", "GORACE": "log_path=/dev/shm/fae-race", "GOFAEUNKNOWN": "1", "CGO_FAEUNKNOWN": "1",
	} {
		kv := key + "=" + value
		cases = append(cases, nestedGoCase{argv: build, env: []string{kv}, reason: "nested go with " + kv + ": " + key + " may name a program go runs"})
	}
	for _, c := range []struct {
		argv  []string
		env   string
		clamp string
	}{
		{build, "GOMAXPROCS=64", "2"}, {build, "GOMAXPROCS=3", "2"}, {build, "GOMAXPROCS=0", "2"}, {build, "GOMAXPROCS=many", "2"},
		{suite, "GOMAXPROCS=2", "1"}, {[]string{"go", "env", "GOMAXPROCS"}, "GOMAXPROCS=64", "2"},
	} {
		cases = append(cases, nestedGoCase{argv: c.argv, env: []string{c.env}, reason: "nested go with " + c.env + ": more threads than the call's clamp, -p=" + c.clamp})
	}
	cases = append(cases,
		nestedGoCase{argv: []string{"go", "env", "GOGCCFLAGS"}, env: []string{"CC=/bin/sh"}, reason: "nested go with CC=/bin/sh"},
		nestedGoCase{argv: build, env: []string{"CC=", "CC=/bin/sh"}, reason: "nested go with CC=/bin/sh", policyOnly: true},
		nestedGoCase{argv: build, env: []string{"CC=/opt/own/cc", "CC=/bin/sh"}, reason: "nested go with CC=/bin/sh", policyOnly: true},
	)
	checkNestedGoCases(t, p, root, g, cases)
	// The broker records the run's own values of exactly the keys a nested
	// call may keep but not change, the last of each.
	kept := NestedGoKeptEnv([]string{"CC=/a", "CC=/opt/own/cc", "GOMAXPROCS=8", "GOCACHE=/c", "GOOS=linux", "GOFLAGS=-mod=mod", "GOROOT=/r", "GOFAEUNKNOWN=1", "PATH=/bin", "NOEQUALS"})
	if want := map[string]string{"CC": "/opt/own/cc", "GOMAXPROCS": "8", "GOCACHE": "/c", "GOFAEUNKNOWN": "1"}; !reflect.DeepEqual(kept, want) {
		t.Errorf("NestedGoKeptEnv = %v, want %v", kept, want)
	}
	for _, key := range goBuildLocationKeys {
		if nestedGoEnv[key] != nestedGoEnvLocation {
			t.Errorf("build location %s is not a location in nestedGoEnv", key)
		}
	}
}

// FAE-HOST-014, FAE-HOST-015, FAE-HOST-020: a nested go test's -cpu
// (-test.cpu, in GOFLAGS too) may list no GOMAXPROCS value above the call's
// clamp; after -args, the test binary's -test. flags are an allowlist of
// flags that write no file (-test.parallel dropped, -test.cpu checked), the
// test's own flags pass, and a "--" ends the test binary's flags; a target
// with "..." or a reserved pattern (all, std, cmd, work, tool) is recursive,
// so an unfiltered nested go test of it runs as a whole suite; and the
// linker may not be given a dynamic linker (-I) or library path (-r) for the
// binaries it builds, test binaries included.
func TestAccountedGoClampsCPUTestBinaryFlagsAndPatterns(t *testing.T) {
	p, root := accountedGoRun(t)
	test := func(args ...string) []string { return append([]string{"go", "test"}, args...) }
	ran := func(threads string, args ...string) []string {
		return append([]string{"go", "test", "-p=" + threads, "-parallel=" + threads}, args...)
	}
	cases := []nestedGoCase{
		{argv: test("-cpu", "1,2", "."), want: ran("2", "-cpu", "1,2", ".")},
		{argv: test("-run", "X", "-test.cpu=2", "./..."), want: ran("2", "-run", "X", "-test.cpu=2", "./...")},
		{argv: test(".", "-args", "-test.v", "-test.run", "X", "-test.count=2", "-test.cpu", "2", "-test.parallel", "9", "-update", "-golden=x"),
			want: ran("2", ".", "-args", "-test.v", "-test.run", "X", "-test.count=2", "-test.cpu", "2", "-update", "-golden=x")},
		{argv: test(".", "-args", "--", "-test.coverprofile=/dev/shm/c"), want: ran("2", ".", "-args", "--", "-test.coverprofile=/dev/shm/c")},
		{argv: test("-run", "X", "all"), want: ran("2", "-run", "X", "all")},
		{argv: test("example.com/x"), want: ran("2", "example.com/x")},
		{argv: []string{"go", "build", "std"}, want: []string{"go", "build", "-p=2", "std"}},
		{argv: []string{"go", "build", "-ldflags=-linkmode=external -s", "."}, want: []string{"go", "build", "-p=2", "-ldflags=-linkmode=external -s", "."}},
		{argv: test("-cpu=64", "."), reason: "nested go test -cpu 64 runs tests at more threads than the call's clamp, -p=2"},
		{argv: test("-test.cpu", "1,4", "."), reason: "nested go test -cpu 1,4 runs tests at more threads than the call's clamp, -p=2"},
		{argv: test("-cpu=2", "./..."), reason: "nested go test -cpu 2 runs tests at more threads than the call's clamp, -p=1"},
		{argv: test("-cpu=2", "all"), reason: "nested go test -cpu 2 runs tests at more threads than the call's clamp, -p=1"},
		{argv: test("."), env: []string{"GOFLAGS=-cpu=8"}, reason: "nested go test -cpu 8 runs tests at more threads"},
		{argv: test(".", "-args", "-test.cpu=64"), reason: "nested go test -cpu 64 runs tests at more threads"},
		{argv: test(".", "-args", "-test.cpu", "64"), reason: "nested go test -cpu 64 runs tests at more threads"},
		{argv: []string{"go", "build", "-ldflags", "-I " + root + "/ld.so", "."}, reason: "nested go -ldflags passes -I, which may run another program"},
		{argv: []string{"go", "build", "-ldflags=-s -r " + root, "."}, reason: "nested go -ldflags passes -r, which may run another program"},
	}
	for _, flag := range []string{"-test.coverprofile=/dev/shm/c", "-test.gocoverdir=/dev/shm", "-test.outputdir=" + root, "-test.cpuprofile=c", "-test.memprofile=m",
		"-test.blockprofile=b", "-test.mutexprofile=m", "-test.trace=t", "-test.testlogfile=" + root + "/log", "-test.fuzzcachedir=" + root,
		"-test.fuzzworker", "-test.artifacts", "-test.faeunknown", "--test.coverprofile=/dev/shm/c"} {
		cases = append(cases, nestedGoCase{argv: test(".", "-args", "-test.v", flag), reason: "nested go test passes " + flag + " to the test binary after -args"})
	}
	for _, pattern := range []string{"all", "std", "cmd", "work", "tool", "./...", "example.com/x/...", "..."} {
		cases = append(cases, nestedGoCase{argv: test("-count=1", pattern), want: ran("1", "-count=1", pattern)})
	}
	checkNestedGoCases(t, p, root, &NestedGo{}, cases)
}
