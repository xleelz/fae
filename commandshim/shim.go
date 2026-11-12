// Package commandshim implements the trusted broker's command policy. PATH
// wrappers refuse nested commands by default, or, opt-in, pass them through
// the nested-command policy and account them under the parent's lease
// (InstallAccounting, PrepareAccounted); they are not an OS sandbox.
package commandshim

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

const Refused = 77

// LocalModeRefused is the distinct exit code of a Local-mode git push or
// remote change (FAE-MODE-002); every other policy refusal stays 77.
const LocalModeRefused = 78

type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return "command refused (exit 77): " + r.Reason }
func refuse(s string) error      { return &Refusal{s} }

// LocalRefusal is a Local-mode refusal of an external VCS effect.
type LocalRefusal struct{ Reason string }

func (r *LocalRefusal) Error() string { return "command refused in Local mode (exit 78): " + r.Reason }

// InstallDenials installs immutable-by-convention wrappers in a private session
// directory. Trusted broker entry executes its resolved binary directly; children
// must use these wrappers and cannot silently inherit a parent's resource slot.
func InstallDenials(parent string) (string, error) {
	dir := filepath.Join(parent, "command-shims")
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	for _, name := range ShimNames {
		script := "#!/bin/sh\nprintf '%s\\n' 'fae: command refused (exit 77): role tools and nested heavy commands require a separate broker dispatch; use a scoped host-command stage' >&2\nexit 77\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0500); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func PrependPath(env []string, dir string) []string {
	result := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, "PATH=") {
			result = append(result, e)
		}
	}
	return append(result, "PATH="+dir+string(os.PathListSeparator)+BasePath(env))
}

// BasePath is the effective PATH of env (the last entry wins), or /usr/bin:/bin.
func BasePath(env []string) string {
	path := "/usr/bin:/bin"
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			path = strings.TrimPrefix(e, "PATH=")
		}
	}
	return path
}

type Policy struct {
	Worktree, CWD               string
	CodeRole, LeaseHeld, Nested bool
	Local                       bool
	// RAMRoots must be canonical physical roots. Output beneath them is denied.
	RAMRoots []string
	// Temp, in account mode, are on-disk temporary roots (canonical physical
	// paths) a nested go or gofmt may also use, as the scratch root
	// (AccountedTemp: /var/tmp, where test suites keep short-path fixtures).
	Temp []string
	// Scratch, when set, is a second root a path may resolve into: an
	// account-mode run's on-disk TMPDIR, where its tests keep their fixtures
	// (PrepareAccounted). Prepare's own calls never set it.
	Scratch string
}

func inside(root, p string) bool {
	r, e := filepath.Rel(root, p)
	return e == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator))
}
func resolve(root, p string) (string, error) {
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	p = filepath.Clean(p)
	// Resolve every existing ancestor, including a symlink leading to a missing file.
	suffix := []string{}
	cur := p
	for {
		v, e := filepath.EvalSymlinks(cur)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				v = filepath.Join(v, suffix[i])
			}
			return v, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", fmt.Errorf("unresolvable path %s", p)
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}
func (p Policy) scoped(name string) (string, error) {
	v, e := resolve(p.CWD, name)
	if e != nil {
		return "", e
	}
	if !p.inScope(v) {
		return "", refuse("target outside own worktree: " + name)
	}
	return v, nil
}

// inScope reports whether the resolved path v is inside the worktree, the
// scratch root, when there is one, or a Temp root.
func (p Policy) inScope(v string) bool {
	return inside(p.Worktree, v) || p.Scratch != "" && inside(p.Scratch, v) || slices.ContainsFunc(p.Temp, func(r string) bool { return inside(r, v) })
}

// buildOutput resolves a build's -o value: inside the scope and never under a
// RAM-backed root (FAE-HOST-020).
func (p Policy) buildOutput(v string) (string, error) {
	dest, e := p.scoped(v)
	if e != nil {
		return "", e
	}
	ram, e := p.ramBacked(dest)
	if e != nil {
		return "", e
	}
	if ram {
		return "", refuse("RAM-backed temporary build output: " + dest)
	}
	return dest, nil
}

// ramBacked reports whether the resolved path dest is under one of the
// policy's RAM-backed roots.
func (p Policy) ramBacked(dest string) (bool, error) {
	for _, r := range p.RAMRoots {
		physical, e := resolve("/", r)
		if e != nil {
			return false, e
		}
		if inside(physical, dest) {
			return true, nil
		}
	}
	return false, nil
}

