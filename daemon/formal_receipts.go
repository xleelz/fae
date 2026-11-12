package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
)

// Formal receipts are recorded before their artifact can satisfy a gate. The
// run ID binds them to the durable command, tree, output digest and timing row.
func (x *liveExecutor) recordFormalReceipt(ctx context.Context, d engine.Dispatch, runID string, argv []string, run effects.RunEvidence) error {
	if run.Proof == nil {
		return nil
	}
	raw, err := json.Marshal(map[string]any{"run_id": runID, "task_id": d.TaskID, "dispatch_id": d.ID, "stage": d.Stage, "command": argv, "tree": run.Tree, "executable_digest": run.ExecutableDigest, "output_digest": run.OutputDigest, "runtime_mounts": run.RuntimeMounts, "proof": run.Proof})
	if err != nil {
		return err
	}
	return x.daemon.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "record formal verification receipt", Outcome: string(run.Proof.Outcome)}, "formal-proof/"+runID, raw)
}
