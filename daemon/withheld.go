package daemon

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/scheduler"
)

// Reasons a ready task was not dispatched in a drive round (FAE-SCH-010).
const (
	withheldTasksInFlight = "tasks_in_flight"
	withheldCap           = "cap"
	withheldHostGuard     = "host_guard"
	withheldPaneLimit     = "pane_limit"
	withheldEdge          = "edge"
	withheldDeferral      = "deferral"
	withheldRoute         = "route_unhealthy"
	withheldParked        = "parked"
	withheldMolecule      = "molecule_limit"
	// Every route of the task's role was refused by the bridge in this
	// route generation (FAE-RT-010); the task's reason names the refusals.
	withheldRouteRefused = "route_refused"
	// A resident turn deferred because its slot's transport is stopped or
	// removed but not yet proven retired (FAE-RT-012); the turn's next
	// attempt runs on another slot.
	withheldResidentUnretired = "resident_unretired"
	// A Pending task whose dependency rows could not be read.
	withheldEdgeUnread = "edge_unread"
	// A Pending task that changed while its release ran without the writer
	// lock; the next round decides it afresh.
	withheldReleaseChanged = "release_changed"
	// Rounds that end before deciding any task: the plane has not re-confirmed
	// the checkpoints after a restart, a checkpoint is not yet verifiable, the
	// instance drains, or one of the round's reads failed. A checkpoint left
	// unsettled mid-round, and a drain begun mid-round, withhold the tasks the
	// round has not yet decided.
	withheldPlaneUnconfirmed    = "plane_unconfirmed"
	withheldCheckpointUnsettled = "checkpoint_unsettled"
	withheldDraining            = "draining"
	withheldRoundRead           = "round_read"
	withheldProviderPool        = "provider_pool"
	withheldProviderQuota       = "provider_quota"
	withheldDispatchPaused      = "dispatch_paused"
)

// withheldSnapshot is the latest round's withheld tasks with counts by reason,
// and cumulative task-rounds withheld by reason.
type withheldSnapshot struct {
	Round  uint64
	Counts map[string]int
	Tasks  map[string]string
	Totals map[string]uint64
}

// withheldRound collects one drive round's withheld tasks.
type withheldRound struct {
	d       *Daemon
	blocked map[string][]string
	tasks   map[string]string
	pauses  []controlplane.DispatchPause
}

func (d *Daemon) withholding(blocked map[string][]string) *withheldRound {
	return &withheldRound{d: d, blocked: blocked, tasks: map[string]string{}}
}

func (w *withheldRound) guard() *liveAdmission {
	if w.d.live == nil {
		return nil
	}
	return w.d.live.guard
}

func (w *withheldRound) add(id, reason string) {
	if reason != "" {
		w.tasks[id] = reason
	}
}

// active classifies a dispatched task that is still waiting in admission.
func (w *withheldRound) active(id string) {
	if a := w.guard(); a != nil {
		w.add(id, a.withheldReason(id))
	}
}

// parked classifies a task the engine did not dispatch this round.
func (w *withheldRound) parked(id string, j engine.Job) {
	w.add(id, parkedReason(j, w.guard()))
}

// rest classifies the tasks a round does not decide: those left when the
// tasks-in-flight limit is reached or a drain begins, or every task of a round
// that ends early. A task the round would have dispatched is withheld for
// reason.
func (w *withheldRound) rest(ids []string, reason string) {
	for _, id := range ids {
		if w.d.active[id] {
			w.active(id)
			continue
		}
		var e envelope
		if decodeEnvelope(w.d.jobs[id].Payload, &e) != nil {
			continue
		}
		switch {
		case e.Job.State.Terminal() || e.Job.State == domain.Escalated:
		case e.Job.State == domain.Pending && len(w.blocked[id]) > 0:
			w.add(id, withheldEdge)
		case e.Job.Deferred || e.Job.Parked:
			w.parked(id, e.Job)
		default:
			w.add(id, reason)
		}
	}
}

// withholdRound records a round that ends before deciding any task: each
// task it would have decided is withheld for reason (FAE-SCH-010). A round
// ended by shutdown records nothing.
func (d *Daemon) withholdRound(reason string, blocked map[string][]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ctx.Err() != nil {
		return
	}
	w := d.withholding(blocked)
	w.rest(slices.Sorted(maps.Keys(d.jobs)), reason)
	w.finish()
}