// Prepare validates the supported Go command grammar and injects hard parallelism
// caps. Unsupported build-tool grammars fail closed rather than guessing flags.
func (p Policy) Prepare(argv []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, refuse("empty command")
	}
	root, e := filepath.EvalSymlinks(p.Worktree)
	if e != nil {
		return nil, e
	}
	cwd, e := filepath.EvalSymlinks(p.CWD)
	if e != nil {
		return nil, e
	}
	p.Worktree = root
	p.CWD = cwd
	scope := inside(root, cwd)
	if !CanExecute(p.CodeRole, scope, true, p.LeaseHeld, p.Nested) {
		return nil, refuse("code role, own worktree, a heavy lease and non-nested dispatch are required")
	}
	args := append([]string(nil), argv...)
	name := filepath.Base(args[0])
	if out, handled, err := p.prepareLanguage(args, nil); handled {
		return out, err
	}
	switch name {
	case "git":
		if p.Local {
			return nil, refuse("VCS commands in role command paths are disabled in Local mode; use the dedicated VCS broker")
		}
		return nil, refuse("use the dedicated VCS broker")
	case "cargo", "rustc", "rustfmt", "make", "cmake", "ninja", "pytest":
		return nil, refuse("unsupported build wrapper requires a scoped broker adapter (including version probes)")
	case "gofmt":
		return p.prepareFormat(args)
	case "go":
		if len(args) < 2 || (args[1] != "test" && args[1] != "build" && args[1] != "vet") {
			return nil, refuse("unsupported Go verb; probes also require broker admission")
		}
		out := args[:2]
		targets := 0
		recursive, filtered, run := false, false, ""
		for i := 2; i < len(args); i++ {
			a := args[i]
			if targets > 0 && strings.HasPrefix(a, "-") {
				return nil, refuse("flags must precede package targets")
			}
			if a == "-p" || a == "-parallel" || a == "-test.parallel" {
				if i+1 >= len(args) {
					return nil, refuse("missing parallelism value")
				}
				i++
				continue
			}
			if strings.HasPrefix(a, "-p=") || strings.HasPrefix(a, "-parallel=") || strings.HasPrefix(a, "-test.parallel=") {
				continue
			}
			if a == "-o" || strings.HasPrefix(a, "-o=") {
				v := strings.TrimPrefix(a, "-o=")
				if a == "-o" {
					i++
					if i >= len(args) {
						return nil, refuse("missing output path")
					}
					v = args[i]
				}
				dest, e := p.buildOutput(v)
				if e != nil {
					return nil, e
				}
				out = append(out, "-o", dest)
				continue
			}
			if strings.HasPrefix(a, "-") {
				if targets > 0 {
					return nil, refuse("flags must precede package targets")
				}
				if a == "-json" || a == "-a" || a == "-x" || a == "-race" || a == "-v" || a == "-short" || a == "-failfast" || strings.HasPrefix(a, "-count=") || strings.HasPrefix(a, "-run=") || strings.HasPrefix(a, "-timeout=") {
					if v, ok := strings.CutPrefix(a, "-run="); ok {
						filtered, run = true, v // go test keeps the last -run
					}
					out = append(out, a)
					continue
				}
				return nil, refuse("unsupported Go flag: " + a)
			}
			recursive = recursive || strings.HasSuffix(a, "/...")
			path := strings.TrimSuffix(a, "/...")
			if !strings.HasPrefix(path, ".") && !filepath.IsAbs(path) {
				return nil, refuse("target must be an explicit own-worktree path")
			}
			if _, e := p.scoped(path); e != nil {
				return nil, e
			}
			targets++
			out = append(out, a)
		}
		if targets == 0 {
			out = append(out, ".")
		}
		// Flags must precede positional package arguments for Go's parser. A
		// whole-suite test run (recursive targets, and no -run filter or one
		// that selects every test, goTestRunSelectsEveryTest) uses one thread
		// (FAE-HOST-015); other runs and builds are clamped to 2.
		threads := "2"
		if args[1] == "test" && recursive && (!filtered || goTestRunSelectsEveryTest(run)) {
			threads = "1"
		}
		flags := []string{out[0], out[1], "-p=" + threads}
		if args[1] == "test" {
			flags = append(flags, "-parallel="+threads)
		}
		return append(flags, out[2:]...), nil
	default:
		return args, nil // Unknown vectors are separately literal-allowlisted by the trusted broker.
	}
}

// prepareFormat expands a gofmt vector's targets to explicit, bounded Go files
// (FAE-HOST-012); standard input and any flag but -l, -w, -d and -s are refused.
func (p Policy) prepareFormat(args []string) ([]string, error) {
	out := []string{args[0]}
	targets := 0
	for _, a := range args[1:] {
		if a == "-w" || a == "-l" || a == "-d" || a == "-s" {
			out = append(out, a)
			continue
		}
		if strings.HasPrefix(a, "-") {
			return nil, refuse("unsupported formatter flag")
		}
		v, e := p.scoped(a)
		if e != nil {
			return nil, e
		}
		files, e := FormatTargets(v)
		if e != nil {
			return nil, e
		}
		out = append(out, files...)
		targets += len(files)
	}
	if targets == 0 {
		return nil, refuse("formatter requires explicit per-file targets")
	}
	return out, nil
}

// gitExternalSubcommands push or change remotes: no engine run or role
// session makes that external effect through git (FAE-MODE-002, FAE-EXT-001).
// send-pack and http-push are push's plumbing.
var gitExternalSubcommands = map[string]bool{"push": true, "remote": true, "send-pack": true, "http-push": true}

// nestedGoFlagRule is how the nested-command policy treats one go flag
// (PrepareAccounted); a flag nestedGoFlags does not list is refused.
type nestedGoFlagRule uint8

const (
	nestedGoBool    nestedGoFlagRule = iota + 1 // allowed; takes no separate value
	nestedGoValue                               // allowed; takes a value
	nestedGoClamp                               // -p, -parallel: dropped for the clamp
	nestedGoOutput                              // -o: made absolute; inside the scope, never RAM-backed
	nestedGoPath                                // a file or directory: inside the scope, never RAM-backed
	nestedGoProfile                             // a test profile: a path relative to -outputdir
	nestedGoPGO                                 // -pgo: auto, off or a path
	nestedGoTools                               // -gcflags, -ldflags, -asmflags: listed tool flags only
	nestedGoChoice                              // -buildmode, -compiler: checked values
	nestedGoCPU                                 // -cpu: each GOMAXPROCS value at most the call's clamp
)

