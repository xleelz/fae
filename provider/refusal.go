package provider

import (
	"errors"

	agentruntime "github.com/xleelz/fae/runtime"
)

// Refusal is a turn the bridge refuses with a class the runtime reports
// (FAE-WF-034, FAE-RT-004, FAE-RT-010): agentruntime.RefusedOutput when a
// completed provider turn's answer fails the schema, the edit shape or the
// edit rules; agentruntime.RefusedBinding when, before the provider is
// launched, this bridge cannot run the turn's binding as configured (another
// provider's binding, the model, the Claude effort, configuration directory
// or credentials, the provider executable or timeout);
// agentruntime.RefusedBeforeProvider when the task's scope, the brief, the
// role's policy or a local step refuses the turn before the provider is
// launched. Where Run stopped decides the class, never the provider's answer.
// Run records it in the session's log directory; ExitCode then maps it to the
// protocol's exit status.
type Refusal struct {
	Class    string
	Err      error
	recorded bool
}

func (r *Refusal) Error() string { return r.Err.Error() }
func (r *Refusal) Unwrap() error { return r.Err }

// ExitCode is the bridge process's exit status for Run's result: 0 on
// success, agentruntime.BridgeRefusalExit for a refusal Run recorded, else 1.
func ExitCode(err error) int {
	var refusal *Refusal
	switch {
	case err == nil:
		return 0
	case errors.As(err, &refusal) && refusal.recorded:
		return agentruntime.BridgeRefusalExit
	}
	return 1
}

// recordRefusal writes a refusal's record. A refusal that cannot be recorded
// is an untyped failure: the runtime then sees only a failed bridge.
func recordRefusal(l agentruntime.BridgeConfig, refusal *Refusal) error {
	if e := agentruntime.RecordBridgeRefusal(l, refusal.Class, refusal.Err.Error()); e != nil {
		return errors.Join(refusal.Err, e)
	}
	refusal.recorded = true
	return refusal
}
