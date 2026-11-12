package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// DefaultStuckCommitBound is the Appendix A stuck commit-stage detector bound.
const DefaultStuckCommitBound = 10 * time.Minute

// detectStuckCommits runs under d.mu each round (FAE-VCS-009): a task resting
// in a VcsCommit or Integrate stage with no running worker (no lease, no owner)
// beyond the bound raises one audited alert per resting episode. The episode
// (stage, dispatch sequence, start, alerted) is durable, so a restart neither
// re-arms the clock nor repeats the alert; any new dispatch starts a new one.
func (d *Daemon) detectStuckCommits(ctx context.Context, now time.Time) error {
	bound := d.config.StuckCommitBound
	if bound <= 0 {
		bound = DefaultStuckCommitBound
	}
	if d.commitRest == nil {
		d.commitRest = map[string]commitRest{}
	}
	resting := map[string]bool{}
	for id, snapshot := range d.jobs {
		if d.active[id] {
			continue
		}
		var e envelope
		if decodeEnvelope(snapshot.Payload, &e) != nil {
			continue
		}
		stage, ok := e.Job.Document.Stages[string(e.Job.State)]
		if !ok || (stage.Primitive != workflow.VcsCommit && stage.Primitive != workflow.Integrate) {
			continue
		}
		resting[id] = true
		rest, seen := d.commitRest[id]
		if !seen || rest.Stage != string(e.Job.State) || rest.Dispatch != e.Job.Sequence {
			durable, err := d.restingEpisode(ctx, id, string(e.Job.State), e.Job.Sequence, now)
			if err != nil {
				d.taskLog("task %s stuck-commit clock deferred: %v\n", id, err)
				delete(resting, id)
				continue
			}
			d.commitRest[id] = durable
			rest = durable
		}
		if rest.Alerted || now.Sub(rest.Since) < bound {
			continue
		}
		body, _ := json.Marshal(map[string]any{"task": id, "stage": rest.Stage, "since": rest.Since, "bound_seconds": bound.Seconds(), "parked": e.Job.Parked, "reason": e.Job.Reason})
		if _, _, err := d.raiseAlert(ctx, controlplane.Alert{Severity: "error", Source: "stuck-commit-stage", Task: id, Detail: body}, false); err != nil {
			d.taskLog("task %s stuck-commit alert deferred: %v\n", id, err)
			continue
		}
		d.taskLog("task %s rests in %s with no lease since %s\n", id, rest.Stage, rest.Since.Format(time.RFC3339))
		rest.Alerted = true
		d.commitRest[id] = rest
		if err := d.saveRestingEpisode(ctx, id, rest); err != nil {
			d.taskLog("task %s stuck-commit alert record deferred: %v\n", id, err)
		}
		d.notifyEvents()
	}
	for id := range d.commitRest {
		if !resting[id] {
			delete(d.commitRest, id)
		}
	}
	return nil
}

type commitRest struct {
	Stage    string    `json:"stage"`
	Dispatch int       `json:"dispatch_sequence"`
	Since    time.Time `json:"since"`
	Alerted  bool      `json:"alerted"`
}

func stuckCommitKey(task string) string {
	raw, _ := json.Marshal(task)
	h, _ := domain.ContentHash(raw)
	return "stuck-commit/" + h
}

// restingEpisode resumes the durable episode of this stage with no dispatch
// since, or starts and records a new one.
func (d *Daemon) restingEpisode(ctx context.Context, id, stage string, sequence int, now time.Time) (commitRest, error) {
	raw, err := d.store.GetMarker(ctx, stuckCommitKey(id))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return commitRest{}, err
	}
	var prior commitRest
	if err == nil && json.Unmarshal(raw, &prior) == nil && prior.Stage == stage && prior.Dispatch == sequence && !prior.Since.IsZero() {
		return prior, nil
	}
	rest := commitRest{Stage: stage, Dispatch: sequence, Since: now}
	return rest, d.saveRestingEpisode(ctx, id, rest)
}
func (d *Daemon) saveRestingEpisode(ctx context.Context, id string, rest commitRest) error {
	raw, _ := json.Marshal(rest)
	return d.store.PutMarker(ctx, controlplane.Audit{Actor: "daemon", Action: "stuck-commit clock", Outcome: "resting"}, stuckCommitKey(id), raw)
}