// nestedGoFlags is the allowlist of flags a nested go build, test, vet or
// list may pass, as an argument or in GOFLAGS, by name without its dashes (a
// go test flag also with its test. prefix): go's build flags but -C,
// -toolexec, -gccgoflags and -linkshared; go test's but -exec; go list's;
// go vet's but -vettool, and its analyzers' (go tool vet -flags).
var nestedGoFlags = map[string]nestedGoFlagRule{
	// go build's flags, which test, vet and list share.
	"a": nestedGoBool, "n": nestedGoBool, "x": nestedGoBool, "v": nestedGoBool, "asan": nestedGoBool, "msan": nestedGoBool,
	"race": nestedGoBool, "cover": nestedGoBool, "buildvcs": nestedGoBool, "json": nestedGoBool, "modcacherw": nestedGoBool,
	"trimpath": nestedGoBool, "work": nestedGoBool,
	"covermode": nestedGoValue, "coverpkg": nestedGoValue, "installsuffix": nestedGoValue, "mod": nestedGoValue, "tags": nestedGoValue,
	"p": nestedGoClamp, "o": nestedGoOutput, "modfile": nestedGoPath, "overlay": nestedGoPath, "pkgdir": nestedGoPath,
	"debug-actiongraph": nestedGoPath, "debug-runtime-trace": nestedGoPath, "debug-trace": nestedGoPath, "pgo": nestedGoPGO,
	"gcflags": nestedGoTools, "ldflags": nestedGoTools, "asmflags": nestedGoTools, "buildmode": nestedGoChoice, "compiler": nestedGoChoice,
	// go test's, and the test binary's that go test passes on.
	"c": nestedGoBool, "artifacts": nestedGoBool, "benchmem": nestedGoBool, "failfast": nestedGoBool, "fullpath": nestedGoBool,
	"short": nestedGoBool, "vet": nestedGoValue, "bench": nestedGoValue, "benchtime": nestedGoValue, "blockprofilerate": nestedGoValue,
	"count": nestedGoValue, "cpu": nestedGoCPU, "fuzz": nestedGoValue, "fuzzminimizetime": nestedGoValue, "fuzztime": nestedGoValue,
	"list": nestedGoValue, "memprofilerate": nestedGoValue, "mutexprofilefraction": nestedGoValue, "run": nestedGoValue,
	"shuffle": nestedGoValue, "skip": nestedGoValue, "timeout": nestedGoValue, "parallel": nestedGoClamp, "outputdir": nestedGoPath,
	"coverprofile": nestedGoProfile, "cpuprofile": nestedGoProfile, "memprofile": nestedGoProfile, "blockprofile": nestedGoProfile,
	"mutexprofile": nestedGoProfile, "trace": nestedGoProfile,
	// go list's.
	"compiled": nestedGoBool, "deps": nestedGoBool, "e": nestedGoBool, "export": nestedGoBool, "find": nestedGoBool, "m": nestedGoBool,
	"retracted": nestedGoBool, "test": nestedGoBool, "u": nestedGoBool, "versions": nestedGoBool, "f": nestedGoValue, "reuse": nestedGoPath,
	// go vet's and its analyzers'.
	"diff": nestedGoBool, "all": nestedGoBool, "appends": nestedGoBool, "asmdecl": nestedGoBool, "assign": nestedGoBool,
	"atomic": nestedGoBool, "bool": nestedGoBool, "bools": nestedGoBool, "buildtag": nestedGoBool, "buildtags": nestedGoBool,
	"cgocall": nestedGoBool, "composites": nestedGoBool, "composites.whitelist": nestedGoBool, "compositewhitelist": nestedGoBool,
	"copylocks": nestedGoBool, "defers": nestedGoBool, "directive": nestedGoBool, "errorsas": nestedGoBool, "framepointer": nestedGoBool,
	"hostport": nestedGoBool, "httpresponse": nestedGoBool, "ifaceassert": nestedGoBool, "loopclosure": nestedGoBool,
	"lostcancel": nestedGoBool, "methods": nestedGoBool, "nilfunc": nestedGoBool, "printf": nestedGoBool, "rangeloops": nestedGoBool,
	"shift": nestedGoBool, "sigchanyzer": nestedGoBool, "slog": nestedGoBool, "stdmethods": nestedGoBool, "stdversion": nestedGoBool,
	"stringintconv": nestedGoBool, "structtag": nestedGoBool, "testinggoroutine": nestedGoBool, "testinggoroutine.subtest": nestedGoBool,
	"tests": nestedGoBool, "timeformat": nestedGoBool, "unmarshal": nestedGoBool, "unreachable": nestedGoBool, "unsafeptr": nestedGoBool,
	"unusedresult": nestedGoBool, "waitgroup": nestedGoBool, "printf.funcs": nestedGoValue, "printfuncs": nestedGoValue,
	"unusedfuncs": nestedGoValue, "unusedresult.funcs": nestedGoValue, "unusedresult.stringmethods": nestedGoValue,
	"unusedstringmethods": nestedGoValue,
}

// nestedGoToolFlags are the compiler, linker and assembler flags a nested
// -gcflags, -ldflags or -asmflags may pass. None runs another program (the
// linker's -extld, -extar and -extldflags are not listed, nor -I and -r,
// which name the dynamic linker and library path the built binary, a test
// binary included, starts with) or writes a file of its own (-o, -tmpdir,
// -asmhdr, -json, the profile flags and the like); the compiler's and
// assembler's -d debug settings are not listed either. -linkmode=external
// runs the run's own C compiler as the linker (accountedGoEnv keeps CC).
var nestedGoToolFlags = map[string]map[string]bool{
	"gcflags": {"B": true, "C": true, "K": true, "L": true, "N": true, "S": true, "V": true, "W": true, "asan": true, "clobberdead": true,
		"clobberdeadreg": true, "dwarf": true, "dwarfbasentries": true, "dwarflocationlists": true, "e": true, "errorurl": true,
		"gendwarfinl": true, "h": true, "j": true, "l": true, "live": true, "m": true, "msan": true, "r": true, "race": true,
		"smallframes": true, "spectre": true, "t": true, "v": true, "w": true, "wb": true},
	"ldflags": {"B": true, "E": true, "H": true, "L": true, "R": true, "T": true, "V": true, "X": true, "a": true, "asan": true,
		"aslr": true, "bindnow": true, "buildid": true, "checklinkname": true, "compressdwarf": true, "d": true, "debugtramp": true,
		"dumpdep": true, "e": true, "f": true, "funcalign": true, "g": true, "h": true, "installsuffix": true, "k": true, "linkmode": true,
		"msan": true, "n": true, "pluginpath": true, "pruneweakmap": true, "race": true, "randlayout": true, "s": true,
		"strictdups": true, "v": true, "w": true},
	"asmflags": {"D": true, "I": true, "S": true, "V": true, "debug": true, "e": true, "spectre": true, "v": true},
}

// goFlagName splits a go flag argument into its name (without its one or two
// leading dashes) and an attached =value.
func goFlagName(a string) (name, value string, attached bool) {
	name, value, attached = strings.Cut(strings.TrimPrefix(strings.TrimPrefix(a, "-"), "-"), "=")
	return name, value, attached
}

// accountedGoFlag refuses a go flag that runs other programs around the
// toolchain or the tests (-toolexec, -exec, -vettool) or moves the command
// out of its directory (-C), whether given as an argument or in GOFLAGS.
func accountedGoFlag(name string) error {
	if name == "toolexec" || name == "exec" || name == "vettool" || name == "C" {
		return refuse("nested go flag -" + name + " runs other programs or leaves the run's directories")
	}
	return nil
}

// unsupportedNestedGoFlag refuses a go flag, argument or GOFLAGS entry, that
// nestedGoFlags does not list.
func unsupportedNestedGoFlag(a string) error {
	return refuse("unsupported nested go flag " + a + "; nested calls may pass only the go build, test, vet and list flags the policy lists")
}

// goQuotedFields splits s as go splits GOFLAGS and a -gcflags, -ldflags or
// -asmflags value (cmd/internal/quoted.Split): at spaces, tabs and newlines,
// with a field that starts with a single or double quote running to the
// next such quote, unescaped.
func goQuotedFields(s string) ([]string, error) {
	var fields []string
	space := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
	for len(s) > 0 {
		for len(s) > 0 && space(s[0]) {
			s = s[1:]
		}
		if len(s) == 0 {
			break
		}
		if q := s[0]; q == '"' || q == '\'' {
			end := strings.IndexByte(s[1:], q)
			if end < 0 {
				return nil, fmt.Errorf("unterminated %c string", q)
			}
			fields, s = append(fields, s[1:1+end]), s[2+end:]
			continue
		}
		i := 0
		for i < len(s) && !space(s[i]) {
			i++
		}
		fields, s = append(fields, s[:i]), s[i:]
	}
	return fields, nil
}

