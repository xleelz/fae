package daemon

import (
	"slices"
	"strings"
	"testing"
)

// The instance-level files pin and its test_file follow the one path rule:
// startup and fae doctor refuse a path in .git at any depth, in any case.
func TestLiveInstanceFilesRefuseNestedGitMetadata(t *testing.T) {
	for _, c := range []LiveConfig{
		{Files: []string{"a.go", "sub/.git/config"}},
		{Files: []string{"a.go", ".GIT/config"}},
		{Files: []string{"a.go", "../escape.go"}},
		{Files: []string{"a.go", ""}},
		{Files: []string{"a.go", "a_test.go"}, TestFile: "vendor/x/.Git/a_test.go"},
	} {
		if err := validateLiveScopeConfig(c); err == nil {
			t.Errorf("instance scope %v %q accepted", c.Files, c.TestFile)
		}
		if issues := CheckLiveConfig(c); !slices.ContainsFunc(issues, func(issue string) bool { return strings.Contains(issue, "outside .git") }) {
			t.Errorf("doctor missed %v %q: %v", c.Files, c.TestFile, issues)
		}
	}
	if err := validateLiveScopeConfig(LiveConfig{Files: []string{"a.go", "a_test.go", ".gitignore", "x.git/y.go"}, TestFile: "a_test.go"}); err != nil {
		t.Fatal(err)
	}
}
