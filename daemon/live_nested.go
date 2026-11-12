package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/effects"
)

// The live config's range for max_output_bytes; 0 keeps the broker default.
const (
	liveMinOutputBytes = 1 << 20
	liveMaxOutputBytes = 64 << 20
)

// liveNestedBroker is the fae executable account mode's nested-command
// policy runs in (effects.Broker.NestedBroker): the running executable when it
// is the fae binary, else "". Tests that run a daemon in account mode point
// it at a built fae.
var liveNestedBroker = commandshim.SelfBroker

// liveCommandBoundsIssues checks the live config's nested_commands mode and
// max_output_bytes bound. Setup refuses on any; CheckLiveConfig names each.
// Account mode also needs the fae executable, which applies the nested-command
// policy (`heavy-op run --shim-policy`).
func liveCommandBoundsIssues(c LiveConfig) []error {
	var issues []error
	switch effects.NestedMode(c.NestedCommands) {
	case "", effects.NestedRefuse:
	case effects.NestedAccount:
		if liveNestedBroker() == "" {
			issues = append(issues, fmt.Errorf("nested_commands %q needs the fae executable, whose heavy-op run applies the nested-command policy; run the daemon as the fae binary", c.NestedCommands))
		}
	default:
		issues = append(issues, fmt.Errorf("nested_commands %q must be %q or %q", c.NestedCommands, effects.NestedRefuse, effects.NestedAccount))
	}
	if c.MaxOutputBytes != 0 && (c.MaxOutputBytes < liveMinOutputBytes || c.MaxOutputBytes > liveMaxOutputBytes) {
		issues = append(issues, fmt.Errorf("max_output_bytes %d must be between %d (1 MiB) and %d (64 MiB); omit it for the 4 MiB default", c.MaxOutputBytes, liveMinOutputBytes, liveMaxOutputBytes))
	}
	return issues
}

// nestedCommandsKey names the durable record of one account-mode run's nested
// calls. Like an engine run row's identity (controlplane.EngineRunID) it is
// the digest of the run's dispatch, command and start, so every run has a
// record of its own, whatever its output. The start is the broker's
// (effects.RunEvidence.Started), so the key is not the engine run row's ID;
// the record names the task and dispatch, and its command and output digest
// match the row's.
func nestedCommandsKey(dispatchID string, command []string, started time.Time) string {
	return "nested-commands/" + controlplane.EngineRunID(controlplane.EngineRun{DispatchID: dispatchID, Command: command, Started: started})
}

// nestedCommandsRecord is the durable record of one account-mode host
// command run's nested calls (FAE-HOST-014).
type nestedCommandsRecord struct {
	TaskID       string                   `json:"task_id"`
	DispatchID   string                   `json:"dispatch_id"`
	Command      []string                 `json:"command"`
	Started      time.Time                `json:"started"`
	OutputDigest string                   `json:"output_digest"`
	Tree         string                   `json:"tree"`
	Count        int                      `json:"count"`
	Calls        []commandshim.NestedCall `json:"calls"`
	// CallsWithheld is why the calls keep only their times: the secrets
	// their arguments and directories must be redacted of could not be read.
	CallsWithheld string `json:"calls_withheld,omitempty"`
}

// recordNestedCommands is the live broker's RecordNested: it stores the
// run's nested calls before the run's evidence is used, so a nested
// invocation shows as accounted in durable state. The task and dispatch come
// from the run's context (effects.WithNestedRun). Every secret the daemon
// holds is replaced in each call's arguments and directory before it is
// stored (FAE-SEC-003; redactedNestedCalls); the command is the run's own,
// as its engine run row records it. A run that cannot be recorded is not
// evidence.
func (d *Daemon) recordNestedCommands(ctx context.Context, run effects.RunEvidence) error {
	id, _ := effects.NestedRunOf(ctx)
	calls, withheld := d.redactedNestedCalls(run.Nested)
	rec := nestedCommandsRecord{TaskID: id.TaskID, DispatchID: id.DispatchID, Command: run.Command, Started: run.Started, OutputDigest: run.OutputDigest, Tree: run.Tree, Count: run.NestedCount, Calls: calls, CallsWithheld: withheld}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	inputs, err := json.Marshal(map[string]string{"task": id.TaskID, "dispatch": id.DispatchID})
	if err != nil {
		return err
	}
	return d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "account nested commands", Inputs: inputs, Outcome: "ok"}, nestedCommandsKey(id.DispatchID, run.Command, run.Started), raw)
}
