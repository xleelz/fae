package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/cron"
	"github.com/xleelz/fae/workflow"
	"sort"
	"time"
)

const cronRoundLimit = 10

func (d *Daemon) saveCrons(ctx context.Context, a controlplane.Audit) error {
	raw, e := json.Marshal(d.crons)
	if e != nil {
		return e
	}
	return d.store.PutMarker(ctx, a, "cron-schedules", raw)
}
func (d *Daemon) loadCrons(ctx context.Context) error {
	d.crons = map[string]cron.Schedule{}
	raw, e := d.store.GetMarker(ctx, "cron-schedules")
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = decode(raw, &d.crons); e != nil {
		return e
	}
	if d.crons == nil || len(d.crons) > 1000 {
		return errors.New("invalid cron collection")
	}
	for id, s := range d.crons {
		if s.Spec.ID != id || s.MessageID == "" || s.Validate() != nil {
			return fmt.Errorf("invalid durable cron %s", id)
		}
		if _, exists := d.loops[id]; exists {
			return errors.New("duplicate schedule identity across kinds")
		}
		if e = d.store.RequireScheduleAuthority(ctx, s.MessageID); e != nil {
			return e
		}
		if _, _, e = d.store.GetWorkflow(ctx, s.Spec.Template.WorkflowID, s.Spec.Template.Revision); e != nil {
			return e
		}
	}
	return nil
}
func (d *Daemon) cronSchedule(ctx context.Context, p ScheduleParams, a controlplane.Audit) (any, error) {
	if p.Action == "create" {
		if p.Cron == nil || p.Spec != nil {
			return nil, errors.New("exactly one cron specification required")
		}
		if d.draining {
			return nil, errors.New("draining: schedule creation paused")
		}
		if len(d.crons)+len(d.loops) >= 1000 {
			return nil, errors.New("schedule limit reached")
		}
		spec := *p.Cron
		if _, ok := d.crons[spec.ID]; ok {
			return nil, errors.New("schedule already exists")
		}
		if _, ok := d.loops[spec.ID]; ok {
			return nil, errors.New("schedule already exists")
		}
		rev, raw, e := d.store.GetWorkflow(ctx, spec.Template.WorkflowID, spec.Template.Revision)
		if e != nil {
			return nil, e
		}
		doc, e := workflow.Parse(raw)
		if e != nil {
			return nil, e
		}
		if issues := workflow.Validate(doc); len(issues) > 0 {
			return nil, fmt.Errorf("invalid cron workflow: %v", issues)
		}
		if d.live != nil && d.live.guard.panes != nil {
			if e = validatePaneFloor(doc, d.live.guard.effectivePaneLimit()); e != nil {
				return nil, e
			}
		}
		target := AdmitParams{TargetConfig: spec.Template.TargetConfig, TargetRevision: spec.Template.TargetRevision, Mode: spec.Template.Mode}
		if target.TargetConfig != "" {
			if e = d.store.TargetActive(ctx, target.TargetConfig); e != nil {
				return nil, e
			}
		}
		if e = d.pinTarget(ctx, &target, doc); e != nil {
			return nil, e
		}
		spec.Template.TargetRevision = target.TargetRevision
		spec.Template.Revision = rev.Number
		if spec.Template.RunID == "" {
			spec.Template.RunID = "cron-" + spec.ID
		}
		s, e := cron.New(spec)
		if e != nil {
			return nil, e
		}
		s.MessageID = a.MessageID
		d.crons[spec.ID] = s
		if e = d.saveCrons(ctx, a); e != nil {
			delete(d.crons, spec.ID)
			return nil, e
		}
		d.signal()
		return s, nil
	}
	s, ok := d.crons[p.ID]
	if !ok {
		return nil, errors.New("cron schedule not found")
	}
	if p.Action == "inspect" {
		return s, nil
	}
	prior := s
	switch p.Action {
	case "pause":
		if s.State != "active" {
			return nil, errors.New("only active cron schedules can pause")
		}
		s.State = "paused"
	case "resume":
		if s.State != "paused" {
			return nil, errors.New("only paused cron schedules can resume")
		}
		s.State = "active"
		s.Error = ""
		s.Bound()
	case "retire":
		s.State = "retired"
	default:
		return nil, errors.New("unsupported cron action")
	}
	d.crons[p.ID] = s
	if e := d.saveCrons(ctx, a); e != nil {
		d.crons[p.ID] = prior
		return nil, e
	}
	d.signal()
	return s, nil
}

