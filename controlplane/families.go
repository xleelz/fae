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

// The record families of FAE-CP-011 that are derived state. Each projection
// below is written in the same transaction as the authoritative write it
// derives from, so it can never disagree with a committed authority, and no
// decision reads it back (FAE-GEN-011: one authority, one projection):
//
//	leases          <- resource_pools (the lease ledger, mutatePool)
//	ticket_outbox   <- ticket_emissions (EnqueueTicket, SaveTicket)
//	external_effects<- effect checkpoints (RecordEffect), ticket emissions and
//	                   the ExternalEffect artifact of the checkpoint
//	sessions        <- job provenance (the checkpoint's recordJobTx)
//	code_reviews    <- the review-lifecycle marker (PutMarker with CodeReview)
//	schedules       <- the cron, loop and interval schedule markers (PutMarker)
//
// engine_runs is itself the authority for an engine run's execution record
// (command, sanitized-environment delta, times); see RecordEngineRun.

// refLimit is the largest inline TEXT value a projection writes; a longer
// value is stored as a content-addressed blob and the column holds its
// reference (FAE-CP-012).
func (s *Store) inlineOrRef(ctx context.Context, tx *sql.Tx, content []byte) (string, error) {
	if len(content) <= min(s.options.InlineLimit, textLimit) {
		return string(content), nil
	}
	if err := s.putBlob(ctx, tx, content); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(map[string]BlobRef{"blob_ref": {Digest: Digest(content), Size: int64(len(content))}})
	return string(raw), nil
}

// blobRef recognizes a column value inlineOrRef stored by reference.
func blobRef(text string) (BlobRef, bool) {
	if !strings.HasPrefix(text, `{"blob_ref":`) {
		return BlobRef{}, false
	}
	var v map[string]BlobRef
	if json.Unmarshal([]byte(text), &v) != nil || len(v) != 1 || !validDigest(v["blob_ref"].Digest) {
		return BlobRef{}, false
	}
	return v["blob_ref"], true
}

// ResolveText reads a text column value, following a blob reference.
func (s *Store) ResolveText(ctx context.Context, text string) ([]byte, error) {
	if ref, ok := blobRef(text); ok {
		return s.GetBlob(ctx, ref)
	}
	return []byte(text), nil
}

// keyColumn fits an identity into a VARCHAR(255) key: longer identities are
// replaced by their digest.
func keyColumn(id string) string {
	if len(id) <= 255 {
		return id
	}
	return "sha256:" + Digest([]byte(id))
}