// nestedGoToolValue refuses a nested -gcflags, -ldflags or -asmflags value
// (an optional pattern= prefix, then flags split as go splits them) that
// passes a tool flag nestedGoToolFlags does not list, or that go could not
// split.
func nestedGoToolValue(name, value string) error {
	v := strings.TrimSpace(value)
	if v != "" && v[0] != '-' {
		_, v, _ = strings.Cut(v, "=") // go refuses a pattern without =
	}
	bad := ""
	fields, err := goQuotedFields(v)
	if err != nil {
		bad = value
	}
	for _, f := range fields {
		if tool, _, _ := goFlagName(f); bad == "" && strings.HasPrefix(f, "-") && !nestedGoToolFlags[name][tool] {
			bad = f
		}
	}
	if bad != "" {
		return refuse("nested go -" + name + " passes " + bad + ", which may run another program or write a file; nested calls may pass only listed compiler, linker and assembler flags")
	}
	return nil
}

// nestedGoProfiles collects a nested go test's profile paths, which are
// relative to -outputdir, else to the go command's directory, and its -cpu
// lists (-test.cpu after -args too), checked once the call's clamp is known.
type nestedGoProfiles struct {
	outputDir string
	paths     []string
	cpus      []string
}

// nestedGoValue checks the value of a nested go flag with rule: a path inside
// the scope and never under a RAM-backed root, -pgo's profile, a tool flag
// list, -buildmode and -compiler. Test profiles are collected in profiles,
// checked once -outputdir is known, and -cpu lists once the clamp is.
func (p Policy) nestedGoValue(name string, rule nestedGoFlagRule, value string, profiles *nestedGoProfiles) error {
	switch rule {
	case nestedGoOutput, nestedGoPath:
		if _, e := p.buildOutput(value); e != nil {
			return e
		}
		if name == "outputdir" {
			profiles.outputDir = value
			if !filepath.IsAbs(value) {
				profiles.outputDir = filepath.Join(p.CWD, value)
			}
		}
	case nestedGoProfile:
		profiles.paths = append(profiles.paths, value)
	case nestedGoCPU:
		profiles.cpus = append(profiles.cpus, value)
	case nestedGoPGO:
		if value != "auto" && value != "off" {
			_, e := p.buildOutput(value)
			return e
		}
	case nestedGoTools:
		return nestedGoToolValue(name, value)
	case nestedGoChoice:
		if name == "buildmode" && value == "shared" || name == "compiler" && value != "gc" {
			return refuse("nested go -" + name + "=" + value + " is refused; nested builds use the gc compiler and install no shared libraries")
		}
	}
	return nil
}

// nestedGoTestBinaryFlags are the test binary's own flags (-test.NAME) a
// nested go test may pass after -args, where go test hands them to the test
// binary without reading them: those that write no file, by name, true for
// one that takes a separate value. -test.parallel is dropped for the clamp
// and -test.cpu checked against it. The profile and output flags
// (-test.coverprofile, -test.outputdir, ...), which go test's own forms
// before -args give with their paths checked, go test's plumbing
// (-test.gocoverdir, -test.testlogfile, -test.fuzzcachedir,
// -test.fuzzworker) and any other -test. flag are refused there.
var nestedGoTestBinaryFlags = map[string]bool{
	"bench": true, "benchmem": false, "benchtime": true, "blockprofilerate": true, "count": true, "cpu": true, "failfast": false,
	"fullpath": false, "fuzz": true, "fuzzminimizetime": true, "fuzztime": true, "list": true, "memprofilerate": true,
	"mutexprofilefraction": true, "paniconexit0": false, "parallel": true, "run": true, "short": false, "shuffle": true, "skip": true,
	"timeout": true, "v": false,
}

// nestedGoReservedPatterns are go's reserved package patterns (go help
// packages). Each expands to many packages, so a nested target naming one,
// like one with "...", is recursive: a nested go test of it without a -run
// filter runs as a whole suite, one package and one test at a time.
var nestedGoReservedPatterns = map[string]bool{"all": true, "cmd": true, "std": true, "tool": true, "work": true}

// nestedGoCPUList refuses a nested go test's -cpu list (GOMAXPROCS values
// the tests run at, one pass each) with a value above the call's clamp: the
// top-level policy never runs a test at more threads than that.
func nestedGoCPUList(list string, threads int) error {
	for _, f := range strings.Split(list, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		if n, err := strconv.Atoi(f); err != nil || n < 1 || n > threads {
			return refuse("nested go test -cpu " + list + " runs tests at more threads than the call's clamp, -p=" + strconv.Itoa(threads) + "; nested calls may list only values from 1 to the clamp")
		}
	}
	return nil
}

// PrepareAccounted is the policy of a nested call in an account-mode engine
// run (FAE-HOST-014's accounted branch): the call passes through this shim and
// runs under the parent run's lease instead of taking one. env is the call's
// environment. The scope is the run's worktree, its Scratch directory and, for
// go and gofmt, its Temp roots.
//
//   - git: prepareAccountedGit (accounted_git.go). A call may change only
//     repositories in the run's directories or a temporary directory, never the
//     run's own; a push or remote change exits 78 for a Local task.
//   - go build, test, vet and list run from inside the scope, with Prepare's
//     clamp: -p=2 (and -parallel=2 for test), or 1 for a whole-suite test run
//     (recursive targets, no -run filter; FAE-HOST-015); the call's own -p,
//     -parallel and -test.parallel are dropped wherever they appear, and
//     -cpu (-test.cpu) may list no value above the clamp. A target with
//     "..." or a reserved pattern (all, std, cmd, work, tool) is recursive.
//     Path targets must resolve inside the scope; import-path targets are
//     left to go. Their flags are an allowlist (nestedGoFlags), in the
//     arguments and in GOFLAGS: every path flag resolves inside the scope and
//     never under a RAM-backed root (FAE-HOST-020), -gcflags, -ldflags and
//     -asmflags pass only listed tool flags (nestedGoToolFlags), -compiler is
//     gc and -buildmode never shared; -toolexec, -exec, -vettool and -C are
//     refused by name. After -args, a -test. flag must be one that writes no
//     file (nestedGoTestBinaryFlags); the test's own flags pass. RunShim then
//     applies accountedGoEnv: the run's GOROOT mirror, GOTOOLCHAIN=local,
//     GOENV=off, no RAM-backed build location, and an environment allowlist
//     (nestedGoEnv) that keeps the programs go runs the run's own.
//   - go version and go env are read-only probes and run as called; go env -w
//     and -u are refused. Every other go verb is refused.
//   - gofmt gets Prepare's formatter limits (FAE-HOST-012) inside the scope.
//   - Every other tool is refused, as Prepare refuses it.
func (p Policy) PrepareAccounted(argv, env []string) ([]string, error) {
	if len(argv) == 0 {
		return nil, refuse("empty command")
	}
	var e error
	if p.Worktree, e = filepath.EvalSymlinks(p.Worktree); e != nil {
		return nil, e
	}
	if p.Scratch != "" {
		if p.Scratch, e = filepath.EvalSymlinks(p.Scratch); e != nil {
			return nil, e
		}
	}
	if p.CWD, e = filepath.EvalSymlinks(p.CWD); e != nil {
		return nil, e
	}
	args := append([]string(nil), argv...)
	accountedPolicy := p
	accountedPolicy.CodeRole, accountedPolicy.LeaseHeld = true, true
	if out, handled, err := accountedPolicy.prepareLanguage(args, env); handled {
		return out, err
	}
	switch filepath.Base(args[0]) {
	case "git":
		return p.prepareAccountedGit(args, env)
	case "gofmt":
		if !p.inScope(p.CWD) {
			return nil, refuse("nested build, test or format outside the run's worktree and scratch directory: " + p.CWD)
		}
		return p.prepareFormat(args)
	case "go":
		return p.prepareAccountedGo(args, env)
	}
	return nil, refuse("unsupported build wrapper requires a scoped broker adapter (including version probes)")
}

