package daemon

// CanRetireContained requires a resolving commit and task-owned evidence.
// @ ensures result == (resolves && owned)
func CanRetireContained(resolves, owned bool) (result bool) {
	return resolves && owned
}