// LeaseID is the projected identity of one grant: a request ID may be reused
// after release, its queue time and owner may not.
func LeaseID(pool string, l scheduler.Lease) string {
	return Digest([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s", pool, l.ID, l.Owner.PID, l.Owner.Started, l.Queued.UTC().Format(time.RFC3339Nano))))
}

// projectLeasesTx records the holders a pool mutation granted or changed and
// closes the leases it released, with the reason from its events.
func projectLeasesTx(ctx context.Context, tx *sql.Tx, key, kind string, before, after []scheduler.Lease, reasons map[string]string, fallback string, now time.Time) error {
	old := map[string]scheduler.Lease{}
	for _, l := range before {
		old[LeaseID(key, l)] = l
	}
	held := map[string]bool{}
	for _, l := range after {
		id := LeaseID(key, l)
		held[id] = true
		if prior, ok := old[id]; ok && prior.Expires.Equal(l.Expires) && prior.Weight == l.Weight {
			continue
		}
		owner := fmt.Sprintf("pid:%d/start:%d", l.Owner.PID, l.Owner.Started)
		if _, err := tx.ExecContext(ctx, `INSERT INTO leases (lease_id,kind,scope_key,owner,weight,priority_class,granted_at,expires_at) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE weight=VALUES(weight),expires_at=VALUES(expires_at)`, id, bounded(kind, 64), key, owner, l.Weight, fmt.Sprintf("class-%d", l.Class), now, l.Expires.UTC()); err != nil {
			return err
		}
	}
	for id, l := range old {
		if held[id] {
			continue
		}
		reason := reasons[l.ID]
		if reason == "" {
			reason = fallback
		}
		if _, err := tx.ExecContext(ctx, `UPDATE leases SET released_at=?,release_reason=? WHERE lease_id=? AND released_at IS NULL`, now, bounded(reason, textLimit), id); err != nil {
			return err
		}
	}
	return nil
}

// effectRow is one external call (FAE-EXT-006): keyed by the intent's
// idempotency key, so every attempt of one logical call shares one row.
type effectRow struct {
	Key, Task, Tool, Digest, Session string
	Arguments                        map[string]string
	ExternalID, Reconciliation       string
	Outcome                          string
}

// intentView is the audited part of an external intent; the daemon records
// external.Intent values, decoded here without importing the adapter.
type intentView struct {
	Key            string            `json:"key"`
	IdempotencyKey string            `json:"idempotency_key"`
	Task           string            `json:"task"`
	Tool           string            `json:"tool"`
	Arguments      map[string]string `json:"arguments"`
	Digest         string            `json:"digest"`
	ArgumentDigest string            `json:"argument_digest"`
	Session        string            `json:"session"`
}

func (i *intentView) normalize() {
	if i.Key == "" && i.IdempotencyKey != "" {
		i.Key = i.IdempotencyKey
	}
	if i.Digest == "" && i.ArgumentDigest != "" {
		i.Digest = i.ArgumentDigest
	}
	if i.Digest == "" && i.Key != "" {
		i.Digest = Digest([]byte(i.Key))
	}
}

func (i intentView) valid() bool { return i.Key != "" && i.Tool != "" && i.Digest != "" }

type receiptView struct {
	Key            string `json:"key"`
	IdempotencyKey string `json:"idempotency_key"`
	Task           string `json:"task"`
	Tool           string `json:"tool"`
	Digest         string `json:"digest"`
	ArgumentDigest string `json:"argument_digest"`
	ExternalID     string `json:"external_id"`
	State          string `json:"state"`
}

func (r *receiptView) normalize() {
	if r.Key == "" && r.IdempotencyKey != "" {
		r.Key = r.IdempotencyKey
	}
	if r.Digest == "" && r.ArgumentDigest != "" {
		r.Digest = r.ArgumentDigest
	}
	if r.Digest == "" && r.Key != "" {
		r.Digest = Digest([]byte(r.Key))
	}
}

// upsertEffectTx records a call's intent, or advances its reported identity,
// reconciliation state and outcome. Pinned values are names and the literal
// values the workflow pinned; the argument digest covers the whole call.
func (s *Store) upsertEffectTx(ctx context.Context, tx *sql.Tx, r effectRow, now time.Time) error {
	if r.Key == "" {
		return nil
	}
	id := keyColumn(r.Key)
	var present int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM external_effects WHERE effect_id=?`, id).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		if r.Tool == "" || r.Digest == "" {
			return nil
		}
		if r.Arguments == nil {
			r.Arguments = map[string]string{}
		}
		raw, err := json.Marshal(r.Arguments)
		if err != nil {
			return err
		}
		pinned, err := s.inlineOrRef(ctx, tx, raw)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO external_effects (effect_id,task_id,tool,argument_digest,pinned_values,external_id,reconciliation,created_at,session_id,outcome,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, id, bounded(r.Task, MaxTaskIDBytes), bounded(r.Tool, 255), bounded(r.Digest, 64), pinned, nullable(bounded(r.ExternalID, 255)), bounded(r.Reconciliation, 64), now, bounded(r.Session, 3072), bounded(r.Outcome, 64), now)
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE external_effects SET external_id=COALESCE(?,external_id),reconciliation=?,outcome=?,updated_at=? WHERE effect_id=?`, nullable(bounded(r.ExternalID, 255)), bounded(r.Reconciliation, 64), bounded(r.Outcome, 64), now, id)
	return err
}

// effectFromCheckpoint reads an effect checkpoint as an external call: an
// intent, or an observation of a reported identity (the observation carries
// its intent, or is a bare receipt keyed like it).
func effectFromCheckpoint(phase string, payload []byte) (effectRow, bool) {
	switch phase {
	case "intent":
		var i intentView
		if json.Unmarshal(payload, &i) != nil {
			return effectRow{}, false
		}
		i.normalize()
		if !i.valid() {
			return effectRow{}, false
		}
		return effectRow{Key: i.Key, Task: i.Task, Tool: i.Tool, Digest: i.Digest, Session: i.Session, Arguments: i.Arguments, Reconciliation: "intended", Outcome: "intended"}, true
	case "observed", "performed":
		defaultState := "reported"
		if phase == "performed" {
			defaultState = "performed"
		}
		var o struct {
			Intent     *intentView  `json:"intent"`
			Reported   *receiptView `json:"reported"`
			ExternalID string       `json:"external_id"`
			Outcome    string       `json:"outcome"`
		}
		if json.Unmarshal(payload, &o) == nil && o.Intent != nil {
			o.Intent.normalize()
			if o.Intent.valid() && (o.Reported != nil || o.Outcome != "" || o.ExternalID != "" || phase == "performed") {
				reconciliation, outcome := defaultState, defaultState
				if o.Outcome != "" {
					reconciliation, outcome = o.Outcome, o.Outcome
					if o.Outcome == "reconciled" {
						reconciliation = "validated"
					}
				}
				externalID := o.ExternalID
				if o.Reported != nil && o.Reported.ExternalID != "" {
					externalID = o.Reported.ExternalID
				}
				return effectRow{Key: o.Intent.Key, Task: o.Intent.Task, Tool: o.Intent.Tool, Digest: o.Intent.Digest, Session: o.Intent.Session, Arguments: o.Intent.Arguments, ExternalID: externalID, Reconciliation: reconciliation, Outcome: outcome}, true
			}
		}
		var r receiptView
		if json.Unmarshal(payload, &r) == nil {
			r.normalize()
			if r.Key != "" && (r.ExternalID != "" || phase == "performed") {
				return effectRow{Key: r.Key, Task: r.Task, Tool: r.Tool, Digest: r.Digest, ExternalID: r.ExternalID, Reconciliation: defaultState, Outcome: defaultState}, true
			}
		}
	}
	return effectRow{}, false
}

// effectFromArtifact reads a checkpointed ExternalEffect output: the call's
// final reconciliation, recorded once with the artifact that cites it.
func effectFromArtifact(fields map[string]any) (effectRow, bool) {
	key, _ := fields["idempotency_key"].(string)
	state, _ := fields["reconciliation_state"].(string)
	if key == "" || state == "" {
		return effectRow{}, false
	}
	tool, _ := fields["tool"].(string)
	digest, _ := fields["argument_digest"].(string)
	id, _ := fields["external_id"].(string)
	reconciliation := state
	if state == "reconciled" {
		reconciliation = "validated"
	}
	return effectRow{Key: key, Tool: tool, Digest: digest, ExternalID: id, Reconciliation: reconciliation, Outcome: state}, true
}

// upsertOutboxTx projects one ticket emission and, once it holds a pinned
// intent, the external call it makes.
func (s *Store) upsertOutboxTx(ctx context.Context, tx *sql.Tx, r TicketEmission, now time.Time) error {
	var lastError any
	if r.LastError != "" {
		lastError = Digest([]byte(r.LastError))
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ticket_outbox (outbox_key,task_id,room,position,ticket_id,desired_step,started,emitted,attempts,last_error_digest,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE ticket_id=VALUES(ticket_id),started=VALUES(started),emitted=VALUES(emitted),attempts=VALUES(attempts),last_error_digest=VALUES(last_error_digest),updated_at=VALUES(updated_at)`, r.Key, r.Task, r.Room, r.Position, bounded(r.TicketID, 255), bounded(r.Step, 255), r.Started, r.Emitted, r.Attempts, lastError, now); err != nil {
		return err
	}
	if len(r.Intent) == 0 {
		return nil
	}
	var pinned struct {
		Intent intentView `json:"intent"`
	}
	if json.Unmarshal(r.Intent, &pinned) != nil || !pinned.Intent.valid() {
		return nil
	}
	i := pinned.Intent
	e := effectRow{Key: i.Key, Task: r.Task, Tool: i.Tool, Digest: i.Digest, Session: i.Session, Arguments: i.Arguments, ExternalID: r.TicketID, Reconciliation: "intended", Outcome: ticketOutcome(r)}
	switch {
	case r.Emitted:
		e.Reconciliation = "validated"
	case r.Started:
		e.Reconciliation = "started"
	}
	return s.upsertEffectTx(ctx, tx, e, now)
}

// upsertSessionTx records the role session a provenance entry names: its
// role, task, binding and pane, and when its transcript says it ran. The
// session's processes stay in the runtime's ownership registry; the owner
// recorded here is the daemon process that holds them.
func (s *Store) upsertSessionTx(ctx context.Context, tx *sql.Tx, r ProvenanceRecord) error {
	if r.SessionID == "" {
		return nil
	}
	binding, err := json.Marshal(map[string]string{"provider": r.Provider, "model": r.Model, "execution_kind": r.ExecutionKind, "stage": r.Stage})
	if err != nil {
		return err
	}
	launched := r.ProducedAt.UTC()
	var pane any
	if t := r.Transcript; t != nil {
		if !t.StartedAt.IsZero() {
			launched = t.StartedAt.UTC()
		}
		pane = nullable(bounded(t.Pane, 255))
	}
	var pid, start any
	if s.options.OwnerPID > 0 {
		pid, start = s.options.OwnerPID, nullable(bounded(s.options.OwnerStart, 255))
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO sessions (session_id,pane_id,role,task_id,binding,owner_pid,owner_start,launched_at) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE pane_id=COALESCE(VALUES(pane_id),pane_id)`, keyColumn(r.SessionID), pane, bounded(r.Role, 255), r.TaskID, string(binding), pid, start, launched)
	return err
}

// CodeReview projects the review-lifecycle marker of one task's review.
type CodeReview struct {
	ID, Task, Branch, Tree string
	Revisions              int
	Published, Merged      bool
}

// Projection is a record-family row written in the same transaction as the
// authoritative marker it derives from (PutMarker).
type Projection interface {
	projectTx(ctx context.Context, s *Store, tx *sql.Tx, marker []byte, now time.Time) error
}

func (r CodeReview) projectTx(ctx context.Context, s *Store, tx *sql.Tx, marker []byte, _ time.Time) error {
	if r.ID == "" {
		return nil
	}
	if r.Task == "" {
		return errors.New("code review projection requires its task")
	}
	publish, merge := "draft", "open"
	if r.Published {
		publish = "published"
	}
	if r.Merged {
		merge = "merged"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO code_reviews (review_id,task_id,branch,tree_hash,revisions,publish_state,merge_state,status_digest) VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE task_id=VALUES(task_id),branch=IF(VALUES(branch)='',branch,VALUES(branch)),tree_hash=IF(VALUES(tree_hash)='',tree_hash,VALUES(tree_hash)),revisions=VALUES(revisions),publish_state=VALUES(publish_state),merge_state=VALUES(merge_state),status_digest=VALUES(status_digest)`, keyColumn(r.ID), r.Task, bounded(r.Branch, 255), bounded(r.Tree, 255), r.Revisions, publish, merge, Digest(marker))
	return err
}

// scheduleMarkers are the durable schedule collections the daemon keeps as
// markers, and the kind each projects. A collection marker maps schedule id to
// schedule; an interval marker is one singleton schedule.
var scheduleMarkers = map[string]struct {
	Kind       string
	Collection bool
}{
	"cron-schedules":              {"cron", true},
	"provider-loops":              {"loop", true},
	"backup-creation-schedule":    {"interval", false},
	"backup-retention-schedule":   {"interval", false},
	"backup-replication-schedule": {"interval", false},
}

// scheduleView is the projected part of every schedule kind.
type scheduleView struct {
	Spec      json.RawMessage `json:"spec"`
	State     string          `json:"state"`
	Message   string          `json:"authorizing_message"`
	Interval  *int64          `json:"interval_seconds"`
	Keep      *int64          `json:"keep"`
	SessionID string          `json:"session_id"`
}

// projectSchedulesTx replaces the schedules a marker holds: each row's
// specification, lifecycle state and provenance (the authorizing message and
// the audited write). A schedule removed from the marker is removed here.
func (s *Store) projectSchedulesTx(ctx context.Context, tx *sql.Tx, a Audit, name string, payload []byte) error {
	marker, ok := scheduleMarkers[name]
	if !ok {
		return nil
	}
	views := map[string]scheduleView{}
	if marker.Collection {
		if err := json.Unmarshal(payload, &views); err != nil {
			return fmt.Errorf("schedule marker %s: %w", name, err)
		}
	} else {
		var v scheduleView
		if err := json.Unmarshal(payload, &v); err != nil {
			return fmt.Errorf("schedule marker %s: %w", name, err)
		}
		views[name] = v
	}
	ids := make([]string, 0, len(views))
	rowID := map[string]string{}
	expected := map[string]bool{}
	for id := range views {
		ids = append(ids, id)
		rowID[id] = name
		if marker.Collection {
			rowID[id] = keyColumn(name + "/" + id)
		}
		expected[rowID[id]] = true
	}
	sort.Strings(ids)
	rows, err := tx.QueryContext(ctx, `SELECT schedule_id FROM schedules WHERE marker=?`, name)
	if err != nil {
		return err
	}
	stale := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !expected[id] {
			stale = append(stale, id)
		}
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, id := range stale {
		if _, err = tx.ExecContext(ctx, `DELETE FROM schedules WHERE schedule_id=?`, id); err != nil {
			return err
		}
	}
	now := time.Now().UTC()
	for _, id := range ids {
		v := views[id]
		spec := []byte(v.Spec)
		if len(spec) == 0 {
			spec, _ = json.Marshal(map[string]any{"interval_seconds": v.Interval, "keep": v.Keep})
		}
		canonical, err := CanonicalJSON(spec)
		if err != nil {
			return err
		}
		specification, err := s.inlineOrRef(ctx, tx, canonical)
		if err != nil {
			return err
		}
		provenance, _ := json.Marshal(map[string]string{"marker": name, "authorizing_message": v.Message, "session_id": v.SessionID, "actor": a.Actor, "action": a.Action, "message_id": auditMessage(ctx, a)})
		state := v.State
		if state == "" {
			state = "unknown"
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schedules (schedule_id,kind,specification,state,provenance,marker,updated_at) VALUES (?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE kind=VALUES(kind),specification=VALUES(specification),state=VALUES(state),provenance=VALUES(provenance),updated_at=VALUES(updated_at)`, rowID[id], marker.Kind, specification, bounded(state, 64), string(provenance), name, now); err != nil {
			return err
		}
	}
	return nil
}

// EnvironmentDelta is the sanitized-environment delta of an engine run
// (FAE-ROB-030): variable names only, never values.
type EnvironmentDelta struct {
	// Removed names were in the daemon's environment and not passed.
	Removed []string `json:"removed"`
	// Set names were set explicitly by the declared command.
	Set []string `json:"set"`
	// Replaced names were passed with a value Fae's policy chose.
	Replaced []string `json:"replaced"`
}

// EngineRun is the record of one engine-run execution.
type EngineRun struct {
	TaskID, DispatchID string
	Command            []string
	Tree               string
	Environment        EnvironmentDelta
	ExitCode           int
	ExecutedCount      int
	OutputDigest       string
	Started, Finished  time.Time
}

// EngineRunID is stable for one execution: the dispatch, vector and start.
func EngineRunID(r EngineRun) string {
	raw, _ := json.Marshal([]any{r.DispatchID, r.Command, r.Started.UTC().Format(time.RFC3339Nano)})
	return Digest(raw)
}

// RecordEngineRun stores an engine run's execution record with its audit row.
// It is the authority for what ran and in which environment; the stage's
// artifact carries only the evidence the workflow gates on.
func (s *Store) RecordEngineRun(ctx context.Context, r EngineRun) (string, error) {
	if r.TaskID == "" || len(r.Command) == 0 || r.Tree == "" || r.Started.IsZero() || r.Finished.Before(r.Started) || !validDigest(r.OutputDigest) {
		return "", errors.New("complete engine run record required")
	}
	for _, names := range [][]string{r.Environment.Removed, r.Environment.Set, r.Environment.Replaced} {
		for _, name := range names {
			if name == "" || strings.ContainsAny(name, "=\x00") {
				return "", fmt.Errorf("environment delta holds a name only, not %q", name)
			}
		}
	}
	id := EngineRunID(r)
	command, err := json.Marshal(r.Command)
	if err != nil {
		return "", err
	}
	delta := r.Environment
	for _, names := range []*[]string{&delta.Removed, &delta.Set, &delta.Replaced} {
		if *names == nil {
			*names = []string{}
		}
		sort.Strings(*names)
	}
	environment, err := json.Marshal(delta)
	if err != nil {
		return "", err
	}
	inputs, _ := json.Marshal(map[string]any{"run_id": id, "task_id": r.TaskID, "dispatch_id": r.DispatchID, "exit_code": r.ExitCode, "executed_count": r.ExecutedCount})
	err = s.Mutate(ctx, Audit{Actor: "daemon", Action: "engine run", Inputs: inputs, Outcome: fmt.Sprintf("exit %d", r.ExitCode)}, func(ctx context.Context, tx *sql.Tx) error {
		commandText, err := s.inlineOrRef(ctx, tx, command)
		if err != nil {
			return err
		}
		environmentText, err := s.inlineOrRef(ctx, tx, environment)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO engine_runs (run_id,task_id,command,tree_hash,environment_delta,exit_code,executed_count,output_digest,started_at,finished_at,dispatch_id) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, id, r.TaskID, commandText, bounded(r.Tree, 255), environmentText, r.ExitCode, r.ExecutedCount, r.OutputDigest, r.Started.UTC(), r.Finished.UTC(), bounded(r.DispatchID, 1024))
		return err
	})
	return id, err
}

