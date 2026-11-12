package daemon

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// LandingPolicy is a target's declared landing policy (FAE-CTL-030): the
// known flaky tests the full-suite gate tolerates and the frozen-region check
// commands run on the merged tip. It lives in the pinned target revision, so a
// landing operator cannot widen it per landing.
type LandingPolicy struct {
	Flakes         []string        `json:"flakes,omitempty"`
	Frozen         [][]string      `json:"frozen,omitempty"`
	DocumentGlobs  []string        `json:"document_globs,omitempty"`
	DocumentChecks []DocumentCheck `json:"document_checks,omitempty"`
	RequiredTests  []RequiredTest  `json:"required_tests,omitempty"`
}

var flakeName = regexp.MustCompile(`^([A-Za-z0-9_./-]+:)?Test[A-Za-z0-9_]*$`)

func validateLandingPolicy(c TargetConfig) error {
	l := c.Landing
	if l == nil {
		return nil
	}
	if c.SchemaVersion < 4 {
		return errors.New("a landing policy requires target schema 4")
	}
	if len(l.Flakes) > 100 || len(l.Frozen) > 32 {
		return errors.New("a landing policy lists at most 100 flakes and 32 frozen checks")
	}
	for i, f := range l.Flakes {
		if !flakeName.MatchString(f) || slices.Contains(l.Flakes[:i], f) {
			return fmt.Errorf("landing flake %q must be a unique <Test> or <package>:<Test>", f)
		}
	}
	for _, argv := range l.Frozen {
		if len(argv) == 0 || !slices.ContainsFunc(c.AllowedCommands, func(shape []string) bool { return slices.Equal(shape, argv) }) {
			return fmt.Errorf("frozen check %q must be a literal allowlisted command", strings.Join(argv, " "))
		}
	}
	return nil
}

// pinnedLandingPolicy reads the landing policy of the target revision the task
// pinned at admission; a task without a target tolerates no flake and runs no
// frozen check.
func (d *Daemon) pinnedLandingPolicy(ctx context.Context, a AdmitParams) (LandingPolicy, error) {
	if a.TargetConfig == "" {
		return LandingPolicy{}, nil
	}
	_, raw, err := d.store.GetTarget(ctx, a.TargetConfig, a.TargetRevision)
	if err != nil {
		return LandingPolicy{}, err
	}
	c, err := parseTarget(raw)
	if err != nil {
		return LandingPolicy{}, err
	}
	var lp LandingPolicy
	if c.Landing != nil {
		lp = *c.Landing
	}
	if len(lp.DocumentGlobs) == 0 && len(c.DocumentGlobs) > 0 {
		lp.DocumentGlobs = slices.Clone(c.DocumentGlobs)
	}
	if len(lp.DocumentChecks) == 0 && len(c.DocumentChecks) > 0 {
		lp.DocumentChecks = slices.Clone(c.DocumentChecks)
	}
	if len(lp.RequiredTests) == 0 && len(c.RequiredTests) > 0 {
		lp.RequiredTests = slices.Clone(c.RequiredTests)
	}
	return lp, nil
}