// prepareAccountedGo is PrepareAccounted's go branch.
func (p Policy) prepareAccountedGo(args, env []string) ([]string, error) {
	verb := ""
	if len(args) > 1 {
		verb = args[1]
	}
	build := verb == "build" || verb == "test" || verb == "vet" || verb == "list"
	profiles := &nestedGoProfiles{outputDir: p.CWD}
	for _, kv := range env {
		value, ok := strings.CutPrefix(kv, "GOFLAGS=")
		if !ok {
			continue
		}
		fields, err := goQuotedFields(value)
		if err != nil {
			return nil, unsupportedNestedGoFlag(kv)
		}
		for _, f := range fields {
			name, v, attached := goFlagName(f)
			if e := accountedGoFlag(name); e != nil {
				return nil, e
			}
			bare := strings.TrimPrefix(name, "test.")
			rule := nestedGoFlags[bare]
			if rule == 0 || !strings.HasPrefix(f, "-") {
				return nil, unsupportedNestedGoFlag(f)
			}
			// go takes a GOFLAGS value only as -name=value, and applies
			// build flags only to build verbs.
			if build && attached {
				if e := p.nestedGoValue(bare, rule, v, profiles); e != nil {
					return nil, e
				}
			}
		}
	}
	switch verb {
	case "version", "env":
		for _, a := range args[2:] {
			if name, _, _ := goFlagName(a); strings.HasPrefix(a, "-") && (name == "w" || name == "u") {
				return nil, refuse("nested go env -w and -u change the user's Go environment; nested calls may only read it")
			}
		}
		return args, nil
	case "build", "test", "vet", "list":
	default:
		return nil, refuse("unsupported nested Go verb " + strconv.Quote(verb) + "; nested calls may run go build, test, vet, list, version and env")
	}
	if !p.inScope(p.CWD) {
		return nil, refuse("nested build, test or format outside the run's worktree and scratch directory: " + p.CWD)
	}
	out := args[:2:2]
	recursive, filtered, testArgs, testFlags := false, false, false, false
	run := "" // the last -run (-test.run) value, as go test and the test binary keep it
	for i := 2; i < len(args); i++ {
		a := args[i]
		name, value, attached := goFlagName(a)
		bare := strings.TrimPrefix(name, "test.")
		switch {
		case testArgs: // after -args: the test binary's arguments
			// The test binary's flags end at "--"; the test's own flags,
			// which the policy cannot read, pass as called.
			if testFlags = testFlags && a != "--"; !testFlags || !strings.HasPrefix(a, "-") || a == "-" || bare == name {
				break
			}
			separate, listed := nestedGoTestBinaryFlags[bare]
			if !listed {
				return nil, refuse("nested go test passes " + a + " to the test binary after -args, unchecked; nested calls may pass there only test binary flags that write no file (give a profile or output flag before -args, where its path is checked)")
			}
			consumed := separate && !attached && i+1 < len(args)
			if consumed {
				i++
				value = args[i]
			}
			switch bare {
			case "parallel":
				continue
			case "cpu":
				profiles.cpus = append(profiles.cpus, value)
			case "run":
				// The test binary gets go test's own -test.run first and
				// these after it, so this one is the run's filter.
				filtered, run = true, value
			}
			if consumed {
				out = append(out, a)
				a = value
			}
		case a == "-args" || a == "--args":
			testArgs, testFlags = true, true
		case !strings.HasPrefix(a, "-") || a == "-":
			recursive = recursive || strings.Contains(a, "...") || nestedGoReservedPatterns[a]
			if path := strings.TrimSuffix(a, "/..."); strings.HasPrefix(a, ".") || filepath.IsAbs(a) {
				if path == "" {
					path = "/"
				}
				if _, e := p.scoped(path); e != nil {
					return nil, e
				}
			}
		default:
			if e := accountedGoFlag(name); e != nil {
				return nil, e
			}
			rule := nestedGoFlags[bare]
			if rule == 0 {
				return nil, unsupportedNestedGoFlag(a)
			}
			if rule == nestedGoBool {
				break
			}
			if !attached {
				if i+1 >= len(args) {
					switch rule {
					case nestedGoClamp:
						return nil, refuse("missing parallelism value")
					case nestedGoOutput:
						return nil, refuse("missing output path")
					}
					break // go reports the missing value
				}
				i++
				value = args[i]
			}
			switch rule {
			case nestedGoClamp:
				continue
			case nestedGoOutput:
				dest, e := p.buildOutput(value)
				if e != nil {
					return nil, e
				}
				out = append(out, "-o", dest)
				continue
			}
			if e := p.nestedGoValue(bare, rule, value, profiles); e != nil {
				return nil, e
			}
			if bare == "run" {
				filtered, run = true, value
			}
			if !attached {
				out = append(out, a)
				a = value
			}
		}
		out = append(out, a)
	}
	for _, v := range profiles.paths {
		if !filepath.IsAbs(v) {
			v = filepath.Join(profiles.outputDir, v)
		}
		if _, e := p.buildOutput(v); e != nil {
			return nil, e
		}
	}
	threads := nestedGoThreads
	if verb == "test" && recursive && (!filtered || goTestRunSelectsEveryTest(run)) {
		threads = 1
	}
	if verb == "test" {
		for _, list := range profiles.cpus {
			if e := nestedGoCPUList(list, threads); e != nil {
				return nil, e
			}
		}
	}
	flags := []string{out[0], verb, "-p=" + strconv.Itoa(threads)}
	if verb == "test" {
		flags = append(flags, "-parallel="+strconv.Itoa(threads))
	}
	return append(flags, out[2:]...), nil
}

