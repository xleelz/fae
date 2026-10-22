package scheduler

// TightenPaneLimit reconciles cooperating instance limits without allowing a
// more permissive instance to erase another live instance's admission ceiling.
// @ requires current >= 0 && requested >= 0
// @ ensures limit <= current && limit <= requested
// @ ensures limit == current || limit == requested
func TightenPaneLimit(current, requested int) (limit int) {
	if requested < current {
		return requested
	}
	return current
}
