package daemon

// IntegrateDisposition is the one FAE-VCS-009 disposition of a failed
// Integrate attempt. An interrupted attempt stays with restart recovery,
// which remeasures it; a proven no-write target move re-verifies against the
// current tip; a first unproven failure retries the idempotent integration
// once (containment first, then a compare-and-swap from the measured tip);
// anything else escalates with its typed reason. Nothing rests unowned.
func IntegrateDisposition(interrupted, provenMove, retried bool) string {
	switch {
	case interrupted:
		return "uncertain"
	case provenMove:
		return "reverify"
	case !retried:
		return "retry"
	}
	return "escalate"
}

// CommitDisposition is the one disposition of a failed task-branch commit:
// an interrupted write stays with restart recovery; a branch already at the
// prepared commit is adopted; an unchanged branch retries once; anything else
// escalates with its typed reason.
func CommitDisposition(interrupted, committed, unchanged, retried bool) string {
	switch {
	case interrupted:
		return "uncertain"
	case committed:
		return "committed"
	case unchanged && !retried:
		return "retry"
	}
	return "escalate"
}
