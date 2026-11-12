package daemon

import (
	"context"
	"errors"
	"github.com/xleelz/fae/processowner"
	"path/filepath"

	"github.com/xleelz/fae/hostguard"
	agentruntime "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/scheduler"
)

// Resident reservations outlive individual turn heavy/role leases. They are
// released only by the runtime after exact resident process retirement. A new
// resident pane passes the same pressure hold and bounded back-off as a
// one-shot pane (FAE-HOST-002, -005, -006). Its refusal withholds the turn's
// task by cause, as a one-shot pane's does: pane_limit at the pane limit,
// otherwise host_guard (FAE-SCH-010). Any failure, a refusal or a failed
// pressure, census or ledger step, launches nothing, so the runtime defers the
// turn as infrastructure, never as a startup retry charged to the provider
// binding (FAE-RT-004). A failure that is not a refusal is withheld as
// host_guard.
func (a *liveAdmission) ReserveResidentPane(ctx context.Context, residentID, socket string) error {
	if a.panes == nil || a.inventory == nil {
		return errors.New("resident pane requires live Herdr admission")
	}
	observe := a.observe
	if observe == nil {
		observe = liveObserve
	}
	pressure, err := a.pressure()
	if err != nil {
		return err
	}
	id := a.proofID(residentID)
	reserved := false
	r := scheduler.PaneReservation{ID: id, Owner: a.owner, Birth: a.birth, Socket: socket}
	step, checks, err := hostguard.Wait(ctx, pressure.clock, pressure.policy, func(ctx context.Context, n int) (hostguard.Step, error) {
		return a.paneStep(ctx, pressure, observe, id, residentID, n, r, &reserved)
	})
	if err != nil {
		return err
	}
	if step.Outcome != hostguard.OutcomeProceed {
		err = a.refuse("pressure", id, residentID, checks, step)
		if task := withheldTaskOf(ctx); task != "" {
			a.refused(task, withheldHostGuard, err)
		}
		return err
	}
	return nil
}

// residentReconciliationDeferred withholds a turn's task by its cause when the
// turn deferred because the retirement of its resident slot stayed unproven
// at launch (FAE-SCH-010, FAE-RT-012). The turn launched nothing and charged
// nothing; its next attempt runs on another slot.
func (a *liveAdmission) residentReconciliationDeferred(ctx context.Context, _ string, cause error) {
	if task := withheldTaskOf(ctx); task != "" {
		a.refused(task, withheldResidentUnretired, cause)
	}
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
