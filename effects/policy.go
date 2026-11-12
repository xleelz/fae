package effects

import "github.com/xleelz/fae/workflow"

// CommitIdentity configures the Git author, committer, and optional Co-authored-by
// trailer for brokered commits and controller landings (FAE-VCS-012).
type CommitIdentity = workflow.CommitIdentity

// ContentPolicy enforces target content_policy (deny_literals, deny_patterns)
// and deny_paths across commits, integrations, landings, and git-push (FAE-SEC-009).
type ContentPolicy = workflow.ContentPolicy

// ScanGitRange scans every commit in baseRev..headRev against policy (FAE-SEC-009, FAE-EXT-023).
var ScanGitRange = workflow.ScanGitRange
