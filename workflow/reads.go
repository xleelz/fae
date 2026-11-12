package workflow

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
)

// ReadPolicy declares allowed and denied path globs over the worktree and any
// declared read roots (FAE-SEC-008). Deny always wins over allow, and a stage
// or task policy may only narrow its role's policy.
type ReadPolicy struct {
	Allow     []string   `json:"allow,omitempty"`
	Deny      []string   `json:"deny,omitempty"`
	AllowSets [][]string `json:"allow_sets,omitempty"`
}

// IsGitPush reports whether an ExternalEffect stage is an engine-run git-push
// stage (FAE-EXT-023, FAE-MODE-002).
func (s Stage) IsGitPush() bool {
	return s.Primitive == ExternalEffect && (s.EffectKind == "git-push" || s.Subject == "git-push")
}

// CleanRelPath normalizes a repository-relative path and reports whether it is safe.
func CleanRelPath(rel string) (string, bool) {
	s := strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/")
	s = strings.TrimPrefix(s, "./")
	if s == "" || s == "." || strings.HasPrefix(s, "/") || strings.ContainsRune(s, 0) || strings.ContainsRune(s, '\n') {
		return "", false
	}
	cleaned := path.Clean(s)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	for _, seg := range strings.Split(cleaned, "/") {
		if seg == "" || seg == "." || seg == ".." || seg == ".git" {
			return "", false
		}
	}
	return cleaned, true
}

// ValidGlob reports whether pattern is a well-formed relative path glob.
func ValidGlob(pattern string) bool {
	p := strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsRune(p, 0) || strings.ContainsRune(p, '\n') {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, "x"); err != nil {
			return false
		}
	}
	return true
}

// MatchGlob reports whether rel matches glob pattern, supporting ** across
// path segments as well as standard *, ?, and [...] within segments.
func MatchGlob(pattern, rel string) bool {
	return matchGlobInternal(pattern, rel, false)
}

// MatchGlobFold is case-insensitive MatchGlob (used for target deny_paths).
func MatchGlobFold(pattern, rel string) bool {
	return matchGlobInternal(pattern, rel, true)
}

func matchGlobInternal(pattern, rel string, fold bool) bool {
	p := strings.ReplaceAll(strings.TrimSpace(pattern), "\\", "/")
	p = strings.TrimPrefix(p, "./")
	dirOnly := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	r := strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/")
	r = strings.TrimPrefix(r, "./")
	r = strings.TrimSuffix(r, "/")
	if p == "" || r == "" {
		return false
	}
	if fold {
		p = strings.ToLower(p)
		r = strings.ToLower(r)
	}
	if dirOnly {
		return r == p || strings.HasPrefix(r, p+"/")
	}
	pSegs := strings.Split(p, "/")
	rSegs := strings.Split(r, "/")
	if matchSegments(pSegs, rSegs) {
		return true
	}
	// If pattern has no slash and Ends with *, or is a directory prefix when ends with /**
	if len(pSegs) >= 2 && pSegs[len(pSegs)-1] == "**" {
		prefix := strings.Join(pSegs[:len(pSegs)-1], "/")
		if r == prefix {
			return true
		}
	}
	return false
}

func matchSegments(pSegs, rSegs []string) bool {
	if len(pSegs) == 0 {
		return len(rSegs) == 0
	}
	if pSegs[0] == "**" {
		// Collapse consecutive **
		for len(pSegs) > 1 && pSegs[1] == "**" {
			pSegs = pSegs[1:]
		}
		if len(pSegs) == 1 {
			return true
		}
		for i := 0; i <= len(rSegs); i++ {
			if matchSegments(pSegs[1:], rSegs[i:]) {
				return true
			}
		}
		return false
	}
	if len(rSegs) == 0 {
		return false
	}
	ok, err := path.Match(pSegs[0], rSegs[0])
	if err != nil || !ok {
		return false
	}
	return matchSegments(pSegs[1:], rSegs[1:])
}

func matchesPathOrAncestor(pattern, rel string, fold bool) bool {
	if matchGlobInternal(pattern, rel, fold) {
		return true
	}
	parts := strings.Split(rel, "/")
	for i := 1; i < len(parts); i++ {
		if matchGlobInternal(pattern, strings.Join(parts[:i], "/"), fold) {
			return true
		}
	}
	return false
}

// MatchDenyPathIndex returns the index of the first pattern in denyPaths that
// matches rel (case-insensitively), or -1 if none match (FAE-SEC-009).
func MatchDenyPathIndex(denyPaths []string, rel string) int {
	r := strings.ReplaceAll(strings.TrimSpace(rel), "\\", "/")
	r = strings.TrimPrefix(r, "./")
	for i, pat := range denyPaths {
		if matchesPathOrAncestor(pat, r, true) {
			return i
		}
	}
	return -1
}

