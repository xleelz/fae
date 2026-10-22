// Package commandshim implements the trusted broker's command policy. PATH
// wrappers deliberately refuse nested commands; they are not an OS sandbox.
package commandshim

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const Refused = 77

type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return "command refused (exit 77): " + r.Reason }
func refuse(s string) error      { return &Refusal{s} }

// InstallDenials installs immutable-by-convention wrappers in a private session
// directory. Trusted broker entry executes its resolved binary directly; children
// must use these wrappers and cannot silently inherit a parent's resource slot.
func InstallDenials(parent string) (string, error) {
	dir := filepath.Join(parent, "command-shims")
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	for _, name := range []string{"go", "gofmt", "cargo", "rustc", "rustfmt", "make", "cmake", "ninja", "pytest", "git"} {
		script := "#!/bin/sh\nprintf '%s\\n' 'fae: command refused (exit 77): role tools and nested heavy commands require a separate broker dispatch; use a scoped host-command stage' >&2\nexit 77\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0500); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func PrependPath(env []string, dir string) []string {
	result := make([]string, 0, len(env)+1)
	path := "/usr/bin:/bin"
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			path = strings.TrimPrefix(e, "PATH=")
		} else {
			result = append(result, e)
		}
	}
	return append(result, "PATH="+dir+string(os.PathListSeparator)+path)
}

type Policy struct {
	Worktree, CWD               string
	CodeRole, LeaseHeld, Nested bool
	Local                       bool
	// RAMRoots must be canonical physical roots. Output beneath them is denied.
	RAMRoots []string
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
	if !inside(p.Worktree, v) {
		return "", refuse("target outside own worktree: " + name)
	}
	return v, nil
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
	switch name {
	case "git":
		if p.Local {
			return nil, refuse("VCS commands in role command paths are disabled in Local mode; use the dedicated VCS broker")
		}
		return nil, refuse("use the dedicated VCS broker")
	case "cargo", "rustc", "rustfmt", "make", "cmake", "ninja", "pytest":
		return nil, refuse("unsupported build wrapper requires a scoped broker adapter (including version probes)")
	case "gofmt":
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
	case "go":
		if len(args) < 2 || (args[1] != "test" && args[1] != "build" && args[1] != "vet") {
			return nil, refuse("unsupported Go verb; probes also require broker admission")
		}
		out := args[:2]
		targets := 0
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
				dest, e := p.scoped(v)
				if e != nil {
					return nil, e
				}
				for _, r := range p.RAMRoots {
					physical, e := resolve("/", r)
					if e != nil {
						return nil, e
					}
					if inside(physical, dest) {
						return nil, refuse("RAM-backed temporary build output: " + dest)
					}
				}
				out = append(out, "-o", dest)
				continue
			}
			if strings.HasPrefix(a, "-") {
				if targets > 0 {
					return nil, refuse("flags must precede package targets")
				}
				if a == "-json" || a == "-a" || a == "-x" || a == "-race" || a == "-v" || a == "-short" || a == "-failfast" || strings.HasPrefix(a, "-count=") || strings.HasPrefix(a, "-run=") || strings.HasPrefix(a, "-timeout=") {
					out = append(out, a)
					continue
				}
				return nil, refuse("unsupported Go flag: " + a)
			}
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
		// Flags must precede positional package arguments for Go's parser.
		flags := []string{out[0], out[1], "-p=2"}
		if args[1] == "test" {
			flags = append(flags, "-parallel=2")
		}
		return append(flags, out[2:]...), nil
	default:
		return args, nil // Unknown vectors are separately literal-allowlisted by the trusted broker.
	}
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