// dispatchCrons uses deterministic idempotency keys and commits each admission
// before moving its cursor. A crash between those commits replays only the SQL
// admission response, never repeats worktree or provider effects.
func (d *Daemon) dispatchCrons(now time.Time) error {
	defer d.publishStatus()
	ids := make([]string, 0, len(d.crons))
	for id := range d.crons {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, _ := d.crons[ids[i]].At(d.crons[ids[i]].Next)
		b, _ := d.crons[ids[j]].At(d.crons[ids[j]].Next)
		if a.Equal(b) {
			return ids[i] < ids[j]
		}
		return a.Before(b)
	})
	remaining := cronRoundLimit
	for _, id := range ids {
		s := d.crons[id]
		priorState := s.State
		s.Bound()
		if priorState != s.State {
			d.crons[id] = s
			if e := d.saveCrons(d.ctx, controlplane.Audit{Actor: "scheduler", Action: "cron completed", MessageID: s.MessageID, Outcome: s.State}); e != nil {
				return e
			}
		}
		skip, indices := s.Due(now)
		if skip > 0 {
			s.Next += skip
			s.Skipped += skip
			d.crons[id] = s
			raw, _ := json.Marshal(map[string]any{"schedule": id, "skipped": skip, "next_occurrence": s.Next})
			if e := d.saveCrons(d.ctx, controlplane.Audit{Actor: "scheduler", Action: "cron catch-up skipped", MessageID: s.MessageID, Inputs: raw, Outcome: "bounded catch-up"}); e != nil {
				return e
			}
		}
		for _, index := range indices {
			if remaining == 0 {
				return nil
			}
			if d.ctx.Err() != nil {
				return d.ctx.Err()
			}
			tmpl := s.Spec.Template
			task := s.TaskID(index)
			p := AdmitParams{TargetConfig: tmpl.TargetConfig, TargetRevision: tmpl.TargetRevision, ID: task, WorkflowID: tmpl.WorkflowID, Revision: tmpl.Revision, Tree: tmpl.Tree, Title: tmpl.Title, Content: tmpl.Content, Priority: tmpl.Priority, RunID: tmpl.RunID, Mode: tmpl.Mode, AuthorizingMessage: s.MessageID}
			raw, _ := json.Marshal(p)
			_, e := d.admit(d.ctx, "schedule:"+task, raw, controlplane.Audit{Actor: "scheduler", Action: "cron occurrence admitted", MessageID: s.MessageID, Inputs: raw, Outcome: "ok"})
			if e != nil {
				if d.ctx.Err() != nil {
					return d.ctx.Err()
				}
				s.State = "paused"
				s.Error = e.Error()
				d.crons[id] = s
				detail, _ := json.Marshal(map[string]any{"schedule": id, "occurrence": index, "error": s.Error})
				return d.saveCrons(d.ctx, controlplane.Audit{Actor: "scheduler", Action: "cron admission failed", MessageID: s.MessageID, Inputs: detail, Outcome: "paused"})
			}
			s.Next = index + 1
			s.Admitted++
			s.LastTask = task
			s.Error = ""
			s.Bound()
			d.crons[id] = s
			detail, _ := json.Marshal(map[string]any{"schedule": id, "occurrence": index, "task": task})
			if e = d.saveCrons(d.ctx, controlplane.Audit{Actor: "scheduler", Action: "cron occurrence checkpoint", MessageID: s.MessageID, Inputs: detail, Outcome: s.State}); e != nil {
				return e
			}
			remaining--
		}
	}
	return nil
}
