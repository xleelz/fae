package commandshim

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// go test, go run and go generate start their programs with $GOROOT/bin first
// on PATH (cmd/go's base.AppendPATH), so a go or gofmt that a test starts
// would find the real toolchain ahead of a brokered run's wrappers. A run
// whose Go toolchain is known therefore runs go under a GOROOT mirror
// (InstallGoRoot): every entry of the real GOROOT but bin/ is a link to the
// real one, and bin/go and bin/gofmt are the run's wrappers. Go puts the
// mirror's bin/ first, so the wrappers come first again (FAE-HOST-014).

// NestedGo is an account-mode run's Go setting for its nested go and gofmt
// calls (accountedGoEnv): GoRoot is the run's GOROOT mirror, "" when the run
// has no Go toolchain; Env holds the run's own values of the Go environment
// a nested call may keep but not change (NestedGoKeptEnv): its build
// locations, which it may keep wherever they are, the programs go runs and
// their flags, and GOMAXPROCS; and Temp are the on-disk temporary roots in
// the calls' scope (AccountedTemp). Programs are the absolute paths, on the
// run's own PATH, of the programs a nested go call finds by name for cgo
// (NestedGoPrograms), which accountedCgoPrograms pins each call to.
type NestedGo struct {
	GoRoot   string            `json:"goroot,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Temp     []string          `json:"temp,omitempty"`
	Programs map[string]string `json:"programs,omitempty"`
}

// sharedTempDir is the host's shared temporary directory that survives a
// reboot, conventionally on disk. Test suites keep fixtures there that need
// short paths, such as Unix sockets (at most 108 bytes); Fae's own daemon
// and CLI tests do, and build and test inside them.
const sharedTempDir = "/var/tmp"

// AccountedTemp returns the on-disk temporary roots an account-mode run's
// nested go and gofmt may use beside its worktree and TMPDIR, and the
// RAM-backed roots they may not write under (FAE-HOST-020). The system
// temporary directory and /dev/shm are always RAM-backed. /var/tmp joins the
// scope when it is on disk and the RAM-backed roots when it is a RAM file
// system. Every root is a canonical physical path.
func AccountedTemp() (scope, ram []string) {
	for _, r := range []string{"/tmp", "/dev/shm"} {
		if physical, err := filepath.EvalSymlinks(r); err == nil {
			ram = append(ram, physical)
		}
	}
	physical, err := filepath.EvalSymlinks(sharedTempDir)
	switch {
	case err != nil:
	case ramFileSystem(physical):
		ram = append(ram, physical)
	default:
		scope = append(scope, physical)
	}
	return scope, ram
}

// goBuildLocationKeys name the directories and files a go build reads or
// writes beyond its flags: its cache, module cache, GOPATH, work and coverage
// directories, workspace file and TMPDIR (go's work directory without
// GOTMPDIR).
var goBuildLocationKeys = []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOTMPDIR", "GOCOVERDIR", "GOWORK", "TMPDIR"}

// NestedGoKeptEnv returns a brokered run's env values of the Go environment
// its nested go calls may keep but not change (NestedGo.Env): every key
// nestedGoEnv does not let a call set freely, the build locations included,
// but the run's Go setting and GOFLAGS, which have checks of their own. Each
// is the last of its key, as exec passes it.
func NestedGoKeptEnv(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		key, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch nestedGoEnvRuleOf(key) {
		case nestedGoEnvOwn, nestedGoEnvThreads, nestedGoEnvLocation:
			out[key] = v
		}
	}
	return out
}

// lastGoEnv is the last value of key in env, as os/exec passes it.
func lastGoEnv(env []string, key string) (string, bool) {
	value, found := "", false
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, key+"="); ok {
			value, found = v, true
		}
	}
	return value, found
}

// isGoRootDir reports whether dir looks like a GOROOT, as cmd/go decides it:
// it has a pkg/tool directory.
func isGoRootDir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "pkg", "tool"))
	return err == nil && st.IsDir()
}

// GoRoot is the GOROOT a go command run with env uses: env's GOROOT when it
// is set, else the GOROOT that holds goBinary, found as cmd/go finds its own
// (two or three directories up from the binary, as given and then with
// symlinks resolved). goBinary "" means the go on env's PATH. It returns ""
// when there is no Go toolchain to mirror: no go, a GOROOT without
// pkg/tool, or a go outside any GOROOT.
func GoRoot(goBinary string, env []string) string {
	if v, ok := lastGoEnv(env, "GOROOT"); ok && v != "" {
		if v = filepath.Clean(v); filepath.IsAbs(v) && isGoRootDir(v) {
			return v
		}
		return ""
	}
	if goBinary == "" {
		if goBinary = resolveShimTool("go", "", BasePath(env)); goBinary == "" {
			return ""
		}
	}
	exe, err := filepath.Abs(goBinary)
	if err != nil {
		return ""
	}
	candidates := []string{exe}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != exe {
		candidates = append(candidates, resolved)
	}
	for _, c := range candidates {
		for _, up := range []string{"../..", "../../.."} {
			if dir := filepath.Join(c, up); isGoRootDir(dir) {
				return dir
			}
		}
	}
	return ""
}

// GoRootGap reports why a brokered run with env gets no GOROOT mirror
// although it has a go: env's GOROOT holds no toolchain, or the go on env's
// PATH sits outside any GOROOT (a version manager's shim, or a copied binary
// relying on its built-in GOROOT). go test puts its real GOROOT's bin/ first
// on its tests' PATH, so without a mirror the go and gofmt a Go test starts
// reach the real toolchain, neither refused nor accounted (FAE-HOST-014).
// goBinary is the go on env's PATH, "" for none; gap is "" when the run gets
// a mirror or has no go.
func GoRootGap(env []string) (goBinary, gap string) {
	goBinary = resolveShimTool("go", "", BasePath(env))
	if GoRoot(goBinary, env) != "" {
		return goBinary, ""
	}
	const effect = "so brokered runs get no GOROOT mirror and the go and gofmt a Go test starts reach the real toolchain, neither refused nor accounted (FAE-HOST-014)"
	if v, ok := lastGoEnv(env, "GOROOT"); ok && v != "" {
		return goBinary, "GOROOT=" + v + " holds no Go toolchain (no pkg/tool), " + effect
	}
	if goBinary == "" {
		return "", ""
	}
	return goBinary, "the go on PATH, " + goBinary + ", sits outside any GOROOT (a version manager's shim or a copied binary), " + effect + "; put a go that sits in its GOROOT's bin/, or a symlink to one, first on the daemon's PATH, or set GOROOT in each HostCommand stage's environment"
}

// goRootMirrorName is the mirror's directory under the run's parent.
const goRootMirrorName = "goroot"

// SealGoRoot makes a mirror InstallGoRoot made, and its bin/, read-only, so
// a command that runs under it cannot rename or replace an entry by mistake
// (the go and gofmt wrappers among them). It is not containment: the command
// runs as the mirror's owner and can make it writable again. RemoveGoRoot
// removes a sealed mirror.
func SealGoRoot(mirror string) error {
	if err := os.Chmod(filepath.Join(mirror, "bin"), 0500); err != nil {
		return err
	}
	return os.Chmod(mirror, 0500)
}

// RemoveGoRoot removes parent's GOROOT mirror (InstallGoRoot), sealed or not;
// a parent without one is left as it is.
func RemoveGoRoot(parent string) error {
	mirror := filepath.Join(parent, goRootMirrorName)
	for _, dir := range []string{mirror, filepath.Join(mirror, "bin")} {
		if st, err := os.Lstat(dir); err == nil && st.IsDir() {
			if err = os.Chmod(dir, 0700); err != nil {
				return err
			}
		}
	}
	return os.RemoveAll(mirror)
}

// InstallGoRoot makes parent/goroot, a mirror of the GOROOT real for one
// brokered run: a symlink for every entry of real but bin/, and a bin/ whose
// go and gofmt link to the run's wrappers in parent/command-shims
// (InstallDenials or InstallAccounting, installed after it) and whose other
// entries link to real's. The broker sets GOROOT to the mirror, so go uses
// the real toolchain, cache keys included, and starts tests with the
// wrappers first on PATH. real may itself be a mirror: a broker run inside
// another run's tests mirrors that run's mirror, and its wrappers come first.
func InstallGoRoot(parent, real string) (string, error) {
	if !filepath.IsAbs(parent) || !filepath.IsAbs(real) {
		return "", errors.New("a GOROOT mirror needs an absolute parent and GOROOT")
	}
	mirror := filepath.Join(parent, goRootMirrorName)
	if err := os.Mkdir(mirror, 0700); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(real)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if e.Name() != "bin" {
			if err = os.Symlink(filepath.Join(real, e.Name()), filepath.Join(mirror, e.Name())); err != nil {
				return "", err
			}
		}
	}
	bin := filepath.Join(mirror, "bin")
	if err = os.Mkdir(bin, 0700); err != nil {
		return "", err
	}
	tools, err := os.ReadDir(filepath.Join(real, "bin"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, e := range tools {
		if e.Name() != "go" && e.Name() != "gofmt" {
			if err = os.Symlink(filepath.Join(real, "bin", e.Name()), filepath.Join(bin, e.Name())); err != nil {
				return "", err
			}
		}
	}
	for _, name := range []string{"go", "gofmt"} {
		if err = os.Symlink(filepath.Join(parent, "command-shims", name), filepath.Join(bin, name)); err != nil {
			return "", err
		}
	}
	return mirror, nil
}

// mirrorsGoRoot reports whether goroot is mirror or a mirror that a broker
// running inside the run made of it (InstallGoRoot), following each mirror's
// pkg link back a few levels.
func mirrorsGoRoot(goroot, mirror string) bool {
	g, mirror := filepath.Clean(goroot), filepath.Clean(mirror)
	for range 8 {
		if g == mirror {
			return true
		}
		target, err := os.Readlink(filepath.Join(g, "pkg"))
		if err != nil || !filepath.IsAbs(target) || filepath.Base(target) != "pkg" {
			return false
		}
		g = filepath.Dir(target)
	}
	return false
}

// GoRunEnv is env with a brokered run's Go setting (FAE-HOST-014):
// GOROOT=goroot when goroot is set, GOTOOLCHAIN=local, since a switched
// toolchain puts its own bin/ first again, and GOENV=off, so no Go env file
// adds flags the nested-command policy never sees. Earlier values of those
// keys are dropped (GOROOT only when goroot replaces it).
func GoRunEnv(env []string, goroot string) []string {
	out := make([]string, 0, len(env)+3)
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if key == "GOTOOLCHAIN" || key == "GOENV" || key == "GOROOT" && goroot != "" {
			continue
		}
		out = append(out, kv)
	}
	if goroot != "" {
		out = append(out, "GOROOT="+goroot)
	}
	return append(out, "GOTOOLCHAIN=local", "GOENV=off")
}

// goRootMirror is g's GOROOT mirror, "" for none.
func (g *NestedGo) goRootMirror() string {
	if g == nil {
		return ""
	}
	return g.GoRoot
}

// tempRoots is g's on-disk temporary roots, none for no g.
func (g *NestedGo) tempRoots() []string {
	if g == nil {
		return nil
	}
	return g.Temp
}
