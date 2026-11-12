package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// FAE-ARC-023: a bound workspace that is a package inside a multi-package build
// workspace is detected from validated markers in its ancestors. Detection only
// stats and reads bounded marker files on the workspace's own ancestor chain; it
// never runs a build tool and never creates, syncs or merges a build workspace.

// Providers for task worktrees.
const (
	// ProviderGitWorktree is an isolated git worktree of the bound workspace.
	ProviderGitWorktree = "git-worktree"
	// ProviderBuildBinding is a pre-provisioned build-workspace binding for the
	// package (FAE-EXT-015); Fae never creates or syncs it on the drive's path.
	ProviderBuildBinding = "build-workspace-binding"
)

const maxMarkerBytes = 1 << 20

// BuildWorkspace is the detected and pinned build-workspace identity.
type BuildWorkspace struct {
	Kind      string `json:"kind,omitempty"`      // brazil, go-work, cargo, pnpm, npm, bazel; "" when standalone
	Root      string `json:"root,omitempty"`      // directory holding the marker
	Marker    string `json:"marker,omitempty"`    // marker file
	Digest    string `json:"digest,omitempty"`    // sha256 of the marker bytes
	Toolchain string `json:"toolchain,omitempty"` // the build tool whose every invocation is heavy (FAE-HOST-013)
	Provider  string `json:"provider"`
}

// ErrAmbiguousBuildWorkspace refuses markers that disagree or cannot be validated.
var ErrAmbiguousBuildWorkspace = errors.New("ambiguous build workspace")

type marker struct {
	kind, file, toolchain string
	// member reports whether the marker's workspace contains pkg (relative to
	// the marker directory); an error is an unvalidated marker.
	member func(raw []byte, rel string) (bool, error)
}

var markers = []marker{
	{"brazil", "packageInfo", "brazil-build", func(_ []byte, rel string) (bool, error) {
		parts := strings.Split(rel, "/")
		return len(parts) == 2 && parts[0] == "src", nil
	}},
	{"go-work", "go.work", "go", goWorkMember},
	{"cargo", "Cargo.toml", "cargo", cargoMember},
	{"pnpm", "pnpm-workspace.yaml", "pnpm", pnpmMember},
	{"npm", "package.json", "npm", npmMember},
	{"bazel", "MODULE.bazel", "bazel", anyMember},
	{"bazel", "WORKSPACE", "bazel", anyMember},
	{"bazel", "WORKSPACE.bazel", "bazel", anyMember},
}

func anyMember([]byte, string) (bool, error) { return true, nil }

// within reports whether rel (the package) is pattern or lies under it; a
// pattern may use path globs per element.
func within(pattern, rel string) bool {
	pattern = strings.TrimPrefix(filepath.ToSlash(filepath.Clean(pattern)), "./")
	if pattern == "." {
		return true
	}
	p, r := strings.Split(pattern, "/"), strings.Split(rel, "/")
	if len(p) > len(r) {
		return false
	}
	for i := range p {
		if p[i] == "**" {
			return true
		}
		if ok, err := filepath.Match(p[i], r[i]); err != nil || !ok {
			return false
		}
	}
	return true
}

var goUse = regexp.MustCompile(`(?m)^\s*use\s+(?:\(([^)]*)\)|(\S+))`)

func goWorkMember(raw []byte, rel string) (bool, error) {
	text := string(raw)
	if !regexp.MustCompile(`(?m)^\s*go\s+\d`).MatchString(text) {
		return false, errors.New("go.work lacks a go directive")
	}
	member := false
	for _, m := range goUse.FindAllStringSubmatch(text, -1) {
		for _, u := range strings.Fields(m[1] + " " + m[2]) {
			if strings.HasPrefix(u, "//") {
				continue
			}
			member = member || within(strings.Trim(u, `"`), rel)
		}
	}
	return member, nil
}

var trailingComma = regexp.MustCompile(`,\s*]`)

// tomlUncomment drops a # comment outside quotes and surrounding space.
func tomlUncomment(line string) string {
	quote := rune(0)
	for i, r := range line {
		switch {
		case quote != 0 && r == quote:
			quote = 0
		case quote == 0 && (r == '"' || r == '\''):
			quote = r
		case quote == 0 && r == '#':
			return strings.TrimSpace(line[:i])
		}
	}
	return strings.TrimSpace(line)
}

// tomlArray reads a string array value of key inside the [table] section.
func tomlArray(text, table, key string) ([]string, bool, error) {
	section := ""
	var out []string
	found := false
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(line, "[] \t")
			continue
		}
		if section != table || !strings.HasPrefix(line, key) || !strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(line, key)), "=") {
			continue
		}
		value := tomlUncomment(strings.SplitN(line, "=", 2)[1])
		for !strings.Contains(value, "]") && i+1 < len(lines) {
			i++
			value += tomlUncomment(lines[i])
		}
		value = trailingComma.ReplaceAllString(value, "]")
		if !strings.HasPrefix(value, "[") || !strings.Contains(value, "]") {
			return nil, false, fmt.Errorf("[%s] %s is not an array", table, key)
		}
		var items []string
		if err := json.Unmarshal([]byte(strings.ReplaceAll(value[:strings.LastIndex(value, "]")+1], "'", `"`)), &items); err != nil {
			return nil, false, fmt.Errorf("[%s] %s: %v", table, key, err)
		}
		out, found = append(out, items...), true
	}
	return out, found, nil
}

