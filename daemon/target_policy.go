package daemon

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/xleelz/fae/workflow"
)

// effectiveTargetDocumentPolicy resolves DocumentGlobs and DocumentChecks from
// TargetConfig, falling back between top-level fields and c.Landing (FAE-CTL-052).
func effectiveTargetDocumentPolicy(c TargetConfig) ([]string, []DocumentCheck) {
	globs := slices.Clone(c.DocumentGlobs)
	checks := slices.Clone(c.DocumentChecks)
	if c.Landing != nil {
		if len(globs) == 0 && len(c.Landing.DocumentGlobs) > 0 {
			globs = slices.Clone(c.Landing.DocumentGlobs)
		}
		if len(checks) == 0 && len(c.Landing.DocumentChecks) > 0 {
			checks = slices.Clone(c.Landing.DocumentChecks)
		}
	}
	return globs, checks
}

// parseTargetPolicy extracts a LandingPolicy from TargetConfig, falling back
// to top-level DocumentGlobs, DocumentChecks, and RequiredTests when not nested
// under c.Landing (FAE-CTL-052).
func parseTargetPolicy(c TargetConfig) (LandingPolicy, error) {
	if err := validateTargetPolicy(c); err != nil {
		return LandingPolicy{}, err
	}
	var lp LandingPolicy
	if c.Landing != nil {
		lp = *c.Landing
	}
	globs, checks := effectiveTargetDocumentPolicy(c)
	if len(lp.DocumentGlobs) == 0 && len(globs) > 0 {
		lp.DocumentGlobs = globs
	}
	if len(lp.DocumentChecks) == 0 && len(checks) > 0 {
		lp.DocumentChecks = checks
	}
	if len(lp.RequiredTests) == 0 && len(c.RequiredTests) > 0 {
		lp.RequiredTests = slices.Clone(c.RequiredTests)
	}
	return lp, nil
}

// validateTargetPolicy checks the schema-4 policy sections. Earlier schemas
// must not carry them: a stored v1-v3 revision keeps its exact meaning.
func validateTargetPolicy(c TargetConfig) error {
	docGlobs, docChecks := effectiveTargetDocumentPolicy(c)
	if c.SchemaVersion < 4 {
		if len(c.RequiredTests) > 0 || c.CompileCheck != nil || c.DiffClassifier != nil || len(docGlobs) > 0 || len(docChecks) > 0 {
			return errors.New("required tests, compile check and diff classifier require target schema 4")
		}
		return nil
	}
	for _, g := range docGlobs {
		if !validGlob(g) && !workflow.ValidWriteGlob(g) {
			return fmt.Errorf("invalid document_globs pattern %q", g)
		}
	}
	if len(docChecks) > 0 && len(docGlobs) == 0 {
		return errors.New("document_checks require at least one document_globs pattern")
	}
	docIDs := map[string]bool{}
	for _, dc := range docChecks {
		if dc.ID == "" || docIDs[dc.ID] {
			return errors.New("document check needs a unique id")
		}
		docIDs[dc.ID] = true
		if len(dc.Command) == 0 || dc.Command[0] == "" {
			return fmt.Errorf("document check %s requires a non-empty command vector", dc.ID)
		}
		if err := workflow.ValidateCheckSpec(workflow.CheckSpec{
			Format:     dc.Format,
			Output:     dc.Output,
			CountField: dc.CountField,
			Writes:     dc.Writes,
		}); err != nil {
			return fmt.Errorf("document check %s: %w", dc.ID, err)
		}
	}
	command := func(what string, argv []string, timeout int) error {
		if len(argv) == 0 || timeout < 1 || timeout > 86400 || !slices.ContainsFunc(c.AllowedCommands, func(shape []string) bool { return slices.Equal(shape, argv) }) {
			return fmt.Errorf("%s must be a literal allowlisted vector with a timeout of 1..86400 s", what)
		}
		return nil
	}
	names := map[string]bool{}
	for _, t := range c.RequiredTests {
		if t.Name == "" || names[t.Name] {
			return errors.New("required test needs a unique name")
		}
		names[t.Name] = true
		if err := command("required test "+t.Name, t.Argv, t.TimeoutSeconds); err != nil {
			return err
		}
		for _, p := range t.Paths {
			if workflow.CheckArg("relative-path", strings.TrimSuffix(p, "/")) != nil {
				return fmt.Errorf("required test %s has an unsafe path scope %q", t.Name, p)
			}
		}
	}
	if c.CompileCheck != nil {
		if err := command("compile check", c.CompileCheck.Argv, c.CompileCheck.TimeoutSeconds); err != nil {
			return err
		}
	}
	if k := c.DiffClassifier; k != nil {
		if len(k.Code) == 0 {
			return errors.New("diff classifier requires at least one code pattern")
		}
		for _, p := range append(append([]string(nil), k.Code...), k.NotCode...) {
			if !validGlob(p) {
				return fmt.Errorf("invalid diff classifier pattern %q", p)
			}
		}
	}
	return nil
}

// targetAllowsStage decides admission: a literal stage needs an identical
// shape; a templated one a shape whose typed slots hold its placeholders.
func targetAllowsStage(c TargetConfig, s workflow.Stage) bool {
	for _, shape := range c.AllowedCommands {
		templated := len(s.Bindings) > 0 || len(s.ArgTemplates) > 0
		if !templated && slices.Equal(shape, s.Command) || templated && (slices.Equal(shape, s.Command) || workflow.MatchTemplate(shape, s)) {
			return true
		}
	}
	return false
}

// targetAllowsVector validates the exact bound vector at execution (FAE-WF-035).
func targetAllowsVector(c TargetConfig, argv []string) bool {
	for _, shape := range c.AllowedCommands {
		if workflow.MatchShape(shape, argv) {
			return true
		}
	}
	return false
}

// requiredFor selects the always-required tests whose path scope the task's
// changed paths touch; an unscoped test always runs (FAE-WF-085).
func requiredFor(c TargetConfig, changed []string) []RequiredTest {
	var out []RequiredTest
	for _, t := range c.RequiredTests {
		selected := len(t.Paths) == 0
		for _, prefix := range t.Paths {
			for _, name := range changed {
				selected = selected || strings.HasPrefix(name, prefix)
			}
		}
		if selected {
			out = append(out, t)
		}
	}
	return out
}

// classifyDiff returns the changed paths the classifier calls production code.
func classifyDiff(k DiffClassifier, changed []string) []string {
	var code []string
	for _, name := range changed {
		matches := func(patterns []string) bool {
			return slices.ContainsFunc(patterns, func(p string) bool { return matchGlob(p, name) })
		}
		if matches(k.Code) && !matches(k.NotCode) {
			code = append(code, name)
		}
	}
	return code
}

func validGlob(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || strings.Contains(p, "..") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" {
			return false
		}
		if seg != "**" {
			if _, err := path.Match(seg, ""); err != nil {
				return false
			}
		}
	}
	return true
}

// matchGlob matches slash-separated paths: "*" and "?" stay within one
// segment and a "**" segment spans any number of segments, including none.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}
func matchSegments(p, n []string) bool {
	if len(p) == 0 {
		return len(n) == 0
	}
	if p[0] == "**" {
		for i := 0; i <= len(n); i++ {
			if matchSegments(p[1:], n[i:]) {
				return true
			}
		}
		return false
	}
	if len(n) == 0 {
		return false
	}
	ok, err := path.Match(p[0], n[0])
	return err == nil && ok && matchSegments(p[1:], n[1:])
}
