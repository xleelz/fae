package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
)

// DefaultWorkflowStatus reports how startup reconciled the stored default
// workflow with the shipped one (status field default_workflow).
type DefaultWorkflowStatus struct {
	State        string `json:"state"`
	Revision     int64  `json:"revision"`
	Hash         string `json:"hash"`
	Generation   int    `json:"generation"`
	UpgradedFrom int64  `json:"upgraded_from,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

// builtinName records the shipped generation in a builtin revision's name;
// a builtin revision named only by its id predates generations (1).
func builtinName(id string, generation int) string {
	return fmt.Sprintf("%s (builtin generation %d)", id, generation)
}
func builtinGeneration(id, name string) int {
	if name == id {
		return 1
	}
	rest, ok := strings.CutPrefix(name, id+" (builtin generation ")
	n, err := strconv.Atoi(strings.TrimSuffix(rest, ")"))
	if !ok || !strings.HasSuffix(rest, ")") || err != nil || n < 1 {
		return 0
	}
	return n
}

// defaultUpgrade decides whether the shipped default becomes a new revision.
// The latest stored revision is replaced when it fails current validation (no
// task can bind to it) or is a daemon-installed builtin of an older
// generation. A valid operator revision, a retired workflow and a builtin of
// the same or a newer generation (a rolled-back binary) are kept.
func defaultUpgrade(latest controlplane.WorkflowListing, shippedHash string, generation int, invalid error) (bool, string, string) {
	builtin := latest.CreatedBy == "daemon"
	g := builtinGeneration(latest.WorkflowID, latest.Name)
	upgrade := MayInstallShippedDefault(latest.Retired, latest.Hash != shippedHash, invalid != nil, builtin, g >= 1 && g < generation)
	switch {
	case latest.Retired:
		return upgrade, "retired", "the default workflow is retired; no revision installed"
	case latest.Hash == shippedHash:
		return upgrade, "current", ""
	case invalid != nil:
		return upgrade, "upgraded", fmt.Sprintf("revision %d fails current validation: %v", latest.Revision, invalid)
	case !builtin:
		return upgrade, "operator-revision", fmt.Sprintf("revision %d is an operator revision; the shipped default is not installed over it", latest.Revision)
	case upgrade:
		return upgrade, "upgraded", fmt.Sprintf("builtin revision %d (generation %d) predates generation %d", latest.Revision, g, generation)
	}
	return upgrade, "kept", fmt.Sprintf("builtin revision %d is not older than generation %d", latest.Revision, generation)
}

// installDefault installs the shipped default as revision 1 at first start
// and, through an audited write, as a new revision when defaultUpgrade says
// so (FAE-WF-080, FAE-WF-093). Tasks keep their pinned revisions.
func (d *Daemon) installDefault(ctx context.Context) error {
	doc := defaults.Shipped()
	if d.live != nil && d.live.guard.panes != nil {
		if err := validatePaneFloor(doc, d.live.guard.effectivePaneLimit()); err != nil {
			return err
		}
	}
	raw, _ := json.Marshal(doc)
	hash, _, err := controlplane.ContentHash(raw)
	if err != nil {
		return err
	}
	rows, err := d.store.ListWorkflows(ctx, doc.ID)
	if err != nil {
		return err
	}
	status := DefaultWorkflowStatus{State: "installed", Hash: hash, Generation: defaults.Generation}
	audit := controlplane.Audit{Actor: "daemon", Action: "install builtin workflow", Outcome: "ok"}
	if len(rows) > 0 {
		latest := rows[len(rows)-1]
		_, stored, err := d.store.GetWorkflow(ctx, doc.ID, latest.Revision)
		if err != nil {
			return err
		}
		upgrade, state, reason := defaultUpgrade(latest, hash, defaults.Generation, validWorkflow(stored))
		status = DefaultWorkflowStatus{State: state, Revision: latest.Revision, Hash: latest.Hash, Generation: builtinGeneration(doc.ID, latest.Name), Reason: reason}
		if latest.CreatedBy != "daemon" {
			status.Generation = 0
		}
		if !upgrade {
			d.defaultWorkflow.Store(status)
			return nil
		}
		inputs, _ := json.Marshal(map[string]any{"from_revision": latest.Revision, "from_hash": latest.Hash, "generation": defaults.Generation, "reason": reason})
		audit = controlplane.Audit{Actor: "daemon", Action: "upgrade builtin workflow", Inputs: inputs, Outcome: "ok"}
		status = DefaultWorkflowStatus{State: state, Hash: hash, Generation: defaults.Generation, UpgradedFrom: latest.Revision, Reason: reason}
	}
	footprint, err := d.resolveFootprint(ctx, doc, 0)
	if err != nil {
		return err
	}
	footprintJSON, _ := json.Marshal(footprint)
	r, err := d.store.WriteWorkflow(ctx, audit, doc.ID, builtinName(doc.ID, defaults.Generation), raw, validWorkflow, footprintJSON)
	if err != nil {
		return err
	}
	status.Revision, status.Hash = r.Number, r.Hash
	d.defaultWorkflow.Store(status)
	d.startupRevisionInstalled(doc.ID, status.UpgradedFrom > 0)
	return nil
}