func cargoMember(raw []byte, rel string) (bool, error) {
	text := string(raw)
	if !regexp.MustCompile(`(?m)^\s*\[workspace\]\s*$`).MatchString(text) {
		return false, nil // a single crate, not a workspace
	}
	members, _, err := tomlArray(text, "workspace", "members")
	if err != nil {
		return false, err
	}
	excluded, _, err := tomlArray(text, "workspace", "exclude")
	if err != nil {
		return false, err
	}
	for _, x := range excluded {
		if within(x, rel) {
			return false, nil
		}
	}
	for _, m := range members {
		if within(m, rel) {
			return true, nil
		}
	}
	return false, nil
}

func pnpmMember(raw []byte, rel string) (bool, error) {
	in, member, any := false, false, false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "packages:"):
			in = true
		case in && strings.HasPrefix(trimmed, "- "):
			any = true
			p := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "- ")), `"'`)
			if !strings.HasPrefix(p, "!") {
				member = member || within(p, rel)
			} else if within(p[1:], rel) {
				return false, nil
			}
		case trimmed != "" && !strings.HasPrefix(trimmed, "#") && line[0] != ' ' && line[0] != '\t':
			in = false
		}
	}
	if !any {
		return false, errors.New("pnpm-workspace.yaml lists no packages")
	}
	return member, nil
}

func npmMember(raw []byte, rel string) (bool, error) {
	var pkg struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return false, err
	}
	if len(pkg.Workspaces) == 0 {
		return false, nil // a single package
	}
	var globs []string
	if err := json.Unmarshal(pkg.Workspaces, &globs); err != nil {
		var nested struct {
			Packages []string `json:"packages"`
		}
		if err = json.Unmarshal(pkg.Workspaces, &nested); err != nil {
			return false, errors.New("package.json workspaces is neither a list nor {packages}")
		}
		globs = nested.Packages
	}
	for _, g := range globs {
		if within(g, rel) {
			return true, nil
		}
	}
	return false, nil
}

func readMarker(path string) ([]byte, bool, error) {
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !st.Mode().IsRegular() {
		return nil, true, fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, true, err
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxMarkerBytes+1))
	if err == nil && len(raw) > maxMarkerBytes {
		err = fmt.Errorf("%s exceeds %d bytes", path, maxMarkerBytes)
	}
	return raw, true, err
}

// DetectBuildWorkspace inspects the strict ancestors of the canonical workspace
// path for build-workspace markers. The nearest claiming marker of a kind wins
// (as the tools resolve it); claims of different kinds, or a present marker that
// fails validation, are ErrAmbiguousBuildWorkspace. Markers inside the
// workspace itself belong to its own tree and are carried by its worktrees.
func DetectBuildWorkspace(path string) (BuildWorkspace, error) {
	none := BuildWorkspace{Provider: ProviderGitWorktree}
	if err := Validate(path); err != nil {
		return none, err
	}
	var claims []BuildWorkspace
	var problems []string
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		for _, m := range markers {
			marker := filepath.Join(dir, m.file)
			raw, present, err := readMarker(marker)
			if !present {
				continue
			}
			member := false
			if err == nil {
				member, err = m.member(raw, rel)
			}
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s (%s): %v", marker, m.kind, err))
				continue
			}
			if member {
				sum := sha256.Sum256(raw)
				claims = append(claims, BuildWorkspace{Kind: m.kind, Root: dir, Marker: marker, Digest: hex.EncodeToString(sum[:]), Toolchain: m.toolchain, Provider: ProviderBuildBinding})
			}
		}
		if dir == "/" {
			break
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return none, fmt.Errorf("%w: unvalidated marker %s", ErrAmbiguousBuildWorkspace, strings.Join(problems, "; "))
	}
	if len(claims) == 0 {
		return none, nil
	}
	kinds := map[string]bool{}
	for _, c := range claims {
		kinds[c.Kind] = true
	}
	if len(kinds) > 1 {
		var names []string
		for _, c := range claims {
			names = append(names, c.Kind+" at "+c.Marker)
		}
		return none, fmt.Errorf("%w: %s", ErrAmbiguousBuildWorkspace, strings.Join(names, ", "))
	}
	return claims[0], nil // the nearest ancestor of the single kind
}

// VerifyBuildWorkspace re-detects at time of use and refuses a changed
// identity: a different kind, root, marker or provider (a marker added,
// removed, or no longer listing the package). An edit that keeps the package a
// member only changes Digest, which the returned current detection carries.
func VerifyBuildWorkspace(path string, pinned BuildWorkspace) (BuildWorkspace, error) {
	current, err := DetectBuildWorkspace(path)
	if err != nil {
		return current, err
	}
	if current.Kind != pinned.Kind || current.Root != pinned.Root || current.Marker != pinned.Marker || current.Provider != pinned.Provider || current.Toolchain != pinned.Toolchain {
		return current, fmt.Errorf("build workspace identity changed since binding: pinned %q %s, now %q %s", pinned.Kind, pinned.Marker, current.Kind, current.Marker)
	}
	return current, nil
}