// EngineRuns reads one task's engine runs, oldest first.
func (s *Store) EngineRuns(ctx context.Context, task string, limit int) ([]EngineRunRow, error) {
	if task == "" || limit < 1 || limit > 10000 {
		return nil, errors.New("task and limit 1..10000 required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id,dispatch_id,command,tree_hash,environment_delta,exit_code,executed_count,output_digest,started_at,finished_at FROM engine_runs WHERE task_id=? ORDER BY started_at,run_id LIMIT ?`, task, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []EngineRunRow{}
	for rows.Next() {
		var r EngineRunRow
		var command, delta string
		if err = rows.Scan(&r.RunID, &r.DispatchID, &command, &r.Tree, &delta, &r.ExitCode, &r.ExecutedCount, &r.OutputDigest, &r.Started, &r.Finished); err != nil {
			return nil, err
		}
		r.Command, r.Environment = json.RawMessage(command), json.RawMessage(delta)
		out = append(out, r)
	}
	return out, rows.Err()
}

// EngineRunRow is a stored engine run; Command and Environment are the
// stored JSON (or a blob reference when they exceeded the inline limit).
type EngineRunRow struct {
	RunID         string          `json:"run_id"`
	DispatchID    string          `json:"dispatch_id"`
	Command       json.RawMessage `json:"command"`
	Tree          string          `json:"tree_hash"`
	Environment   json.RawMessage `json:"environment_delta"`
	ExitCode      int             `json:"exit_code"`
	ExecutedCount int             `json:"executed_count"`
	OutputDigest  string          `json:"output_digest"`
	Started       time.Time       `json:"started_at"`
	Finished      time.Time       `json:"finished_at"`
}

// ExternalEffectRow is one audited external call (FAE-EXT-006).
type ExternalEffectRow struct {
	EffectID       string          `json:"effect_id"`
	Tool           string          `json:"tool"`
	ArgumentDigest string          `json:"argument_digest"`
	Pinned         json.RawMessage `json:"pinned_values"`
	ExternalID     string          `json:"external_id,omitempty"`
	Reconciliation string          `json:"reconciliation"`
	Outcome        string          `json:"outcome"`
	Session        string          `json:"session_id"`
	Created        time.Time       `json:"created_at"`
}

// ExternalEffects reads one task's external calls, oldest first.
func (s *Store) ExternalEffects(ctx context.Context, task string, limit int) ([]ExternalEffectRow, error) {
	if task == "" || limit < 1 || limit > 10000 {
		return nil, errors.New("task and limit 1..10000 required")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT effect_id,tool,argument_digest,pinned_values,COALESCE(external_id,''),reconciliation,outcome,session_id,created_at FROM external_effects WHERE task_id=? ORDER BY created_at,effect_id LIMIT ?`, task, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ExternalEffectRow{}
	for rows.Next() {
		var r ExternalEffectRow
		var pinned string
		if err = rows.Scan(&r.EffectID, &r.Tool, &r.ArgumentDigest, &pinned, &r.ExternalID, &r.Reconciliation, &r.Outcome, &r.Session, &r.Created); err != nil {
			return nil, err
		}
		r.Pinned = json.RawMessage(pinned)
		out = append(out, r)
	}
	return out, rows.Err()
}
