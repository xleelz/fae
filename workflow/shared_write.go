package workflow

import (
	"path/filepath"
	"strings"
)

type SharedWriteRegistry struct {
	Epoch  uint64             `json:"epoch"`
	Grants []SharedWriteGrant `json:"grants"`
}
type SharedWriteGrant struct {
	Session  string             `json:"session"`
	Dispatch string             `json:"dispatch"`
	Active   bool               `json:"active"`
	Scopes   []SharedWriteScope `json:"scopes"`
}
type SharedWriteScope struct {
	Root   string     `json:"root"`
	Layers [][]string `json:"layers"`
}

func SharedWriteScopeAllows(scope SharedWriteScope, absolute string) bool {
	if !filepath.IsAbs(scope.Root) || !filepath.IsAbs(absolute) || len(scope.Layers) == 0 {
		return false
	}
	relative, err := filepath.Rel(scope.Root, absolute)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	relative = filepath.ToSlash(relative)
	for _, layer := range scope.Layers {
		matches := false
		for _, pattern := range layer {
			if MatchGlob(pattern, relative) {
				matches = true
				break
			}
		}
		if !matches {
			return false
		}
	}
	return true
}

// Finished peers remain authoritative for the epoch: a slower collector must
// not revert legitimate writes merely because their producing session ended.
func ForeignSharedWrite(registry SharedWriteRegistry, own, absolute string) bool {
	if own == "" {
		return false
	}
	for _, grant := range registry.Grants {
		if own == grant.Session || own == grant.Dispatch {
			continue
		}
		for _, scope := range grant.Scopes {
			if SharedWriteScopeAllows(scope, absolute) {
				return true
			}
		}
	}
	return false
}
func SharedWriteScopesOverlap(a, b SharedWriteScope) bool {
	if len(a.Layers) == 0 || len(b.Layers) == 0 {
		return false
	}
	// Intersections are disjoint if any pair of their union layers is disjoint.
	for _, left := range a.Layers {
		for _, right := range b.Layers {
			overlaps := false
			for _, x := range left {
				for _, y := range right {
					if GlobsOverlap(filepath.ToSlash(filepath.Join(a.Root, filepath.FromSlash(x))), filepath.ToSlash(filepath.Join(b.Root, filepath.FromSlash(y)))) {
						overlaps = true
						break
					}
				}
				if overlaps {
					break
				}
			}
			if !overlaps {
				return false
			}
		}
	}
	return true
}