// roundReadFailed ends a round whose own read failed. Only infrastructure
// ends the generation; any other failure withholds the round's tasks.
func (d *Daemon) roundReadFailed(fatal error, blocked map[string][]string) error {
	if fatal == nil {
		d.withholdRound(withheldRoundRead, blocked)
	}
	return fatal
}

// parkedReason names why the engine withheld a job. Terminal, escalated and
// operator-decision states are not ready and are not counted.
func parkedReason(j engine.Job, a *liveAdmission) string {
	switch {
	case j.State.Terminal() || j.State == domain.Escalated:
		return ""
	case j.Deferred:
		return withheldDeferral
	case !j.Parked || j.Reason == "HumanDecisionRequired":
		return ""
	case j.Reason == engine.ParkRoutesUnavailable:
		return withheldRoute
	case j.Reason == engine.ParkProviderQuota:
		return withheldProviderQuota
	case engine.RoutesUnavailableReason(j.Reason):
		return withheldRouteRefused
	case j.Reason == "ReviewPending":
		return withheldDeferral
	case j.Reason == "infrastructure-deferred":
		if a != nil {
			if r := a.refusalReason(j.TaskID); r != "" {
				return r
			}
		}
		return withheldHostGuard
	}
	return withheldParked
}

// finish publishes the round's counts and writes the round's trace entry:
// every round that withholds a task records it (FAE-SCH-010), and the first
// round after withholding ends records "none". It runs under the drive mutex.
func (w *withheldRound) finish() {
	d := w.d
	counts := map[string]int{}
	for _, r := range w.tasks {
		counts[r]++
	}
	prev := d.withheld.Load()
	totals := map[string]uint64{}
	if prev != nil {
		maps.Copy(totals, prev.Totals)
	}
	for r, n := range counts {
		totals[r] += uint64(n)
	}
	round := d.driveRounds.Load() + 1
	d.withheld.Store(&withheldSnapshot{Round: round, Counts: counts, Tasks: w.tasks, Totals: totals})
	d.publishStatus()
	if d.log == nil || len(w.tasks) == 0 && (prev == nil || len(prev.Tasks) == 0) {
		return
	}
	var byReason, byTask []string
	for _, r := range slices.Sorted(maps.Keys(counts)) {
		byReason = append(byReason, fmt.Sprintf("%s=%d", r, counts[r]))
	}
	for _, id := range slices.Sorted(maps.Keys(w.tasks)) {
		byTask = append(byTask, id+":"+w.tasks[id])
	}
	if len(byTask) == 0 {
		byReason, byTask = []string{"none"}, []string{"none"}
	}
	fmt.Fprintf(d.log, "drive withheld round=%d counts=%s tasks=%s\n", round, strings.Join(byReason, ","), strings.Join(byTask, ","))
}

// waiting records the reason a task's admission is waiting; "" clears it.
func (a *liveAdmission) waiting(task, reason string) {
	if reason == "" {
		a.waits.Delete(task)
		return
	}
	a.waits.Store(task, reason)
}

func (a *liveAdmission) withheldReason(task string) string {
	v, _ := a.waits.Load(task)
	r, _ := v.(string)
	return r
}

// refused records why a scheduled task's admission was deferred: the stage it
// was waiting in, or the pane limit.
func (a *liveAdmission) refused(task, stage string, err error) {
	if _, scheduled := a.schedule().molecule[task]; !scheduled {
		return
	}
	var refusal *scheduler.AdmissionRefusal
	if errors.As(err, &refusal) && refusal.Reason == "PaneLimit" {
		stage = withheldPaneLimit
	}
	var poolExhausted errProviderPoolExhausted
	if errors.As(err, &poolExhausted) {
		switch poolExhausted.Reason {
		case "paused":
			stage = withheldDispatchPaused
		case "quota-exhausted":
			stage = withheldProviderQuota
		default:
			stage = withheldProviderPool
		}
	}
	a.refusals.Store(task, stage)
}

func (a *liveAdmission) refusalReason(task string) string {
	v, _ := a.refusals.Load(task)
	r, _ := v.(string)
	return r
}

// withheldTaskKey carries a dispatch's task in its launch context, so that an
// admission refusal made below the executor, a resident pane reservation's, is
// classified for that task by its cause like any other (FAE-SCH-010).
type withheldTaskKey struct{}

func withWithheldTask(ctx context.Context, task string) context.Context {
	return context.WithValue(ctx, withheldTaskKey{}, task)
}

// withheldTaskOf names the task a launch context runs for, or "".
func withheldTaskOf(ctx context.Context) string {
	task, _ := ctx.Value(withheldTaskKey{}).(string)
	return task
}
