package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/workflow"
)

// apiReads are the FAE-API-001 reads beyond the status snapshot: Leases,
// Panes, the ProviderHealth series, and TargetConfig validate/diff/simulate.
var apiReads = map[string]bool{"Leases": true, "Panes": true, "TargetConfigValidate": true, "TargetConfigDiff": true, "TargetConfigSimulate": true}

func (d *Daemon) apiRead(ctx context.Context, method string, raw json.RawMessage) (any, error) {
	switch method {
	case "Leases":
		if err := decode(raw, &struct{}{}); err != nil {
			return nil, err
		}
		return d.leases(ctx)
	case "Panes":
		if err := decode(raw, &struct{}{}); err != nil {
			return nil, err
		}
		return d.panes(ctx)
	case "TargetConfigValidate":
		var p struct {
			Document json.RawMessage `json:"document"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return d.targetValidate(ctx, p.Document), nil
	case "TargetConfigDiff":
		var p struct {
			ID   string `json:"id"`
			From int64  `json:"from"`
			To   int64  `json:"to"`
		}
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return d.targetDiff(ctx, p.ID, p.From, p.To)
	case "TargetConfigSimulate":
		var p TargetSimulateParams
		if err := decode(raw, &p); err != nil {
			return nil, err
		}
		return d.targetSimulate(ctx, p)
	}
	return nil, fmt.Errorf("unknown read %q", method)
}

// leases reports role slots, heavy operations, deployment holds and pane
// reservations, each with holders and waiters (FAE-API-001).
func (d *Daemon) leases(ctx context.Context) (any, error) {
	pools, err := d.store.LeasePools(ctx)
	if err != nil {
		return nil, err
	}
	roles := []map[string]any{}
	for _, p := range pools {
		cap := p.Pool.Capacity
		if p.Pool.AdmissionLimit > 0 {
			cap = p.Pool.AdmissionLimit
		}
		roles = append(roles, map[string]any{"key": p.Key, "kind": p.Kind, "capacity": cap, "holders": p.Pool.Holders, "waiters": p.Pool.Waiting})
	}
	holds, err := d.store.MarkersWithPrefix(ctx, "deployment-hold/", 1000)
	if err != nil {
		return nil, err
	}
	deployment := []controller.OverlapHold{}
	for _, raw := range holds {
		var h controller.OverlapHold
		if json.Unmarshal(raw, &h) == nil {
			deployment = append(deployment, h)
		}
	}
	sort.Slice(deployment, func(i, j int) bool { return deployment[i].Task < deployment[j].Task })
	out := map[string]any{"at": time.Now().UTC(), "role_slots": roles, "deployment_holds": deployment, "heavy_ops": nil, "pane_reservations": nil, "provider_pools": nil}
	if d.live != nil && d.live.guard != nil {
		if d.live.guard.host != nil {
			if pool, e := d.live.guard.host.Snapshot(ctx); e == nil {
				out["heavy_ops"] = map[string]any{"capacity": pool.Capacity, "admission_limit": pool.AdmissionLimit, "holders": pool.Holders, "waiters": pool.Waiting}
			} else {
				return nil, e
			}
			if snaps, e := d.live.guard.host.ProviderPoolsSnapshot(ctx); e == nil {
				out["provider_pools"] = snaps
			}
		}
		if d.live.guard.panes != nil {
			reservations, e := d.live.guard.panes.Snapshot(ctx)
			if e != nil {
				return nil, e
			}
			out["pane_reservations"] = map[string]any{"limit": d.live.guard.effectivePaneLimit(), "holders": reservations}
		}
	}
	return out, nil
}

// PaneSession is one live session: its role, task, binding, owner and age.
type PaneSession struct {
	Session    string  `json:"session"`
	Role       string  `json:"role"`
	Task       string  `json:"task"`
	Stage      string  `json:"stage"`
	Binding    string  `json:"binding,omitempty"`
	Owner      int     `json:"owner_pid"`
	AgeSeconds float64 `json:"age_seconds"`
}

// panes reports the sessions of in-flight dispatches from the plane, plus the
// multiplexer's pane reservations (FAE-API-001).
func (d *Daemon) panes(ctx context.Context) (any, error) {
	value, _ := d.status.Load().(statusSnapshot)
	now := time.Now().UTC()
	sessions := []PaneSession{}
	for _, t := range value.Tasks {
		if !t.InFlight {
			continue
		}
		s, err := d.store.GetJob(ctx, t.ID)
		if err != nil {
			return nil, fmt.Errorf("task %s: %w", t.ID, err)
		}
		var e envelope
		if decodeEnvelope(s.Payload, &e) != nil || e.Job.InFlight == nil {
			continue
		}
		for _, session := range e.Job.InFlight.Sessions {
			binding := ""
			if session.Route.Provider != "" {
				binding = session.Route.Provider + "/" + session.Route.Model
			}
			sessions = append(sessions, PaneSession{session.ID, session.Role, t.ID, e.Job.InFlight.Stage, binding, os.Getpid(), now.Sub(e.Job.InFlight.CreatedAt).Seconds()})
		}
	}
	out := map[string]any{"at": now, "sessions": sessions, "reservations": nil, "adapter": "scripted"}
	if d.config.Live != nil {
		out["adapter"] = d.config.Live.Adapter
	}
	if d.live != nil && d.live.guard != nil && d.live.guard.panes != nil {
		reservations, err := d.live.guard.panes.Snapshot(ctx)
		if err != nil {
			return nil, err
		}
		rows := []map[string]any{}
		for _, r := range reservations {
			rows = append(rows, map[string]any{"id": r.ID, "owner_pid": r.Owner.PID, "owned": r.Owner == d.live.guard.owner, "age_seconds": now.Sub(r.Reserved).Seconds(), "seen_at": r.SeenAt})
		}
		out["reservations"] = rows
	}
	return out, nil
}

// providerHealthSeries is the ProviderHealth time series over a window
// (default 15 min): each binding's observed outcomes and scores, oldest first,
// from the plane's provider-observation events.
func (d *Daemon) providerHealthSeries(ctx context.Context, window time.Duration) (map[string][]map[string]any, bool, error) {
	events, truncated, err := d.store.EventsOfKindSince(ctx, "provider-observation", time.Now().UTC().Add(-window), 10000)
	if err != nil {
		return nil, false, err
	}
	series := map[string][]map[string]any{}
	for _, ev := range events {
		var p struct {
			Observation struct {
				Provider string    `json:"provider"`
				Model    string    `json:"model"`
				Outcome  string    `json:"outcome"`
				At       time.Time `json:"at"`
			} `json:"observation"`
			Score float64 `json:"score"`
		}
		if json.Unmarshal(ev.Payload, &p) != nil || p.Observation.Provider == "" {
			continue
		}
		key := p.Observation.Provider + "/" + p.Observation.Model
		series[key] = append(series[key], map[string]any{"sequence": ev.Sequence, "at": ev.Timestamp, "outcome": p.Observation.Outcome, "score": p.Score})
	}
	return series, truncated, nil
}

// targetValidate checks a target configuration as TargetConfigPut would,
// writing nothing and making no external read: the review baseline health
// check stays with review-health (FAE-API-001).
func (d *Daemon) targetValidate(ctx context.Context, raw json.RawMessage) map[string]any {
	issues := []string{}
	var doc TargetConfig
	err := decode(raw, &doc)
	if err == nil && doc.Normal != nil && doc.Normal.Adapter != nil {
		if err = d.store.AdapterActive(ctx, doc.Normal.Adapter.ID); err == nil {
			var b AdapterBinding
			if b, _, err = d.resolveAdapter(ctx, *doc.Normal.Adapter); err == nil {
				doc.Normal.Adapter = &b
			}
		}
	}
	if err == nil {
		resolved, _ := json.Marshal(doc)
		var c TargetConfig
		if c, err = parseTarget(resolved); err == nil {
			if err = d.validateTargetBinding(c); err == nil {
				err = d.validateNormalTarget(ctx, c, AdmitParams{}, workflow.Document{}, false)
			}
		}
	}
	if err != nil {
		issues = append(issues, err.Error())
	}
	return map[string]any{"valid": len(issues) == 0, "errors": issues}
}

// targetDiff is the structural difference between two target revisions: each
// changed JSON path with its old and new values.
func (d *Daemon) targetDiff(ctx context.Context, id string, from, to int64) (any, error) {
	if id == "" || from < 1 || to < 1 {
		return nil, errors.New("target id and positive from/to revisions required")
	}
	_, a, err := d.store.GetTarget(ctx, id, from)
	if err != nil {
		return nil, err
	}
	_, b, err := d.store.GetTarget(ctx, id, to)
	if err != nil {
		return nil, err
	}
	var left, right any
	if err = json.Unmarshal(a, &left); err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &right); err != nil {
		return nil, err
	}
	changes := []map[string]any{}
	jsonDiff("", left, right, &changes)
	return map[string]any{"id": id, "from": from, "to": to, "identical": len(changes) == 0, "changes": changes}, nil
}

func jsonDiff(path string, a, b any, out *[]map[string]any) {
	am, aok := a.(map[string]any)
	bm, bok := b.(map[string]any)
	if aok && bok {
		keys := []string{}
		for k := range am {
			keys = append(keys, k)
		}
		for k := range bm {
			if _, seen := am[k]; !seen {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			jsonDiff(path+"/"+k, am[k], bm[k], out)
		}
		return
	}
	if !reflect.DeepEqual(a, b) {
		if path == "" {
			path = "/"
		}
		*out = append(*out, map[string]any{"path": path, "from": a, "to": b})
	}
}

// TargetSimulateParams simulates a target (stored revision or document)
// against a proposed change: which required tests and compile check apply,
// which paths are production code, and which commands the allowlist admits.
type TargetSimulateParams struct {
	ID       string          `json:"id,omitempty"`
	Revision int64           `json:"revision,omitempty"`
	Document json.RawMessage `json:"document,omitempty"`
	Changed  []string        `json:"changed,omitempty"`
	Commands [][]string      `json:"commands,omitempty"`
}

func (d *Daemon) targetSimulate(ctx context.Context, p TargetSimulateParams) (any, error) {
	raw := []byte(p.Document)
	if (p.ID == "") == (len(raw) == 0) || len(p.Changed) > 10000 || len(p.Commands) > 1000 {
		return nil, errors.New("simulate requires exactly one of id or document, at most 10000 changed paths and 1000 commands")
	}
	if p.ID != "" {
		var err error
		if _, raw, err = d.store.GetTarget(ctx, p.ID, p.Revision); err != nil {
			return nil, err
		}
	}
	c, err := parseTarget(raw)
	if err != nil {
		return nil, err
	}
	required := []string{}
	for _, t := range requiredFor(c, p.Changed) {
		required = append(required, t.Name)
	}
	production := []string{}
	if c.DiffClassifier != nil {
		production = classifyDiff(*c.DiffClassifier, p.Changed)
	}
	commands := []map[string]any{}
	for _, argv := range p.Commands {
		commands = append(commands, map[string]any{"argv": argv, "allowed": targetAllowsVector(c, argv)})
	}
	var compile any
	if c.CompileCheck != nil {
		compile = c.CompileCheck.Argv
	}
	protected := slices.Clone(c.ProtectedBranches)
	return map[string]any{"required_tests": required, "compile_check": compile, "production_paths": production, "classified": c.DiffClassifier != nil, "commands": commands, "protected_branches": protected, "integration_ref": c.IntegrationRef, "normal": c.Normal != nil, "workspace": strings.TrimSpace(c.Workspace)}, nil
}