// nestedGoThreads is the clamp of a nested go call that is not a whole-suite
// test run (Prepare's -p=2, -parallel=2).
const nestedGoThreads = 2

// goTestNameProbes are top-level test function names, as unlike each other
// as Go's rule for them allows (Test alone, or Test and a suffix that does
// not start with a lower-case letter): digits, underscores, upper-case ASCII
// and non-ASCII letters, a title-case letter, ideographs, a repeated Test and
// a long name. A pattern that every real test name matches matches each of
// them; one that matches each of them but misses some real name only makes a
// narrower run use one thread, the stricter clamp.
var goTestNameProbes = []string{
	"Test", "TestA", "TestZ", "Test_", "Test0", "Test9", "Test_x", "TestX_y9", "Test_Test", "TestTest",
	"TestÄö", "TestΣσ", "Testǅ", "Test日本", "TestHTTP2Server", "Test" + strings.Repeat("Q", 120),
}

// goTestPatternSelectsEveryName reports whether pattern, the top-level
// element of one -run alternative, selects every test function: it compiles
// as Go's regexp package compiles it (go test refuses one that does not) and
// matches, anywhere in the name as go test matches, every goTestNameProbes
// name. The empty pattern, any pattern matching the empty string (".*",
// "x*", "(?:)", "(X|.*)"), and patterns every name contains ("Test", "^T",
// "e") all do.
func goTestPatternSelectsEveryName(pattern string) bool {
	re, err := regexp.Compile(pattern)
	if err != nil {
		return false
	}
	for _, name := range goTestNameProbes {
		if !re.MatchString(name) {
			return false
		}
	}
	return true
}

// goTestRunSelectsEveryTest reports whether a go test -run value selects
// every top-level test, so that a recursive run with it is a whole suite
// (FAE-HOST-015). The value is split as the testing package splits it: into
// alternatives at each "|" outside brackets and parentheses, then each into
// one pattern per subtest level at each such "/". It selects every test when
// one alternative's top-level pattern does (goTestPatternSelectsEveryName); a
// subtest pattern after it narrows only subtests, which still run inside
// every top-level test.
func goTestRunSelectsEveryTest(run string) bool {
	brackets, parens, start := 0, 0, 0
	first := true // still in the current alternative's top-level pattern
	for i := 0; i <= len(run); i++ {
		c := byte('|')
		if i < len(run) {
			c = run[i]
		}
		switch {
		case i < len(run) && c == '\\':
			i++
		case i < len(run) && c == '[':
			brackets++
		case i < len(run) && c == ']':
			if brackets > 0 {
				brackets--
			}
		case i < len(run) && c == '(' && brackets == 0:
			parens++
		case i < len(run) && c == ')' && brackets == 0:
			parens--
		case brackets == 0 && parens == 0 && (c == '/' || c == '|'):
			if first && goTestPatternSelectsEveryName(run[start:i]) {
				return true
			}
			first, start = c == '|', i+1
		}
	}
	return false
}

// accountedGoThreads is the clamp PrepareAccounted put first in argv's flags
// (-p=N), or nestedGoThreads for a probe (go version, go env), which has none.
func accountedGoThreads(argv []string) int {
	if len(argv) > 2 {
		if v, ok := strings.CutPrefix(argv[2], "-p="); ok {
			if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= nestedGoThreads {
				return n
			}
		}
	}
	return nestedGoThreads
}

// nestedGoEnvRule is how account mode treats one key of a nested go call's
// environment (accountedGoEnv).
type nestedGoEnvRule uint8

const (
	nestedGoEnvFree     nestedGoEnvRule = iota + 1 // any value
	nestedGoEnvOwn                                 // the run's own value, or unset or empty (go's default)
	nestedGoEnvThreads                             // the run's own, or a thread count at most the call's clamp
	nestedGoEnvLocation                            // a Go build location (accountedGoLocations)
	nestedGoEnvSetting                             // the run's Go setting: GOROOT, GOTOOLCHAIN, GOENV
	nestedGoEnvFlags                               // GOFLAGS, checked flag by flag (prepareAccountedGo)
)

