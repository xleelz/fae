package workflow

import (
	"fmt"
	"slices"
	"strings"
)

// GenerateSpec declares deterministic task-owned output changes.
type GenerateSpec struct {
	Writes []string `json:"writes"`
}

// WriteScopes is an intersection: each nonempty layer must admit the path.
// A nil layer is undeclared; an explicitly empty layer admits no writes.
func AllowsWriteScopes(path string, layers [][]string) bool {
	if !DeclaredPathSafe(path) {
		return false
	}
	for _, layer := range layers {
		if !MatchesAnyGlob(path, layer) {
			return false
		}
	}
	return len(layers) > 0
}

func EffectiveWriteScopes(role Role, stage Stage, branch, task []string) [][]string {
	var layers [][]string
	// Branch/task declarations only narrow existing stage/role authority.
	if !stage.ChangeProducer && stage.Generate == nil && len(role.Writes) == 0 && len(stage.Writes) == 0 {
		layers = append(layers, []string{})
	}
	scopes := [][]string{role.Writes, stage.Writes, branch, task}
	if stage.Generate != nil {
		scopes = append(scopes, stage.Generate.Writes)
	}
	for _, v := range scopes {
		if len(v) > 0 {
			layers = append(layers, slices.Clone(v))
		}
	}
	return layers
}

// GlobScopeNarrows proves a conservative subset relation. Exact paths can be
// contained by any parent glob; a glob can narrow the same glob or a directory
// subtree. Unprovable language inclusion is refused, never guessed.
func GlobScopeNarrows(child, parent []string) bool {
	if parent == nil {
		return true
	}
	for _, p := range child {
		matched := false
		for _, q := range parent {
			if p == q || q == "**" || !strings.ContainsAny(p, "*?[") && MatchGlob(q, p) || strings.HasSuffix(q, "/**") && (strings.HasPrefix(p, strings.TrimSuffix(q, "**"))) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func ValidScopeGlob(p string) bool {
	q := p
	for {
		a := strings.Index(q, "{{")
		if a < 0 {
			break
		}
		b := strings.Index(q[a+2:], "}}")
		if b < 0 {
			return false
		}
		q = q[:a] + "bound" + q[a+2+b+2:]
	}
	_, protected := ProtectedPath(q)
	return DeclaredPathSafe(q) && !protected
}

func validateFilesystem(d Document, bad func(string, string)) {
	if d.Produces != "" && d.Produces != "artifacts" {
		bad("produces", "must be artifacts or omitted")
	}
	check := func(label string, writes, scratch []string, staging string) {
		if staging != "" && staging != "write" && staging != "read" {
			bad(label+".staging", "must be read or write")
		}
		for _, p := range append(slices.Clone(writes), scratch...) {
			if !ValidScopeGlob(p) {
				bad(label, "unsafe write or scratch glob "+p)
			}
		}
		for _, p := range scratch {
			for _, q := range writes {
				if GlobsOverlap(p, q) {
					bad(label+".scratch", "scratch overlaps write scope")
				}
			}
		}
	}
	for name, r := range d.Roles {
		check("roles."+name, r.Writes, r.Scratch, r.Staging)
		if err := ValidateShellAllow(r.Profile.ShellAllow); err != nil {
			bad("roles."+name+".profile.shell_allow", err.Error())
		}
	}
	for id, s := range d.Stages {
		label := "stages." + id
		check(label, s.Writes, s.Scratch, s.Staging)
		for _, name := range stageRoles(s) {
			r := d.Roles[name]
			check(label+".effective", append(slices.Clone(r.Writes), s.Writes...), append(slices.Clone(r.Scratch), s.Scratch...), s.Staging)
			for _, scratch := range append(slices.Clone(r.Scratch), s.Scratch...) {
				for _, readOnly := range s.ReadOnlyFiles {
					if GlobsOverlap(scratch, readOnly) {
						bad(label+".scratch", "scratch overlaps read-only input")
					}
				}
			}
			if s.Writes != nil && !GlobScopeNarrows(s.Writes, r.Writes) {
				bad(label+".writes", "stage write scope widens role scope")
			}
			if s.Profile != nil && s.Profile.ShellAllow != nil && !ShellAllowNarrows(s.Profile.ShellAllow, r.Profile.ShellAllow) {
				bad(label+".profile.shell_allow", "stage shell allowlist widens role profile")
			}
		}
		if s.Profile != nil {
			if err := ValidateShellAllow(s.Profile.ShellAllow); err != nil {
				bad(label+".profile.shell_allow", err.Error())
			}
		}
		if s.Generate != nil {
			if s.Primitive != HostCommand || len(s.Generate.Writes) == 0 {
				bad(label+".generate", "generator requires a HostCommand and nonempty writes")
			}
			for _, p := range s.Generate.Writes {
				if !ValidScopeGlob(p) {
					bad(label+".generate.writes", "unsafe generator glob")
				}
			}
			for _, name := range stageRoles(s) {
				for _, layer := range EffectiveWriteScopes(d.Roles[name], s, nil, nil) {
					if !GlobScopeNarrows(s.Generate.Writes, layer) {
						bad(label+".generate.writes", "generator writes exceed effective scope")
					}
				}
			}
		}
		for _, c := range s.Capture {
			if c != "stdout" && c != "stderr" {
				bad(label+".capture", fmt.Sprintf("unknown stream %q", c))
			}
		}
	}
}
