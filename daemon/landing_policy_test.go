package daemon

import (
	"encoding/json"
	"strings"
	"testing"
)

// FAE-CTL-030: the target's landing policy is validated with the target.
func TestTargetLandingPolicyValidation(t *testing.T) {
	base := targetFixture("/repo")
	base.SchemaVersion = 4
	base.AllowedCommands = append(base.AllowedCommands, []string{"gofmt", "-l", "."})
	for _, tc := range []struct {
		edit func(*TargetConfig)
		want string
	}{
		{func(c *TargetConfig) {}, ""},
		{func(c *TargetConfig) {
			c.Landing = &LandingPolicy{Flakes: []string{"TestA", "pkg/x:TestB"}, Frozen: [][]string{{"gofmt", "-l", "."}}}
		}, ""},
		{func(c *TargetConfig) { c.SchemaVersion = 1; c.Landing = &LandingPolicy{Flakes: []string{"TestA"}} }, "schema 4"},
		{func(c *TargetConfig) { c.Landing = &LandingPolicy{Flakes: []string{"anything goes"}} }, "flake"},
		{func(c *TargetConfig) { c.Landing = &LandingPolicy{Flakes: []string{"TestA", "TestA"}} }, "unique"},
		{func(c *TargetConfig) { c.Landing = &LandingPolicy{Frozen: [][]string{{"rm", "-rf", "."}}} }, "allowlisted"},
	} {
		c := base
		tc.edit(&c)
		raw, _ := json.Marshal(c)
		_, err := parseTarget(raw)
		if (tc.want == "") != (err == nil) || err != nil && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%+v: %v (want %q)", c.Landing, err, tc.want)
		}
	}
}
