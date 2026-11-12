package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/xleelz/fae/scheduler"
)

// DispatchPause is one durable FAE-RT-018 scoped dispatch pause record.
type DispatchPause struct {
	Scope     string    `json:"scope"`
	Target    string    `json:"target"`
	TaskIDs   []string  `json:"task_ids,omitempty"`
	Actor     string    `json:"actor"`
	Reason    string    `json:"reason"`
	MessageID string    `json:"message_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

func normalizeDispatchPause(p DispatchPause) (DispatchPause, error) {
	p.Scope = strings.TrimSpace(p.Scope)
	p.Target = strings.TrimSpace(p.Target)
	switch p.Scope {
	case "instance":
		if p.Target == "" {
			p.Target = "instance"
		}
	case "workflow", "pool":
		if p.Target == "" {
			return p, fmt.Errorf("dispatch pause scope %s requires target", p.Scope)
		}
	case "task_set":
		cleaned := make([]string, 0, len(p.TaskIDs))
		seen := map[string]bool{}
		for _, id := range p.TaskIDs {
			id = strings.TrimSpace(id)
			if id != "" && !seen[id] {
				seen[id] = true
				cleaned = append(cleaned, id)
			}
		}
		sort.Strings(cleaned)
		p.TaskIDs = cleaned
		if p.Target == "" && len(p.TaskIDs) > 0 {
			p.Target = "tasks:" + strings.Join(p.TaskIDs, ",")
		}
		if p.Target == "" {
			return p, errors.New("dispatch pause scope task_set requires run_id target or task_ids")
		}
	default:
		return p, fmt.Errorf("invalid dispatch pause scope %q (must be instance, workflow, task_set, or pool)", p.Scope)
	}
	return p, nil
}

func dispatchPauseMarkerName(scope, target string) string {
	return "dispatch-pause/" + scope + "/" + target
}

func (s *Store) putMarkerTx(ctx context.Context, tx *sql.Tx, name string, payload []byte) error {
	canonical, e := CanonicalJSON(payload)
	if e != nil {
		return e
	}
	if e := s.putBlob(ctx, tx, canonical); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `REPLACE INTO daemon_markers (name,payload_digest,payload_size) VALUES (?,?,?)`, name, Digest(canonical), len(canonical))
	return e
}

func (s *Store) deleteMarkerTx(ctx context.Context, tx *sql.Tx, name string) error {
	_, e := tx.ExecContext(ctx, `DELETE FROM daemon_markers WHERE name=?`, name)
	return e
}

// DeleteMarker removes a durable daemon marker in an audited transaction.
func (s *Store) DeleteMarker(ctx context.Context, a Audit, name string) error {
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		return s.deleteMarkerTx(ctx, tx, name)
	})
}

// PutDispatchPause records a durable FAE-RT-018 scoped dispatch pause with an
// audited control-plane mutation.
func (s *Store) PutDispatchPause(ctx context.Context, a Audit, p DispatchPause) error {
	var err error
	p, err = normalizeDispatchPause(p)
	if err != nil {
		return err
	}
	if p.Actor == "" {
		p.Actor = a.Actor
	}
	if p.MessageID == "" {
		p.MessageID = a.MessageID
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	raw, _ := json.Marshal(p)
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if e := s.putMarkerTx(ctx, tx, dispatchPauseMarkerName(p.Scope, p.Target), raw); e != nil {
			return e
		}
		return s.insertAuthorityDecisionTx(ctx, tx, "dispatch-pause", p.Scope+":"+p.Target, p.Actor, p.Reason, raw)
	})
}

// DeleteDispatchPause removes a durable FAE-RT-018 scoped dispatch pause with
// an audited control-plane mutation.
func (s *Store) DeleteDispatchPause(ctx context.Context, a Audit, scope, target string, taskIDs []string) error {
	p, err := normalizeDispatchPause(DispatchPause{Scope: scope, Target: target, TaskIDs: taskIDs, Actor: a.Actor, MessageID: a.MessageID, CreatedAt: time.Now().UTC()})
	if err != nil {
		return err
	}
	raw, _ := json.Marshal(p)
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		if e := s.deleteMarkerTx(ctx, tx, dispatchPauseMarkerName(p.Scope, p.Target)); e != nil {
			return e
		}
		return s.insertAuthorityDecisionTx(ctx, tx, "dispatch-unpause", p.Scope+":"+p.Target, a.Actor, a.Outcome, raw)
	})
}

func (s *Store) insertAuthorityDecisionTx(ctx context.Context, tx *sql.Tx, kind, target, actor, rationale string, payload []byte) error {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	now := time.Now().UTC()
	id := fmt.Sprintf("auth-%s-%s-%d", kind, target, now.UnixNano())
	if len(id) > 255 {
		id = id[:255]
	}
	if len(target) > 255 {
		target = target[:255]
	}
	if actor == "" {
		actor = "operator"
	}
	return s.insertDecision(ctx, tx, target, Decision{
		ID:        id,
		Kind:      kind,
		Payload:   payload,
		Rationale: rationale,
		Role:      actor,
	})
}

// RecordAuthorityDecision records an audited operator AuthorityDecision row
// (for example for provider pool cap resize or quota/pause controls).
func (s *Store) RecordAuthorityDecision(ctx context.Context, a Audit, kind, target, rationale string, payload json.RawMessage) error {
	return s.Mutate(ctx, a, func(ctx context.Context, tx *sql.Tx) error {
		return s.insertAuthorityDecisionTx(ctx, tx, kind, target, a.Actor, rationale, payload)
	})
}

// ListDispatchPauses returns all active durable dispatch pauses in deterministic
// (scope, target) order.
func (s *Store) ListDispatchPauses(ctx context.Context) ([]DispatchPause, error) {
	markers, e := s.MarkersWithPrefix(ctx, "dispatch-pause/", 1000)
	if e != nil {
		return nil, e
	}
	keys := make([]string, 0, len(markers))
	for k := range markers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]DispatchPause, 0, len(keys))
	for _, k := range keys {
		var p DispatchPause
		if json.Unmarshal(markers[k], &p) == nil && p.Scope != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// MirrorProviderPool projects a host-ledger provider concurrency pool into
// resource_pools ("provider_pool:" + poolID) so control-plane lease views can
// inspect it alongside role pools (FAE-SCH-005, FAE-SCH-015).
func (s *Store) MirrorProviderPool(ctx context.Context, poolID string, pool scheduler.Pool) error {
	key := "provider_pool:" + strings.TrimSpace(poolID)
	return s.transaction(ctx, func(ctx context.Context, tx *sql.Tx) error {
		text, e := s.encodePool(ctx, tx, pool)
		if e != nil {
			return e
		}
		_, e = tx.ExecContext(ctx, `REPLACE INTO resource_pools (pool_key,kind,version,payload) VALUES (?,'provider_pool',1,?)`, key, text)
		return e
	})
}
