package daemon

import "github.com/xleelz/fae/effects"

// Each effect snapshots this setting before execution. Runtime updates affect
// later dispatches; an in-flight command keeps the policy it started with.
func (x *liveExecutor) effectiveNestedCommands() effects.NestedMode {
	if x.guard != nil {
		if value := x.guard.dynamicNested.Load(); value != nil {
			return *value
		}
	}
	if x.config.NestedCommands != "" {
		return effects.NestedMode(x.config.NestedCommands)
	}
	return effects.NestedRefuse
}
