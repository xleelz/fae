package daemon

import (
	"encoding/json"
	"fmt"

	"github.com/xleelz/fae/controlplane"
)

// checkpointAudit names a drive checkpoint's round and task: its Dolt commit
// message is "drive round N checkpoint <task> by daemon" (FAE-CP-008).
func checkpointAudit(round uint64, task string) controlplane.Audit {
	action := fmt.Sprintf("drive round %d checkpoint %s", round, task)
	if len(action) > 255 {
		action = fmt.Sprintf("drive round %d checkpoint", round)
	}
	inputs, _ := json.Marshal(map[string]any{"round": round, "task": task})
	return controlplane.Audit{Actor: "daemon", Action: action, Inputs: inputs, Outcome: "ok"}
}