// nestedGoEnv is the allowlist of a nested go call's environment, from go
// help environment (go1.27) and the runtime's settings. The keys a call may
// set freely change what is built and where modules come from, never a
// program go runs, a file it writes outside its flags' checked paths, or its
// parallelism:
//
//   - the target: GOOS, GOARCH and their per-architecture levels,
//     GOEXPERIMENT, GOFIPS140, CGO_ENABLED and GO_EXTLINK_ENABLED (cgo and
//     external linking run the run's own C compiler, below);
//   - the module source: GO111MODULE, GOPROXY, GONOPROXY, GOPRIVATE,
//     GONOSUMDB, GOSUMDB, GOINSECURE (a direct fetch runs git through the
//     wrapper), and GOBIN, which only go install, a refused verb, writes;
//   - runtime settings: GODEBUG, GOGC, GOMEMLIMIT, GOTRACEBACK;
//   - the CGO_*FLAGS_DISALLOW patterns, which only refuse more #cgo flags.
//
// A call may only keep the run's own value of every other key, or leave it
// unset (or empty) for go's default: the programs go and cgo run (CC, CXX,
// FC, AR, GCCGO, GCCGOTOOLDIR, PKG_CONFIG, GOCACHEPROG, GOAUTH but "off",
// GOVCS) and the C compiler's and linker's flags, which can name programs
// and plugins (CGO_CFLAGS, CGO_CPPFLAGS, CGO_CXXFLAGS, CGO_FFLAGS,
// CGO_LDFLAGS, and the CGO_*FLAGS_ALLOW patterns that let #cgo lines pass
// such flags); GORACE, whose log_path writes anywhere; and any other GO* or
// CGO_* key, which a newer go may read. GOMAXPROCS may also be at most the
// call's clamp, as -cpu may. The build locations, the run's Go setting and
// GOFLAGS have checks of their own.
var nestedGoEnv = map[string]nestedGoEnvRule{
	"GOOS": nestedGoEnvFree, "GOARCH": nestedGoEnvFree, "GO386": nestedGoEnvFree, "GOAMD64": nestedGoEnvFree, "GOARM": nestedGoEnvFree,
	"GOARM64": nestedGoEnvFree, "GOMIPS": nestedGoEnvFree, "GOMIPS64": nestedGoEnvFree, "GOPPC64": nestedGoEnvFree,
	"GORISCV64": nestedGoEnvFree, "GOWASM": nestedGoEnvFree, "GOEXPERIMENT": nestedGoEnvFree, "GOFIPS140": nestedGoEnvFree,
	"CGO_ENABLED": nestedGoEnvFree, "GO_EXTLINK_ENABLED": nestedGoEnvFree,
	"GO111MODULE": nestedGoEnvFree, "GOPROXY": nestedGoEnvFree, "GONOPROXY": nestedGoEnvFree, "GOPRIVATE": nestedGoEnvFree,
	"GONOSUMDB": nestedGoEnvFree, "GOSUMDB": nestedGoEnvFree, "GOINSECURE": nestedGoEnvFree, "GOBIN": nestedGoEnvFree,
	"GODEBUG": nestedGoEnvFree, "GOGC": nestedGoEnvFree, "GOMEMLIMIT": nestedGoEnvFree, "GOTRACEBACK": nestedGoEnvFree,
	"CGO_CFLAGS_DISALLOW": nestedGoEnvFree, "CGO_CPPFLAGS_DISALLOW": nestedGoEnvFree, "CGO_CXXFLAGS_DISALLOW": nestedGoEnvFree,
	"CGO_FFLAGS_DISALLOW": nestedGoEnvFree, "CGO_LDFLAGS_DISALLOW": nestedGoEnvFree,
	"CC": nestedGoEnvOwn, "CXX": nestedGoEnvOwn, "FC": nestedGoEnvOwn, "AR": nestedGoEnvOwn, "GCCGO": nestedGoEnvOwn,
	"GCCGOTOOLDIR": nestedGoEnvOwn, "PKG_CONFIG": nestedGoEnvOwn, "GOCACHEPROG": nestedGoEnvOwn, "GOAUTH": nestedGoEnvOwn,
	"GOVCS": nestedGoEnvOwn, "GORACE": nestedGoEnvOwn,
	"CGO_CFLAGS": nestedGoEnvOwn, "CGO_CPPFLAGS": nestedGoEnvOwn, "CGO_CXXFLAGS": nestedGoEnvOwn, "CGO_FFLAGS": nestedGoEnvOwn,
	"CGO_LDFLAGS": nestedGoEnvOwn, "CGO_CFLAGS_ALLOW": nestedGoEnvOwn, "CGO_CPPFLAGS_ALLOW": nestedGoEnvOwn,
	"CGO_CXXFLAGS_ALLOW": nestedGoEnvOwn, "CGO_FFLAGS_ALLOW": nestedGoEnvOwn, "CGO_LDFLAGS_ALLOW": nestedGoEnvOwn,
	"GOMAXPROCS": nestedGoEnvThreads,
	"GOCACHE":    nestedGoEnvLocation, "GOMODCACHE": nestedGoEnvLocation, "GOPATH": nestedGoEnvLocation, "GOTMPDIR": nestedGoEnvLocation,
	"GOCOVERDIR": nestedGoEnvLocation, "GOWORK": nestedGoEnvLocation, "TMPDIR": nestedGoEnvLocation,
	"GOROOT": nestedGoEnvSetting, "GOTOOLCHAIN": nestedGoEnvSetting, "GOENV": nestedGoEnvSetting,
	"GOFLAGS": nestedGoEnvFlags,
}

// nestedGoEnvRuleOf is key's rule: nestedGoEnv's, else nestedGoEnvOwn for any
// other GO* or CGO_* key, else 0 for a key go does not read.
func nestedGoEnvRuleOf(key string) nestedGoEnvRule {
	if rule := nestedGoEnv[key]; rule != 0 {
		return rule
	}
	if strings.HasPrefix(key, "GO") || strings.HasPrefix(key, "CGO_") {
		return nestedGoEnvOwn
	}
	return 0
}

// keeps reports whether value is the run's own value of key (NestedGo.Env).
func (g *NestedGo) keeps(key, value string) bool {
	if g == nil {
		return false
	}
	own, ok := g.Env[key]
	return ok && own == value
}

// accountedGoEnv is the environment a nested go call runs with under an
// account-mode run's Go setting g (InstallAccounting, RunShim). Every entry
// of env is checked, since go reads the first of a key and exec the last:
//
//   - GOROOT is the run's GOROOT mirror (InstallGoRoot), so the go its tests
//     start meets the run's wrappers again; a mirror that a broker inside the
//     run made of it is kept. GOTOOLCHAIN is local, since a switched
//     toolchain puts its own bin first, and GOENV is off, so no Go env file
//     sets flags the policy never sees. Each is set when the call has none
//     (or an empty one) and refused when it names another.
//   - A key nestedGoEnv does not let a call set freely keeps the run's own
//     value (NestedGo.Env) or is unset; GOMAXPROCS may also be at most the
//     call's clamp. Anything else is refused.
//   - For go build, test, vet and list, a Go build location (GOCACHE,
//     GOMODCACHE, GOPATH, GOTMPDIR, GOCOVERDIR, TMPDIR) outside the scope and
//     under a RAM-backed root, or a GOWORK file outside the scope, is refused
//     unless it is the run's own (NestedGo.Env).
//
// Programs go finds by name on the call's PATH (the default C compiler,
// pkg-config, version control tools, go_GOOS_GOARCH_exec for a
// cross-compiled test) are not checked: a test can run any program on its
// PATH itself.
func (p Policy) accountedGoEnv(argv, env []string, g *NestedGo) ([]string, error) {
	mirror := g.goRootMirror()
	threads := accountedGoThreads(argv)
	goroot := ""
	for _, kv := range env {
		key, v, _ := strings.Cut(kv, "=")
		switch nestedGoEnvRuleOf(key) {
		case nestedGoEnvSetting:
			ok := true
			switch key {
			case "GOROOT":
				ok = mirror == "" || v == "" || mirrorsGoRoot(v, mirror)
				if ok && v != "" && goroot == "" {
					goroot = v
				}
			case "GOTOOLCHAIN":
				ok = v == "" || v == "local"
			case "GOENV":
				ok = v == "" || v == "off"
			}
			if !ok {
				return nil, refuse("nested go with " + kv + ": nested go runs with the run's GOROOT mirror, GOTOOLCHAIN=local and GOENV=off, so the go its tests start meets the run's wrappers and no Go env file sets flags the policy never sees")
			}
		case nestedGoEnvOwn:
			if v != "" && !g.keeps(key, v) && !g.cgoPinned(key, v) && (key != "GOAUTH" || v != "off") {
				return nil, refuse("nested go with " + kv + ": " + key + " may name a program go runs or that program's flags, or is a Go setting the policy does not know; nested calls keep the run's own value or leave it unset")
			}
		case nestedGoEnvThreads:
			if n, err := strconv.Atoi(v); v != "" && !g.keeps(key, v) && (err != nil || n < 1 || n > threads) {
				return nil, refuse("nested go with " + kv + ": more threads than the call's clamp, -p=" + strconv.Itoa(threads) + "; nested calls keep the run's own value or set at most the clamp")
			}
		}
	}
	if mirror != "" && goroot == "" {
		goroot = mirror
	}
	if verb := ""; len(argv) > 1 {
		verb = argv[1]
		if verb == "build" || verb == "test" || verb == "vet" || verb == "list" {
			if e := p.accountedGoLocations(env, g); e != nil {
				return nil, e
			}
		}
	}
	if mirror == "" {
		goroot = "" // no toolchain was mirrored; the call's GOROOT stays
	}
	if len(argv) < 2 || argv[1] != "version" { // go version runs no C toolchain
		var err error
		if env, err = accountedCgoPrograms(env, g); err != nil {
			return nil, err
		}
	}
	return GoRunEnv(env, goroot), nil
}

