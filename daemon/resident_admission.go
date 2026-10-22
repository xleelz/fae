package daemon

import (
	"context"
	"errors"
	"github.com/xleelz/fae/processowner"
	"path/filepath"
	"time"

	"github.com/xleelz/fae/hostguard"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/scheduler"
)

// Resident reservations outlive individual turn heavy/role leases. They are
// released only by the runtime after exact resident process retirement.
func (a *liveAdmission) ReserveResidentPane(ctx context.Context, residentID, socket string) error {
	if a.panes == nil || a.inventory == nil {
		return errors.New("resident pane requires live Herdr admission")
	}
	observe := a.observe
	if observe == nil {
		observe = hostguard.Observe
	}
	cfg := a.hostConfig()
	cfg.PaneLimit = a.effectivePaneLimit()
	return a.panes.Reserve(ctx, scheduler.PaneReservation{ID: a.proofID(residentID), Owner: a.owner, Birth: a.birth, Socket: socket, Reserved: time.Now().UTC()}, a.inventory, observe, cfg, time.Now().UTC())
}

func (a *liveAdmission) ReleaseResidentPane(ctx context.Context, residentID string) error {
	if a.panes == nil {
		return errors.New("resident pane ledger missing")
	}
	if err := a.panes.Release(ctx, a.proofID(residentID), a.owner, a.birth); err != nil {
		return err
	}
	root := filepath.Join(a.daemon.layout.Root, "sessions")
	return processowner.AcknowledgeLinkedRetirement(filepath.Join(root, ".processes"), root, residentID, a.owner.PID, a.birth)
}

// Unknown roles retain their resident until TTL or shutdown; pressure shedding
// is restricted to the stock pre-code global roles.
func classifyResident(l agentruntime.Launch) (global, preCode, protected bool) {
	global = l.SessionPolicy == "workflow-global"
	preCode = global && (l.Role == "spec" || l.Role == "planner")
	protected = l.Role == "oracle" || l.Role == "qa"
	return
}
