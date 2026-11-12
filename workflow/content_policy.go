package workflow

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// CommitIdentity configures the Git author, committer, and optional Co-authored-by
// trailer for brokered commits and controller landings (FAE-VCS-012).
type CommitIdentity struct {
	Name     string `json:"name,omitempty"`
	Email    string `json:"email,omitempty"`
	CoAuthor string `json:"co_author,omitempty"`
}

// GitEnv returns the GIT_AUTHOR_* and GIT_COMMITTER_* environment variables.
func (c *CommitIdentity) GitEnv() []string {
	name, email := "Fae", "fae@localhost"
	if c != nil {
		if strings.TrimSpace(c.Name) != "" {
			name = strings.TrimSpace(c.Name)
		}
		if strings.TrimSpace(c.Email) != "" {
			email = strings.TrimSpace(c.Email)
		}
	}
	return []string{
		"GIT_AUTHOR_NAME=" + name,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name,
		"GIT_COMMITTER_EMAIL=" + email,
	}
}

// FormatMessage appends a Co-authored-by trailer when configured and not already present.
func (c *CommitIdentity) FormatMessage(message string) string {
	msg := strings.TrimRight(message, " \t\r\n")
	if c == nil || strings.TrimSpace(c.CoAuthor) == "" {
		return msg
	}
	trailer := "Co-authored-by: " + strings.TrimSpace(c.CoAuthor)
	if strings.Contains(strings.ToLower(msg), strings.ToLower(trailer)) {
		return msg
	}
	return msg + "\n\n" + trailer
}

// ContentPolicy enforces target content_policy (deny_literals, deny_patterns)
// and deny_paths across commits, integrations, landings, and git-push (FAE-SEC-009).
type ContentPolicy struct {
	DenyLiterals []string `json:"deny_literals,omitempty"`
	DenyPatterns []string `json:"deny_patterns,omitempty"`
	DenyPaths    []string `json:"deny_paths,omitempty"`
}

// IsEmpty reports whether no content or path policy rules are configured.
func (p ContentPolicy) IsEmpty() bool {
	return len(p.DenyLiterals) == 0 && len(p.DenyPatterns) == 0 && len(p.DenyPaths) == 0
}

// Validate checks that every pattern compiles as a case-insensitive regular
// expression and every deny_paths entry is a valid glob.
func (p ContentPolicy) Validate() error {
	for i, lit := range p.DenyLiterals {
		if strings.TrimSpace(lit) == "" {
			return fmt.Errorf("content_policy.deny_literals[%d]: empty literal", i)
		}
	}
	for i, pat := range p.DenyPatterns {
		if strings.TrimSpace(pat) == "" {
			return fmt.Errorf("content_policy.deny_patterns[%d]: empty pattern", i)
		}
		if _, err := regexp.Compile("(?i)" + pat); err != nil {
			return fmt.Errorf("content_policy.deny_patterns[%d]: invalid regex", i)
		}
	}
	for i, g := range p.DenyPaths {
		if !ValidGlob(g) {
			return fmt.Errorf("deny_paths[%d]: invalid glob", i)
		}
	}
	return nil
}

// ScanCommit scans a commit message (including trailers), touched file paths,
// and unified diff added lines against DenyPaths, DenyLiterals, and DenyPatterns.
// On match it returns an error naming ONLY the matching entry index, never the
// matched secret or pattern text (FAE-SEC-009).
func (p ContentPolicy) ScanCommit(message string, touchedPaths []string, diff string) error {
	if p.IsEmpty() {
		return nil
	}
	for _, path := range touchedPaths {
		if idx := MatchDenyPathIndex(p.DenyPaths, path); idx >= 0 {
			return fmt.Errorf("content policy refused: deny_paths[%d]", idx)
		}
	}
	var addedLines []string
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "+++ ") || line == "+++" {
			continue
		}
		if strings.HasPrefix(line, "+") {
			addedLines = append(addedLines, line[1:])
		}
	}
	texts := make([]string, 0, 1+len(touchedPaths)+len(addedLines))
	if message != "" {
		texts = append(texts, message)
	}
	texts = append(texts, touchedPaths...)
	texts = append(texts, addedLines...)

	for i, lit := range p.DenyLiterals {
		needle := strings.ToLower(lit)
		if needle == "" {
			continue
		}
		for _, hay := range texts {
			if strings.Contains(strings.ToLower(hay), needle) {
				return fmt.Errorf("content policy refused: content_policy.deny_literals[%d]", i)
			}
		}
	}
	for i, pat := range p.DenyPatterns {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			return fmt.Errorf("content policy refused: content_policy.deny_patterns[%d]", i)
		}
		for _, hay := range texts {
			if re.MatchString(hay) {
				return fmt.Errorf("content policy refused: content_policy.deny_patterns[%d]", i)
			}
		}
	}
	return nil
}

// ScanGitRange scans every commit in baseRev..headRev (or all commits reachable
// from headRev when baseRev is empty or all-zero) plus the aggregate tree diff
// against policy (FAE-SEC-009, FAE-EXT-023).
func ScanGitRange(ctx context.Context, repoDir, baseRev, headRev string, policy ContentPolicy) error {
	if policy.IsEmpty() {
		return nil
	}
	if headRev == "" {
		return errors.New("head revision required for content policy scan")
	}
	runGit := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", repoDir}, args...)...)
		var out, errBuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(errBuf.String()))
		}
		return strings.TrimSpace(out.String()), nil
	}
	zeroRef := strings.Trim(baseRev, "0") == ""
	revSpec := headRev
	if !zeroRef {
		revSpec = baseRev + ".." + headRev
	}
	revsOut, err := runGit("rev-list", revSpec)
	if err != nil {
		return err
	}
	for _, sha := range strings.Fields(revsOut) {
		msg, err := runGit("log", "-1", "--format=%B", sha)
		if err != nil {
			return err
		}
		namesOut, err := runGit("diff-tree", "--no-commit-id", "--name-only", "-r", "-m", "--root", sha)
		if err != nil {
			return err
		}
		var paths []string
		for _, line := range strings.Split(namesOut, "\n") {
			if p := strings.TrimSpace(line); p != "" {
				paths = append(paths, p)
			}
		}
		diff, err := runGit("show", "--format=", "--unified=0", sha)
		if err != nil {
			return err
		}
		if err := policy.ScanCommit(msg, paths, diff); err != nil {
			return err
		}
	}
	if !zeroRef {
		namesOut, err := runGit("diff", "--name-only", baseRev, headRev)
		if err == nil {
			var paths []string
			for _, line := range strings.Split(namesOut, "\n") {
				if p := strings.TrimSpace(line); p != "" {
					paths = append(paths, p)
				}
			}
			diff, err := runGit("diff", "--unified=0", baseRev, headRev)
			if err == nil {
				if err := policy.ScanCommit("", paths, diff); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