// accountedCgoPrograms pins the programs a nested go call would find by name
// on its own PATH for cgo, the C compiler first (CC, CXX, FC and PKG_CONFIG,
// unset or naming a program without a directory: cgoProgramKeys), to the
// run's own (NestedGo.Programs), keeping the arguments of the run's own value.
// A program the call's PATH does not have either is left unset, so go still
// turns cgo off without a C compiler, or fails as it would. A program the
// call's PATH has and the run's does not is refused: a test could otherwise
// choose the program go runs (FAE-HOST-014). The target system's default
// compiler is the one pinned (GOOS, else the host's). Programs those
// programs find on PATH themselves (the assembler and linker gcc runs, ar for
// -buildmode=c-archive) are not pinned.
func accountedCgoPrograms(env []string, g *NestedGo) ([]string, error) {
	callPath, _ := lastGoEnv(env, "PATH")
	goos, _ := lastGoEnv(env, "GOOS")
	if goos == "" {
		goos = runtime.GOOS
	}
	for _, key := range cgoProgramKeys {
		v, _ := lastGoEnv(env, key)
		fields, err := goQuotedFields(v)
		if err != nil {
			continue // go refuses a value it cannot split
		}
		name, rest := cgoDefaultProgram(key, goos), []string(nil)
		if len(fields) > 0 {
			name, rest = fields[0], fields[1:]
		}
		found := ""
		if cgoProgramByName(name) {
			found = resolveShimTool(name, "", callPath)
		}
		if found == "" {
			continue
		}
		pinned := g.cgoProgram(name)
		value, err := goQuotedJoin(append([]string{pinned}, rest...))
		if pinned == "" || err != nil {
			return nil, refuse("nested go would run " + found + " for " + key + ", a program the call's PATH chose: the run's PATH has no " + name + " to run instead; nested calls run only the run's own programs go finds by name")
		}
		env = withGoEnv(env, key, value)
	}
	return env, nil
}

// accountedGoLocations refuses a nested go build's Go build location outside
// the scope under a RAM-backed root, or a GOWORK file outside the scope,
// unless it is the run's own.
func (p Policy) accountedGoLocations(env []string, g *NestedGo) error {
	var e error
	if p.Worktree, e = filepath.EvalSymlinks(p.Worktree); e != nil {
		return e
	}
	if p.Scratch != "" {
		if p.Scratch, e = filepath.EvalSymlinks(p.Scratch); e != nil {
			return e
		}
	}
	for _, kv := range env {
		key, v, _ := strings.Cut(kv, "=")
		if !slices.Contains(goBuildLocationKeys, key) || v == "" || key == "GOWORK" && v == "off" || g.keeps(key, v) {
			continue
		}
		dirs := []string{v}
		if key == "GOPATH" {
			dirs = filepath.SplitList(v)
		}
		for _, d := range dirs {
			if !filepath.IsAbs(d) {
				continue // go refuses a relative location, or resolves it in the scope
			}
			r, e := resolve("/", d)
			if e != nil {
				return e
			}
			if p.inScope(r) {
				continue
			}
			why := "a workspace file"
			if key != "GOWORK" {
				ram, e := p.ramBacked(r)
				if e != nil {
					return e
				}
				if !ram {
					continue
				}
				why = "under a RAM-backed root"
			}
			return refuse("nested go with " + key + "=" + d + ": " + why + " outside the run's worktree and scratch directory")
		}
	}
	return nil
}

func CheckFormatFile(path string) error {
	st, e := os.Lstat(path)
	if e != nil {
		return e
	}
	if !st.Mode().IsRegular() {
		return refuse("formatter requires a regular per-file target; use item-scoped formatting")
	}
	if st.Size() > 1<<20 {
		return refuse(path + " exceeds 1 MiB; use item-scoped formatting")
	}
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return refuse(path + " exceeds 1 MiB; use item-scoped formatting")
	}
	lines := strings.Count(string(b), "\n")
	if len(b) > 0 && b[len(b)-1] != '\n' {
		lines++
	}
	if lines > 30000 {
		return refuse(path + " exceeds 30k lines; use item-scoped formatting")
	}
	return nil
}

// FormatTargets expands bounded Go formatter directories to explicit files.
// Symlinks and unsupported module-following formatter dialects are never followed.
func FormatTargets(path string) ([]string, error) {
	st, e := os.Lstat(path)
	if e != nil {
		return nil, e
	}
	if !st.IsDir() {
		if e = CheckFormatFile(path); e != nil {
			return nil, e
		}
		return []string{path}, nil
	}
	var out []string
	count := 0
	var bytes int64
	e = filepath.WalkDir(path, func(p string, d fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		count++
		if count > 10000 {
			return refuse("formatter tree exceeds 10000 entries; use item-scoped formatting")
		}
		if p != path && d.IsDir() && strings.HasPrefix(d.Name(), ".") {
			return filepath.SkipDir
		}
		if d.Type()&os.ModeSymlink != 0 {
			return refuse("symlink in formatter tree; use item-scoped formatting")
		}
		if !d.IsDir() && strings.HasSuffix(p, ".go") {
			if e := CheckFormatFile(p); e != nil {
				return e
			}
			st, e := d.Info()
			if e != nil {
				return e
			}
			bytes += st.Size()
			if bytes > 64<<20 {
				return refuse("formatter tree exceeds 64 MiB; use item-scoped formatting")
			}
			out = append(out, p)
		}
		return nil
	})
	return out, e
}
