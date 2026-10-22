package daemon

// CanFailoverProphet prevents a new launch before durable observation and reaping.
// @ ensures res == (observed && retired && withinBudget)
func CanFailoverProphet(observed bool, retired bool, withinBudget bool) (res bool) {
	return observed && retired && withinBudget
}
