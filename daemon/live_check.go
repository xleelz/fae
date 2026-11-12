package daemon

import (
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// CheckLiveConfig is the offline part of the live configuration's startup
// validation, for fae doctor (FAE-CLI-054): every issue it can find without
// a plane or an adapter, each named. Startup still validates in full.
func CheckLiveConfig(c LiveConfig) []string {
	issues := []string{}
	note := func(err error) {
		if err != nil {
			issues = append(issues, err.Error())
		}
	}
	note(validateTicketConfig(c))
	for alias, tool := range c.ExternalTools {
		if err := tool.Validate(); err != nil {
			issues = append(issues, "external tool "+alias+": "+err.Error())
		}
	}
	if c.ResidentSessions && c.Adapter != "" && c.Adapter != "multiplexer" {
		note(errors.New("resident sessions require Herdr multiplexer"))
	}
	if c.StartSeconds < 0 || c.StallSeconds < 0 {
		note(errors.New("start_seconds and stall_seconds must be nonnegative"))
	}
	note(validateLiveTasks(c.Tasks))
	for _, err := range liveCommandBoundsIssues(c) {
		note(err)
	}
	note(validateLiveScopeConfig(c))
	if len(c.Command) == 0 || !filepath.IsAbs(c.Command[0]) || c.Base == "" || c.TimeoutSeconds < 1 || c.HeavyCapacity < 1 || !filepath.IsAbs(c.HostLedger) {
		note(errors.New("live config requires absolute bridge and shared host ledger, base, positive timeout/capacity"))
	} else if info, err := os.Stat(c.Command[0]); err != nil || info.IsDir() || info.Mode().Perm()&0111 == 0 {
		note(errors.New("bridge " + c.Command[0] + " is not an executable file"))
	}
	if decoded, err := hex.DecodeString(c.Base); c.Base != "" && (err != nil || (len(decoded) != 20 && len(decoded) != 32)) {
		note(errors.New("live base must be an immutable full commit SHA"))
	}
	if len(c.Command) > 0 && !slices.Contains(c.Command, "{config}") {
		note(errors.New("bridge command must contain exact {config} argument"))
	}
	if !strings.HasPrefix(c.IntegrationRef, "refs/heads/fae/") {
		note(errors.New("live integration requires a dedicated refs/heads/fae/ target"))
	}
	return issues
}

// validateLiveScopeConfig checks the instance-level scope. Non-empty files
// pin every task without a tasks override; each, and a set test_file, follows
// the one declared-path rule (DeclaredPathSafe). Empty files select derived
// scope: each task's scope comes from its own admission and evidence, so an
// instance-level RED identity belongs to no task and is refused.
func validateLiveScopeConfig(c LiveConfig) error {
	if len(c.Files) == 0 && (c.TestFile != "" || c.TestID != "" || c.ExpectedFailure != "") {
		return errors.New("derived task scope (empty files) refuses an instance-level test_file, test_id or expected_failure")
	}
	paths := slices.Clone(c.Files)
	if c.TestFile != "" {
		paths = append(paths, c.TestFile)
	}
	for _, name := range paths {
		if !DeclaredPathSafe(name) {
			return errors.New("instance files and test_file must each be a clean relative path inside the worktree, outside .git: " + strconv.Quote(name))
		}
	}
	return nil
}
