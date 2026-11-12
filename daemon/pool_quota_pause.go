package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/providerhealth"
	"github.com/xleelz/fae/scheduler"
	"github.com/xleelz/fae/workflow"
)

// PoolResizeParams requests a live audited provider concurrency pool resize (FAE-SCH-015).
type PoolResizeParams struct {
	Pool               string `json:"pool"`
	Provider           string `json:"provider,omitempty"`
	CredentialIdentity string `json:"credential_identity,omitempty"`
	Cap                int    `json:"cap"`
	Actor              string `json:"actor,omitempty"`
	Reason             string `json:"reason"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

// QuotaSetParams marks a provider pool or (provider, model) binding quota-exhausted until T (FAE-RT-018).
type QuotaSetParams struct {
	Pool               string    `json:"pool,omitempty"`
	Provider           string    `json:"provider,omitempty"`
	Model              string    `json:"model,omitempty"`
	ResetAt            time.Time `json:"reset_at"`
	GraceSeconds       int       `json:"grace_seconds,omitempty"`
	CutOffTaskID       string    `json:"cut_off_task_id,omitempty"`
	Actor              string    `json:"actor,omitempty"`
	Reason             string    `json:"reason"`
	AuthorizingMessage string    `json:"authorizing_message,omitempty"`
}

// QuotaClearParams clears a quota-exhausted mark on a provider pool or (provider, model) binding (FAE-RT-018).
type QuotaClearParams struct {
	Pool               string `json:"pool,omitempty"`
	Provider           string `json:"provider,omitempty"`
	Model              string `json:"model,omitempty"`
	Actor              string `json:"actor,omitempty"`
	Reason             string `json:"reason,omitempty"`
	AuthorizingMessage string `json:"authorizing_message,omitempty"`
}

// DispatchPauseParams pauses or unpauses new dispatch in a durable scope (FAE-RT-018).
type DispatchPauseParams struct {
	Scope              string   `json:"scope"`
	Target             string   `json:"target,omitempty"`
	WorkflowID         string   `json:"workflow_id,omitempty"`
	RunID              string   `json:"run_id,omitempty"`
	TaskIDs            []string `json:"task_ids,omitempty"`
	Pool               string   `json:"pool,omitempty"`
	Actor              string   `json:"actor,omitempty"`
	Reason             string   `json:"reason,omitempty"`
	AuthorizingMessage string   `json:"authorizing_message,omitempty"`
}

func resolvePauseScopeTarget(p DispatchPauseParams) (string, string, []string) {
	scope := strings.TrimSpace(p.Scope)
	target := strings.TrimSpace(p.Target)
	if scope == "" {
		switch {
		case strings.TrimSpace(p.WorkflowID) != "":
			scope = "workflow"
		case strings.TrimSpace(p.RunID) != "" || len(p.TaskIDs) > 0:
			scope = "task_set"
		case strings.TrimSpace(p.Pool) != "":
			scope = "pool"
		default:
			scope = "instance"
		}
	}
	switch scope {
	case "instance":
		if target == "" {
			target = "instance"
		}
	case "workflow":
		if target == "" {
			target = strings.TrimSpace(p.WorkflowID)
		}
	case "pool":
		if target == "" {
			target = strings.TrimSpace(p.Pool)
		}
	case "task_set":
		if target == "" {
			target = strings.TrimSpace(p.RunID)
		}
	}
	return scope, target, p.TaskIDs
}

func (d *Daemon) poolResize(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p PoolResizeParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	p.Pool = strings.TrimSpace(p.Pool)
	p.Provider = strings.TrimSpace(p.Provider)
	p.CredentialIdentity = strings.TrimSpace(p.CredentialIdentity)
	p.Reason = strings.TrimSpace(p.Reason)
	if p.Pool == "" {
		return nil, errors.New("provider pool ID required")
	}
	if p.Cap == 0 || p.Cap < -1 || p.Cap > 1000000 {
		return nil, errors.New("provider pool cap must be -1 (unbounded) or between 1 and 1000000")
	}
	if p.Reason == "" {
		return nil, errors.New("provider pool resize requires a reason")
	}
	if p.Actor != "" {
		audit.Actor = strings.TrimSpace(p.Actor)
	}
	cfg := scheduler.ProviderPoolConfig{
		ID: p.Pool,
		Match: scheduler.ProviderPoolMatch{
			Provider:           p.Provider,
			CredentialIdentity: p.CredentialIdentity,
		},
		Cap: p.Cap,
	}
	var snap scheduler.ProviderPoolSnapshot
	if d.live != nil && d.live.guard != nil && d.live.guard.host != nil {
		var err error
		snap, err = d.live.guard.host.ResizeProviderPool(ctx, cfg, p.Reason)
		if err != nil {
			return nil, err
		}
		cfg = snap.Config
		d.live.guard.mirrorProviderPool(ctx, snap)
	} else {
		if cfg.Match.Provider == "" {
			cfg.Match.Provider = p.Pool
		}
		if cfg.Match.CredentialIdentity == "" {
			cfg.Match.CredentialIdentity = "default"
		}
		if err := scheduler.ValidateProviderPoolConfig(cfg); err != nil {
			return nil, err
		}
		effCap := cfg.Cap
		if effCap == -1 {
			effCap = scheduler.UnboundedProviderPoolCap
		}
		pp, _ := scheduler.NewPool(effCap)
		pp.AdmissionLimit = effCap
		pp.ReadySetOrder = true
		snap = scheduler.ProviderPoolSnapshot{
			Config: cfg,
			Pool:   *pp,
		}
		_ = d.store.MirrorProviderPool(ctx, cfg.ID, snap.Pool)
	}
	rc, err := d.readRuntimeConfig(ctx)
	if err == nil {
		updated := false
		for i := range rc.ProviderPools {
			if rc.ProviderPools[i].ID == cfg.ID {
				rc.ProviderPools[i] = cfg
				updated = true
				break
			}
		}
		if !updated {
			rc.ProviderPools = append(rc.ProviderPools, cfg)
		}
		rcRaw, _ := json.Marshal(rc)
		_ = d.store.PutMarker(ctx, audit, "runtime-config", rcRaw)
	}
	decisionPayload, _ := json.Marshal(map[string]any{
		"pool":                cfg.ID,
		"provider":            cfg.Match.Provider,
		"credential_identity": cfg.Match.CredentialIdentity,
		"cap":                 cfg.Cap,
		"reason":              p.Reason,
		"actor":               audit.Actor,
	})
	if err := d.store.RecordAuthorityDecision(ctx, audit, "pool-resize", "pool:"+cfg.ID, p.Reason, decisionPayload); err != nil {
		return nil, err
	}
	d.publishStatus()
	d.signal()
	return map[string]any{
		"pool":            snap.Config.ID,
		"config":          snap.Config,
		"capacity":        snap.Pool.Capacity,
		"admission_limit": snap.Pool.AdmissionLimit,
		"holders":         len(snap.Pool.Holders),
		"waiting":         len(snap.Pool.Waiting),
	}, nil
}

func (d *Daemon) quotaSet(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p QuotaSetParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	p.Pool = strings.TrimSpace(p.Pool)
	p.Provider = strings.TrimSpace(p.Provider)
	p.Model = strings.TrimSpace(p.Model)
	p.Reason = strings.TrimSpace(p.Reason)
	if p.Pool == "" && (p.Provider == "" || p.Model == "") {
		return nil, errors.New("quota set requires --pool or both --provider and --model")
	}
	if p.ResetAt.IsZero() {
		return nil, errors.New("quota set requires reset_at")
	}
	if p.Reason == "" {
		p.Reason = "operator quota set"
	}
	if p.Actor != "" {
		audit.Actor = strings.TrimSpace(p.Actor)
	}
	grace := p.GraceSeconds
	if grace == 0 {
		grace = 60
	} else if grace < 0 {
		grace = 0
	}
	resetAt := p.ResetAt.UTC()
	exhaustedUntil := resetAt.Add(time.Duration(grace) * time.Second)
	if p.Pool != "" {
		if d.live != nil && d.live.guard != nil && d.live.guard.host != nil {
			if err := d.live.guard.host.SetProviderPoolQuota(ctx, scheduler.ProviderPoolQuota{
				PoolID:         p.Pool,
				ResetAt:        resetAt,
				GraceSeconds:   grace,
				ExhaustedUntil: exhaustedUntil,
				CutOffTaskID:   p.CutOffTaskID,
				Actor:          audit.Actor,
				Reason:         p.Reason,
			}); err != nil {
				return nil, err
			}
			if snaps, sErr := d.live.guard.host.ProviderPoolsSnapshot(ctx); sErr == nil {
				if snap, ok := snaps[p.Pool]; ok {
					d.live.guard.mirrorProviderPool(ctx, snap)
				}
			}
		}
		decisionPayload, _ := json.Marshal(map[string]any{
			"pool":            p.Pool,
			"reset_at":        resetAt,
			"grace_seconds":   grace,
			"exhausted_until": exhaustedUntil,
			"cut_off_task_id": p.CutOffTaskID,
			"reason":          p.Reason,
			"actor":           audit.Actor,
		})
		if err := d.store.PutMarker(ctx, audit, "provider-pool-quota/"+p.Pool, decisionPayload); err != nil {
			return nil, err
		}
		if err := d.store.RecordAuthorityDecision(ctx, audit, "quota-set", "pool:"+p.Pool, p.Reason, decisionPayload); err != nil {
			return nil, err
		}
		d.publishStatus()
		d.signal()
		return map[string]any{
			"scope":           "pool",
			"pool":            p.Pool,
			"reset_at":        resetAt,
			"grace_seconds":   grace,
			"exhausted_until": exhaustedUntil,
		}, nil
	}
	q := controlplane.ProviderQuotaState{
		Provider:       p.Provider,
		Model:          p.Model,
		ResetAt:        resetAt,
		GraceSeconds:   grace,
		ExhaustedUntil: exhaustedUntil,
		CutOffTaskID:   p.CutOffTaskID,
		Actor:          audit.Actor,
		Reason:         p.Reason,
		UpdatedAt:      time.Now().UTC(),
	}
	if err := d.store.SetProviderQuota(ctx, audit, q); err != nil {
		return nil, err
	}
	d.publishStatus()
	d.signal()
	return map[string]any{
		"scope":           "binding",
		"provider":        p.Provider,
		"model":           p.Model,
		"reset_at":        resetAt,
		"grace_seconds":   grace,
		"exhausted_until": exhaustedUntil,
	}, nil
}

func (d *Daemon) quotaClear(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p QuotaClearParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	p.Pool = strings.TrimSpace(p.Pool)
	p.Provider = strings.TrimSpace(p.Provider)
	p.Model = strings.TrimSpace(p.Model)
	p.Reason = strings.TrimSpace(p.Reason)
	if p.Pool == "" && (p.Provider == "" || p.Model == "") {
		return nil, errors.New("quota clear requires --pool or both --provider and --model")
	}
	if p.Reason == "" {
		p.Reason = "operator quota clear"
	}
	if p.Actor != "" {
		audit.Actor = strings.TrimSpace(p.Actor)
	}
	audit.Outcome = p.Reason
	if p.Pool != "" {
		if d.live != nil && d.live.guard != nil && d.live.guard.host != nil {
			if err := d.live.guard.host.ClearProviderPoolQuota(ctx, p.Pool, audit.Actor, p.Reason); err != nil {
				return nil, err
			}
			if snaps, sErr := d.live.guard.host.ProviderPoolsSnapshot(ctx); sErr == nil {
				if snap, ok := snaps[p.Pool]; ok {
					d.live.guard.mirrorProviderPool(ctx, snap)
				}
			}
		}
		decisionPayload, _ := json.Marshal(map[string]any{
			"pool":       p.Pool,
			"cleared_at": time.Now().UTC(),
			"reason":     p.Reason,
			"actor":      audit.Actor,
		})
		_ = d.store.PutMarker(ctx, audit, "provider-pool-quota/"+p.Pool, json.RawMessage(`{}`))
		if err := d.store.RecordAuthorityDecision(ctx, audit, "quota-clear", "pool:"+p.Pool, p.Reason, decisionPayload); err != nil {
			return nil, err
		}
		d.publishStatus()
		d.signal()
		return map[string]any{
			"scope":   "pool",
			"pool":    p.Pool,
			"cleared": true,
		}, nil
	}
	if err := d.store.ClearProviderQuota(ctx, audit, p.Provider, p.Model); err != nil {
		return nil, err
	}
	d.publishStatus()
	d.signal()
	return map[string]any{
		"scope":    "binding",
		"provider": p.Provider,
		"model":    p.Model,
		"cleared":  true,
	}, nil
}

func (d *Daemon) dispatchPause(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p DispatchPauseParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Actor != "" {
		audit.Actor = strings.TrimSpace(p.Actor)
	}
	reason := strings.TrimSpace(p.Reason)
	if reason == "" {
		return nil, errors.New("dispatch pause requires a reason")
	}
	scope, target, taskIDs := resolvePauseScopeTarget(p)
	pause := controlplane.DispatchPause{
		Scope:     scope,
		Target:    target,
		TaskIDs:   taskIDs,
		Actor:     audit.Actor,
		Reason:    reason,
		MessageID: audit.MessageID,
		CreatedAt: time.Now().UTC(),
	}
	if err := d.store.PutDispatchPause(ctx, audit, pause); err != nil {
		return nil, err
	}
	if scope == "pool" && d.live != nil && d.live.guard != nil && d.live.guard.host != nil {
		if err := d.live.guard.host.SetProviderPoolPause(ctx, scheduler.ProviderPoolPause{
			PoolID: target,
			Paused: true,
			Actor:  audit.Actor,
			Reason: reason,
		}); err != nil {
			return nil, err
		}
	}
	d.publishStatus()
	d.signal()
	return map[string]any{
		"paused":   true,
		"scope":    scope,
		"target":   pause.Target,
		"task_ids": pause.TaskIDs,
		"actor":    audit.Actor,
		"reason":   reason,
	}, nil
}

func (d *Daemon) dispatchUnpause(ctx context.Context, raw json.RawMessage, audit controlplane.Audit) (any, error) {
	var p DispatchPauseParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if p.Actor != "" {
		audit.Actor = strings.TrimSpace(p.Actor)
	}
	reason := strings.TrimSpace(p.Reason)
	if reason == "" {
		reason = "operator dispatch unpause"
	}
	audit.Outcome = reason
	scope, target, taskIDs := resolvePauseScopeTarget(p)
	if err := d.store.DeleteDispatchPause(ctx, audit, scope, target, taskIDs); err != nil {
		return nil, err
	}
	if scope == "pool" && d.live != nil && d.live.guard != nil && d.live.guard.host != nil {
		if err := d.live.guard.host.SetProviderPoolPause(ctx, scheduler.ProviderPoolPause{
			PoolID: target,
			Paused: false,
			Actor:  audit.Actor,
			Reason: reason,
		}); err != nil {
			return nil, err
		}
	}
	d.publishStatus()
	d.signal()
	return map[string]any{
		"paused": false,
		"scope":  scope,
		"target": target,
		"actor":  audit.Actor,
		"reason": reason,
	}, nil
}

func (d *Daemon) dispatchPauseList(ctx context.Context, raw json.RawMessage) (any, error) {
	if err := decode(raw, &struct{}{}); err != nil {
		return nil, err
	}
	pauses, err := d.store.ListDispatchPauses(ctx)
	if err != nil {
		return nil, err
	}
	return map[string]any{"pauses": pauses}, nil
}

func (d *Daemon) loadDispatchPauses(ctx context.Context) []controlplane.DispatchPause {
	if d.store == nil {
		return nil
	}
	pauses, err := d.store.ListDispatchPauses(ctx)
	if err != nil {
		return nil
	}
	if d.live != nil && d.live.guard != nil && d.live.guard.host != nil {
		for _, p := range pauses {
			if p.Scope == "pool" {
				_ = d.live.guard.host.SetProviderPoolPause(ctx, scheduler.ProviderPoolPause{
					PoolID: p.Target,
					Paused: true,
					Actor:  p.Actor,
					Reason: p.Reason,
				})
			}
		}
	}
	return pauses
}

func (d *Daemon) isTaskDispatchPaused(id string, snapshot controlplane.JobSnapshot, e envelope, pauses []controlplane.DispatchPause) bool {
	for _, p := range pauses {
		switch p.Scope {
		case "instance":
			return true
		case "workflow":
			if p.Target == e.Job.Document.ID || p.Target == snapshot.WorkflowID {
				return true
			}
		case "task_set":
			if (e.Admission.RunID != "" && p.Target == e.Admission.RunID) || p.Target == id || slices.Contains(p.TaskIDs, id) {
				return true
			}
		}
	}
	stage, ok := e.Job.Document.Stages[string(e.Job.State)]
	if !ok || (stage.Primitive != workflow.AgentTurn && stage.Primitive != workflow.Review) {
		return false
	}
	role, hasRole := e.Job.Document.Roles[stage.Role]
	if !hasRole {
		return false
	}
	routes := role.Routes
	if len(routes) == 0 {
		return false
	}
	var poolSnaps map[string]scheduler.ProviderPoolSnapshot
	if d.live != nil && d.live.guard != nil && d.live.guard.host != nil {
		poolSnaps, _ = d.live.guard.host.ProviderPoolsSnapshot(d.ctx)
	}
	pausedPoolIDs := map[string]bool{}
	for _, p := range pauses {
		if p.Scope == "pool" {
			pausedPoolIDs[p.Target] = true
		}
	}
	for _, snap := range poolSnaps {
		if snap.Pause != nil && snap.Pause.Paused {
			pausedPoolIDs[snap.Config.ID] = true
		}
	}
	if len(pausedPoolIDs) == 0 {
		return false
	}
	matchedPaused := 0
	for _, route := range routes {
		credID := "default"
		if d.live != nil && d.live.guard != nil {
			credID = d.live.guard.resolveCredentialIdentity(engine.Session{Route: route})
		}
		routePaused := false
		for _, snap := range poolSnaps {
			if snap.Config.Match.Provider == route.Provider && (snap.Config.Match.CredentialIdentity == credID || snap.Config.Match.CredentialIdentity == "" || snap.Config.Match.CredentialIdentity == "*") {
				if pausedPoolIDs[snap.Config.ID] {
					routePaused = true
				}
				break
			}
		}
		if !routePaused && pausedPoolIDs[route.Provider] {
			routePaused = true
		}
		if routePaused {
			matchedPaused++
		}
	}
	return matchedPaused == len(routes)
}

func (d *Daemon) configureLiveRouteOptions(options *engine.Options, byBinding map[[2]string]providerhealth.Health, now time.Time) {
	var poolSnaps map[string]scheduler.ProviderPoolSnapshot
	if d.live != nil && d.live.guard != nil && d.live.guard.host != nil {
		poolSnaps, _ = d.live.guard.host.ProviderPoolsSnapshot(d.ctx)
	}
	poolQuotaMarkers := map[string]time.Time{}
	if d.store != nil {
		if rawMarkers, err := d.store.MarkersWithPrefix(d.ctx, "provider-pool-quota/", 100); err == nil {
			for k, raw := range rawMarkers {
				var m struct {
					Pool           string    `json:"pool"`
					ResetAt        time.Time `json:"reset_at"`
					ExhaustedUntil time.Time `json:"exhausted_until"`
				}
				if json.Unmarshal(raw, &m) == nil && !m.ExhaustedUntil.IsZero() && now.Before(m.ExhaustedUntil) {
					poolID := strings.TrimPrefix(k, "provider-pool-quota/")
					if m.Pool != "" {
						poolID = m.Pool
					}
					poolQuotaMarkers[poolID] = m.ResetAt
				}
			}
		}
	}
	matchPool := func(route workflow.Binding) (scheduler.ProviderPoolSnapshot, bool) {
		credID := "default"
		if d.live != nil && d.live.guard != nil {
			credID = d.live.guard.resolveCredentialIdentity(engine.Session{Route: route})
		}
		for _, snap := range poolSnaps {
			if snap.Config.Match.Provider == route.Provider && (snap.Config.Match.CredentialIdentity == credID || snap.Config.Match.CredentialIdentity == "" || snap.Config.Match.CredentialIdentity == "*") {
				return snap, true
			}
		}
		return scheduler.ProviderPoolSnapshot{}, false
	}
	options.SelectRoute = func(routes []workflow.Binding) (workflow.Binding, bool) {
		for _, route := range routes {
			if snap, ok := matchPool(route); ok {
				if snap.Pause != nil && snap.Pause.Paused {
					continue
				}
				if snap.Quota != nil && snap.Quota.Active(now) {
					continue
				}
				if _, ok := poolQuotaMarkers[snap.Config.ID]; ok {
					continue
				}
			}
			if _, ok := poolQuotaMarkers[route.Provider]; ok {
				continue
			}
			if byBinding[[2]string{route.Provider, route.Model}].Eligible(now) {
				return route, true
			}
		}
		return workflow.Binding{}, false
	}
	options.QuotaExhausted = func(routes []workflow.Binding) (time.Time, bool) {
		var earliest time.Time
		found := false
		for _, route := range routes {
			if snap, ok := matchPool(route); ok {
				if snap.Quota != nil && snap.Quota.Active(now) {
					r := snap.Quota.ResetAt
					if !found || r.Before(earliest) {
						earliest, found = r, true
					}
					continue
				}
				if r, ok := poolQuotaMarkers[snap.Config.ID]; ok {
					if !found || r.Before(earliest) {
						earliest, found = r, true
					}
					continue
				}
			}
			if r, ok := poolQuotaMarkers[route.Provider]; ok {
				if !found || r.Before(earliest) {
					earliest, found = r, true
				}
				continue
			}
			h := byBinding[[2]string{route.Provider, route.Model}]
			if h.QuotaActive(now) {
				r := h.QuotaResetAt
				if r.IsZero() {
					r = h.QuotaExhaustedUntil
				}
				if !found || r.Before(earliest) {
					earliest, found = r, true
				}
			}
		}
		return earliest, found
	}
}

func (d *Daemon) configureNonLiveRouteOptions(options *engine.Options) {
	if d.store == nil {
		return
	}
	health, err := d.store.ProviderHealth(d.ctx)
	if err != nil {
		return
	}
	hasQuota := false
	byBinding := map[[2]string]providerhealth.Health{}
	now := time.Now().UTC()
	for _, h := range health {
		byBinding[[2]string{h.Provider, h.Model}] = h
		if h.QuotaActive(now) {
			hasQuota = true
		}
	}
	rawMarkers, mErr := d.store.MarkersWithPrefix(d.ctx, "provider-pool-quota/", 100)
	if mErr == nil {
		for _, raw := range rawMarkers {
			var m struct {
				ExhaustedUntil time.Time `json:"exhausted_until"`
			}
			if json.Unmarshal(raw, &m) == nil && !m.ExhaustedUntil.IsZero() && now.Before(m.ExhaustedUntil) {
				hasQuota = true
				break
			}
		}
	}
	if !hasQuota {
		return
	}
	d.configureLiveRouteOptions(options, byBinding, now)
}