// Allows reports whether rel is permitted by p. A nil ReadPolicy permits all
// valid relative paths. Deny always wins over Allow.
func (p *ReadPolicy) Allows(rel string) bool {
	cleaned, ok := CleanRelPath(rel)
	if !ok {
		return false
	}
	if p == nil {
		return true
	}
	for _, d := range p.Deny {
		if matchesPathOrAncestor(d, cleaned, false) {
			return false
		}
	}
	if len(p.AllowSets) > 0 {
		for _, set := range p.AllowSets {
			if !matchesAnyAllow(set, cleaned) {
				return false
			}
		}
		return true
	}
	if len(p.Allow) > 0 {
		return matchesAnyAllow(p.Allow, cleaned)
	}
	return true
}

func matchesAnyAllow(allow []string, rel string) bool {
	for _, a := range allow {
		if matchesPathOrAncestor(a, rel, false) {
			return true
		}
	}
	return false
}

// AllowsPath checks both target denyPaths (FAE-SEC-009) and ReadPolicy p (FAE-SEC-008).
func AllowsPath(p *ReadPolicy, denyPaths []string, rel string) bool {
	if MatchDenyPathIndex(denyPaths, rel) >= 0 {
		return false
	}
	return p.Allows(rel)
}

// ValidateReadPolicy checks that a ReadPolicy is well-formed.
func ValidateReadPolicy(p *ReadPolicy) error {
	if p == nil {
		return nil
	}
	if len(p.Allow) == 0 && len(p.Deny) == 0 {
		return errors.New("read policy must declare allow or deny globs")
	}
	for _, g := range p.Allow {
		if !ValidGlob(g) {
			return fmt.Errorf("invalid allow glob %q", g)
		}
	}
	for _, g := range p.Deny {
		if !ValidGlob(g) {
			return fmt.Errorf("invalid deny glob %q", g)
		}
	}
	return nil
}

// globSamplePath turns a glob into a representative concrete path to test inclusion.
func globSamplePath(g string) string {
	s := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(g), "/"), "./")
	s = strings.ReplaceAll(s, "**", "sub/dir")
	s = strings.ReplaceAll(s, "*", "item")
	s = strings.ReplaceAll(s, "?", "x")
	return s
}

func globSubsetOfSingleAllow(child, parent string) bool {
	c := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(child), "/"), "./")
	p := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(parent), "/"), "./")
	if p == "**" || c == p {
		return true
	}
	if strings.HasSuffix(p, "/**") {
		prefix := strings.TrimSuffix(p, "/**")
		if c == prefix || strings.HasPrefix(c, prefix+"/") {
			return true
		}
	}
	sample := globSamplePath(c)
	if !matchesPathOrAncestor(p, sample, false) {
		return false
	}
	// Ensure wildcard child cannot escape a literal parent segment.
	pSegs := strings.Split(p, "/")
	cSegs := strings.Split(c, "/")
	for i := 0; i < len(pSegs) && i < len(cSegs); i++ {
		if pSegs[i] == "**" {
			return true
		}
		if cSegs[i] == "**" && pSegs[i] != "**" {
			return false
		}
	}
	return len(cSegs) >= len(pSegs) || pSegs[len(pSegs)-1] == "**"
}

func globSubsetOfAllow(child string, allow []string) bool {
	for _, a := range allow {
		if globSubsetOfSingleAllow(child, a) {
			return true
		}
	}
	return false
}

func globDeniedBy(g string, deny []string) bool {
	c := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(g), "/"), "./")
	sample := globSamplePath(c)
	for _, d := range deny {
		if c == strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(d), "/"), "./") {
			return true
		}
		if matchesPathOrAncestor(d, sample, false) {
			return true
		}
	}
	return false
}

// ValidateReadPolicyNarrowing verifies that override only narrows base (FAE-SEC-008):
// override may not allow a glob that is denied by base.Deny or outside base.Allow.
func ValidateReadPolicyNarrowing(base, override *ReadPolicy) error {
	if err := ValidateReadPolicy(override); err != nil {
		return err
	}
	if base == nil || override == nil {
		return nil
	}
	for _, g := range override.Allow {
		if globDeniedBy(g, base.Deny) {
			return fmt.Errorf("read policy allow %q widens path denied by role/base policy", g)
		}
		if len(base.Allow) > 0 && !globSubsetOfAllow(g, base.Allow) {
			return fmt.Errorf("read policy allow %q widens role/base allow policy", g)
		}
	}
	return nil
}

