package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/xleelz/fae/workflow"
	"slices"
	"sort"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
)

func requestMarker(target, id string) string {
	return "change-request/" + controlplane.Digest([]byte(target)) + "/" + controlplane.Digest([]byte(id))
}

func (d *Daemon) changeRequestsForTask(ctx context.Context, id string) ([]engine.AncestorChangeRequest, error) {
	out := []engine.AncestorChangeRequest{}
	seen := map[string]bool{}
	markers, err := d.store.MarkersWithPrefix(ctx, "change-request/"+controlplane.Digest([]byte(id))+"/", 10000)
	if err != nil {
		return nil, err
	}
	for _, raw := range markers {
		var r engine.AncestorChangeRequest
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if r.Target != id {
			return nil, errors.New("change request marker target mismatch")
		}
		seen[r.ID] = true
		out = append(out, r)
	}
	after := int64(0)
	for {
		events, err := d.store.JobEvents(ctx, id, after, 500)
		if err != nil {
			return nil, err
		}
		if len(events) == 0 {
			break
		}
		for _, ev := range events {
			after = ev.Sequence
			if ev.Kind != "change-request" {
				continue
			}
			raw, err := d.store.ResolveText(ctx, string(ev.Payload))
			if err != nil {
				return nil, err
			}
			var e engine.Event
			if json.Unmarshal(raw, &e) != nil || e.Request == nil || e.Request.Target != id || seen[e.Request.ID] {
				continue
			}
			r := *e.Request
			seen[r.ID] = true
			if body, err := d.store.GetMarker(ctx, requestMarker(id, r.ID)); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			} else if err == nil {
				if err = json.Unmarshal(body, &r); err != nil {
					return nil, err
				}
			}
			out = append(out, r)
		}
		if len(events) < 500 {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

type ChangeRequestDispositionParams struct {
	Task               string `json:"task"`
	ID                 string `json:"id"`
	State              string `json:"state"`
	Reason             string `json:"reason"`
	AuthorizingMessage string `json:"authorizing_message"`
}

func (d *Daemon) changeRequestDisposition(ctx context.Context, raw json.RawMessage, a controlplane.Audit) (any, error) {
	var p ChangeRequestDispositionParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Reason == "" || (p.State != "accepted" && p.State != "declined") {
		return nil, errors.New("request disposition requires accepted or declined and reason")
	}
	requests, err := d.changeRequestsForTask(ctx, p.Task)
	if err != nil {
		return nil, err
	}
	for _, r := range requests {
		if r.ID != p.ID {
			continue
		}
		if r.State != "pending" {
			if r.State == p.State && r.Reason == p.Reason {
				return r, nil
			}
			return nil, errors.New("request is already disposed")
		}
		r.State = p.State
		r.Reason = p.Reason
		r.Message = a.MessageID
		r.Actor = a.Actor
		body, err := json.Marshal(r)
		if err != nil {
			return nil, err
		}
		if err = d.store.RecordChangeRequestDisposition(ctx, a, p.Task, requestMarker(p.Task, p.ID), body); err != nil {
			return nil, err
		}
		d.notifyEvents()
		return r, nil
	}
	return nil, errors.New("unknown request addressed to task")
}

type ChangeRequestsParams struct {
	Task string `json:"task"`
}

func (d *Daemon) changeRequestsRead(ctx context.Context, raw json.RawMessage) (any, error) {
	var p ChangeRequestsParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Task == "" {
		return nil, errors.New("task required")
	}
	return d.changeRequestsForTask(ctx, p.Task)
}

func (d *Daemon) validateChangeRequest(j engine.Job, s workflow.Stage) error {
	target := s.Target
	if target == "parent" && j.Parent != nil {
		target = j.Parent.Task
	}
	if !slices.Contains(j.Ancestors, target) {
		return errors.New("target is not an ancestor")
	}
	row, err := d.store.GetJob(d.ctx, target)
	if err != nil {
		return fmt.Errorf("ancestor unavailable: %w", err)
	}
	var e envelope
	if err = decodeEnvelope(row.Payload, &e); err != nil {
		return err
	}
	if !workflow.DeclaresNamedArtifact(e.Job.Document, s.Artifact) {
		return fmt.Errorf("ancestor has no declared artifact %s", s.Artifact)
	}
	return nil
}