// ComposeReadPolicy combines policies from broadest (role) to narrowest (stage, task)
// and appends targetDenyPaths to Deny.
func ComposeReadPolicy(denyPaths []string, policies ...*ReadPolicy) *ReadPolicy {
	var allowSets [][]string
	var lastAllow []string
	var deny []string
	hasAny := len(denyPaths) > 0
	for _, p := range policies {
		if p == nil {
			continue
		}
		hasAny = true
		if len(p.AllowSets) > 0 {
			for _, set := range p.AllowSets {
				if len(set) > 0 {
					allowSets = append(allowSets, slices.Clone(set))
					lastAllow = slices.Clone(set)
				}
			}
		} else if len(p.Allow) > 0 {
			cp := slices.Clone(p.Allow)
			allowSets = append(allowSets, cp)
			lastAllow = cp
		}
		for _, d := range p.Deny {
			if !slices.Contains(deny, d) {
				deny = append(deny, d)
			}
		}
	}
	for _, d := range denyPaths {
		if !slices.Contains(deny, d) {
			deny = append(deny, d)
		}
	}
	if !hasAny {
		return nil
	}
	// Filter lastAllow against earlier allowSets so the serialized Allow list reflects the intersection.
	if len(allowSets) > 1 {
		var narrowed []string
		for _, candidate := range lastAllow {
			ok := true
			for _, set := range allowSets[:len(allowSets)-1] {
				if !globSubsetOfAllow(candidate, set) {
					ok = false
					break
				}
			}
			if ok {
				narrowed = append(narrowed, candidate)
			}
		}
		lastAllow = narrowed
	}
	return &ReadPolicy{
		Allow:     lastAllow,
		Deny:      deny,
		AllowSets: allowSets,
	}
}

// EffectiveRoleStageReadPolicy computes the effective ReadPolicy for a role at
// a stage under taskReads and target denyPaths.
func EffectiveRoleStageReadPolicy(d Document, taskClass, roleName, stageName string, taskReads *ReadPolicy, denyPaths []string) *ReadPolicy {
	var roleReads *ReadPolicy
	if roleName != "" {
		if r, ok := EffectiveRole(d, taskClass, roleName); ok {
			roleReads = r.Reads
		} else if r, ok := d.Roles[roleName]; ok {
			roleReads = r.Reads
		}
	}
	var stageReads *ReadPolicy
	if s, ok := d.Stages[stageName]; ok {
		stageReads = s.Reads
	}
	return ComposeReadPolicy(denyPaths, roleReads, stageReads, taskReads)
}

// CanBindStageReadPolicy reports whether a consumer stage with consumer policy
// may bind an output from a producer stage with producer policy (FAE-SEC-008).
// If the producer's effective read policy permitted paths that the consumer's
// policy denies, it returns false and the offending path/glob.
func CanBindStageReadPolicy(producer, consumer *ReadPolicy) (bool, string) {
	if consumer == nil {
		return true, ""
	}
	for _, d := range consumer.Deny {
		sample := globSamplePath(d)
		if producer.Allows(sample) {
			return false, d
		}
	}
	if len(consumer.Allow) > 0 {
		if producer == nil || len(producer.Allow) == 0 {
			return false, "*"
		}
		for _, pa := range producer.Allow {
			sample := globSamplePath(pa)
			if producer.Allows(sample) && !consumer.Allows(sample) {
				return false, pa
			}
		}
	}
	return true, ""
}

// StageReadRoots returns the deduplicated read_roots names referenced by a stage
// (via its role(s) or command bindings).
func StageReadRoots(d Document, taskClass string, s Stage) []string {
	var out []string
	add := func(name string) {
		if name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	for _, roleName := range stageRoles(s) {
		if r, ok := EffectiveRole(d, taskClass, roleName); ok {
			for _, root := range r.ReadRoots {
				add(root)
			}
		} else if r, ok := d.Roles[roleName]; ok {
			for _, root := range r.ReadRoots {
				add(root)
			}
		}
	}
	keys := make([]string, 0, len(s.Bindings))
	for k := range s.Bindings {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		b := s.Bindings[k]
		if b.IsReadRoot() {
			add(b.ReadRootKey())
		}
	}
	for _, t := range s.ArgTemplates {
		if t.Kind == "read-root" {
			add(t.Key)
		}
	}
	return out
}

func validateReadsAndPush(d Document, bad func(string, string)) {
	roleNames := make([]string, 0, len(d.Roles))
	for name := range d.Roles {
		roleNames = append(roleNames, name)
	}
	slices.Sort(roleNames)
	for _, name := range roleNames {
		r := d.Roles[name]
		if err := ValidateReadPolicy(r.Reads); err != nil {
			bad("roles."+name+".reads", err.Error())
		}
		seenRoots := map[string]bool{}
		for i, root := range r.ReadRoots {
			if strings.TrimSpace(root) == "" || seenRoots[root] {
				bad(fmt.Sprintf("roles.%s.read_roots[%d]", name, i), "read_root name must be non-empty and unique")
			}
			seenRoots[root] = true
		}
	}
	stageIDs := make([]string, 0, len(d.Stages))
	for id := range d.Stages {
		stageIDs = append(stageIDs, id)
	}
	slices.Sort(stageIDs)
	for _, id := range stageIDs {
		s := d.Stages[id]
		p := "stages." + id
		if s.Reads != nil {
			if err := ValidateReadPolicy(s.Reads); err != nil {
				bad(p+".reads", err.Error())
			} else {
				for _, roleName := range stageRoles(s) {
					if r, ok := d.Roles[roleName]; ok {
						if err := ValidateReadPolicyNarrowing(r.Reads, s.Reads); err != nil {
							bad(p+".reads", err.Error())
						}
					}
				}
			}
		}
		if s.IsGitPush() && s.Role != "" {
			bad(p+".role", "git-push ExternalEffect is executed by the engine and must not declare a role")
		}
	}
}
