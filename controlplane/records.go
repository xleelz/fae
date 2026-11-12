package controlplane

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/workflow"
)

// indefinitely is the mirrored deferral of a task deferred without a time.
var indefinitely = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// ErrTransitionEvidence refuses a checkpoint whose recorded transitions do not
// lead from the projected state to its new state, or whose evidence is missing
// (FAE-WF-022: incomplete evidence fails closed).
var ErrTransitionEvidence = errors.New("transition evidence incomplete")

// taskEnvelope is the plane's read of a daemon job checkpoint: the engine job
// and the admission fields the task row projects.
type taskEnvelope struct {
	Job       engine.Job `json:"job"`
	Admission struct {
		RunID    string `json:"run_id"`
		Priority int    `json:"priority"`
		Mode     string `json:"mode"`
	} `json:"admission"`
}

func decodeTaskEnvelope(j JobSnapshot) (taskEnvelope, error) {
	var e taskEnvelope
	dec := json.NewDecoder(bytes.NewReader(j.Payload))
	dec.UseNumber()
	if err := dec.Decode(&e); err != nil {
		return e, fmt.Errorf("job payload is not a task envelope: %w", err)
	}
	if e.Job.TaskID != j.ID || string(e.Job.State) != j.State || int64(e.Job.Revision) != j.WorkflowRevision || e.Job.Document.ID != j.WorkflowID {
		return e, errors.New("job payload does not match its snapshot identity")
	}
	return e, nil
}

// inject is the fault-injection seam between the writes of one checkpoint.
func (s *Store) inject(step string) error {
	if s.fault != nil {
		return s.fault(step)
	}
	return nil
}

// taskCursor is the projected task row: its state and how much of the job's
// append-only history and provenance is already recorded.
type taskCursor struct {
	State, Molecule     string
	History, Provenance int
	Exists              bool
}

func readTaskCursor(ctx context.Context, tx *sql.Tx, id string) (taskCursor, error) {
	var c taskCursor
	err := tx.QueryRowContext(ctx, `SELECT state,molecule_id,history_cursor,provenance_cursor FROM tasks WHERE task_id=? FOR UPDATE`, id).Scan(&c.State, &c.Molecule, &c.History, &c.Provenance)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	c.Exists = err == nil
	return c, err
}

func transitionEvent(e engine.Event) bool {
	return e.From != "" && e.To != "" && (e.Kind == "transition" || e.Kind == "fan-cursor" || e.Kind == "migrated" && e.From != e.To)
}

type plannedArtifact struct {
	Row      ProvenanceRecord
	SchemaID string
	Version  int64
	Content  []byte
}
type plannedTransition struct {
	From, To, Owner, Note string
	Attempt               int
	Evidence              []byte
}
type plannedDecision struct {
	Index                     int
	Kind, Role, Note, Session string
	Payload                   []byte
}

// recordPlan is everything one checkpoint appends, computed without writing.
type recordPlan struct {
	Artifacts   []plannedArtifact
	Transitions []plannedTransition
	Decisions   []plannedDecision
}

// planRecords derives a checkpoint's new relational records from the job. It
// is pure: a job it refuses writes nothing. Every new transition must follow
// the projected state, a completion must cite the artifacts it recorded, and
// every new provenance entry must name a stored, content-addressed artifact.
func planRecords(j JobSnapshot, job engine.Job, c taskCursor, molecule, actor string) (recordPlan, error) {
	var p recordPlan
	if len(job.History) < c.History || len(job.Provenance) < c.Provenance {
		return p, fmt.Errorf("%w: task %s history or provenance shrank below its recorded cursor", ErrTransitionEvidence, j.ID)
	}
	artifacts := map[string][]string{}
	for _, prov := range job.Provenance[c.Provenance:] {
		a, err := planArtifact(job, prov, molecule)
		if err != nil {
			return p, err
		}
		p.Artifacts = append(p.Artifacts, a)
		artifacts[prov.DispatchID] = append(artifacts[prov.DispatchID], prov.ArtifactID)
	}
	// A decision's session is the role session of the dispatch it decided.
	sessions := map[string]string{}
	for _, prov := range job.Provenance {
		if prov.Session != "" {
			sessions[prov.DispatchID] = prov.Session
		}
	}
	state := c.State
	related := []engine.Event{}
	for n, ev := range job.History[c.History:] {
		index := c.History + n
		if kind, role := decisionKind(ev, actor); kind != "" {
			raw, err := json.Marshal(ev)
			if err != nil {
				return p, err
			}
			p.Decisions = append(p.Decisions, plannedDecision{Index: index, Kind: kind, Role: role, Note: ev.Cause, Session: sessions[ev.DispatchID], Payload: raw})
		}
		if !transitionEvent(ev) {
			related = append(related, ev)
			continue
		}
		if state != "" && string(ev.From) != state {
			return p, fmt.Errorf("%w: task %s transition %s -> %s does not follow %s", ErrTransitionEvidence, j.ID, ev.From, ev.To, state)
		}
		if ev.Kind == "fan-cursor" {
			fan, ok := job.Document.Stages[ev.Stage]
			_, inBranch := workflow.BranchOf(job.Document, ev.Stage)[string(ev.From)]
			joined := false
			for _, following := range job.History[index+1:] {
				if following.Kind == "join" && following.Stage == fan.Join {
					joined = true
					break
				}
				if transitionEvent(following) {
					break
				}
			}
			if !ok || fan.Primitive != workflow.FanOut || string(ev.To) != fan.Join || (string(ev.From) != ev.Stage && !inBranch) || !joined {
				return p, fmt.Errorf("%w: invalid fan checkpoint projection", ErrTransitionEvidence)
			}
		}
		cited := []string{}
		if dispatch, completed := strings.CutPrefix(ev.Cause, "completed "); completed {
			if cited = artifacts[dispatch]; len(cited) == 0 {
				return p, fmt.Errorf("%w: task %s completion %s recorded no artifact", ErrTransitionEvidence, j.ID, dispatch)
			}
		}
		raw, err := json.Marshal(map[string]any{"task_id": j.ID, "history_index": index, "transition": ev, "related": related, "artifacts": cited})
		if err != nil {
			return p, err
		}
		owner := ev.Stage
		if strings.HasPrefix(ev.Cause, "Operator") {
			owner, _, _ = strings.Cut(ev.Cause, ":")
		} else if ev.Kind == "migrated" {
			owner = "OperatorMigrate"
		}
		if owner == "" {
			owner = "kernel"
		}
		p.Transitions = append(p.Transitions, plannedTransition{From: string(ev.From), To: string(ev.To), Owner: owner, Note: ev.Cause, Attempt: ev.Attempt, Evidence: raw})
		state, related = string(ev.To), []engine.Event{}
	}
	if state != "" && state != j.State {
		return p, fmt.Errorf("%w: task %s state %s has no transition from %s", ErrTransitionEvidence, j.ID, j.State, state)
	}
	return p, nil
}

func planArtifact(job engine.Job, p engine.Provenance, molecule string) (plannedArtifact, error) {
	var artifact *workflow.Evidence
	for i := range job.Evidence {
		if a := &job.Evidence[i]; a.ID == p.ArtifactID && a.Session == p.Session && a.Stage == p.Stage {
			artifact = a
			break
		}
	}
	if artifact == nil {
		return plannedArtifact{}, fmt.Errorf("%w: provenance %s names no stored artifact", ErrTransitionEvidence, p.ArtifactID)
	}
	raw, err := workflow.EvidenceJSON(*artifact)
	if err != nil {
		return plannedArtifact{}, err
	}
	hash, content, err := ContentHash(raw)
	if err != nil {
		return plannedArtifact{}, err
	}
	if hash != artifact.ID {
		return plannedArtifact{}, fmt.Errorf("%w: artifact %s content does not match its address", ErrTransitionEvidence, artifact.ID)
	}
	commit := ""
	if p.Primitive == workflow.VcsCommit || p.Primitive == workflow.Integrate {
		commit, _ = artifact.Fields["sha"].(string)
	}
	row := ProvenanceRecord{ArtifactID: hash, TaskID: job.TaskID, MoleculeID: molecule, Kind: artifact.Type, DispatchID: p.DispatchID, Stage: p.Stage, Primitive: string(p.Primitive), Role: p.Role, Provider: p.Provider, Model: p.Model, SessionID: p.Session, Transcript: p.Transcript, Tree: p.Tree, Inputs: p.Inputs, CommitRef: commit, ExecutionKind: p.ExecutionKind, WorkflowDigest: p.WorkflowHash, WorkflowRevision: int64(p.WorkflowRevision), ProducedAt: p.ProducedAt.UTC(), CheckpointSession: p.CheckpointSession, CheckpointTree: p.CheckpointTree, ResumedFromCheckpoint: p.ResumedFromCheckpoint, ReadRoots: p.ReadRoots, Reads: p.Reads}
	return plannedArtifact{Row: row, SchemaID: artifact.Type, Version: int64(artifact.Version), Content: content}, nil
}

// decisionKind classifies the history entries that are decisions: gate
// results, escalations, operator task controls and migrations.
func decisionKind(ev engine.Event, actor string) (string, string) {
	switch {
	case ev.Kind == "gate":
		return "gate:" + ev.Stage, "engine"
	case ev.Kind == "escalation" || ev.Kind == "transition" && ev.To == domain.Escalated:
		return "escalation", "engine"
	case ev.Kind == "task-control":
		method, _, _ := strings.Cut(ev.Cause, ":")
		return "operator:" + method, actor
	case ev.Kind == "migrated":
		return "migration", actor
	}
	return "", ""
}

// recordJobTx projects one job checkpoint into the relational records inside
// the checkpoint's own transaction: new artifacts with append-only provenance,
// one transition row per state change with its evidence digest, decision rows,
// the task projection and the molecule's end. History and provenance are
// append-only in the job; the task row's cursors name what is already recorded,
// so each entry is written exactly once (FAE-WF-022, -051, FAE-CP-011). The
// envelope remains the engine's working state; these rows are authoritative
// for reads and audit.
func (s *Store) recordJobTx(ctx context.Context, tx *sql.Tx, a Audit, j JobSnapshot, molecule string) error {
	if !validDigest(molecule) {
		return errors.New("task requires a bound molecule")
	}
	e, err := decodeTaskEnvelope(j)
	if err != nil {
		return err
	}
	c, err := readTaskCursor(ctx, tx, j.ID)
	if err != nil {
		return err
	}
	plan, err := planRecords(j, e.Job, c, molecule, a.Actor)
	if err != nil {
		return err
	}
	return s.writeRecordsTx(ctx, tx, a, j, e, c, plan, molecule, true)
}

// families projects the sessions and external calls the new artifacts name,
// and the task row's Board columns; the version-8 backfill runs before those
// tables and columns exist (Migration9, Migration11).
func (s *Store) writeRecordsTx(ctx context.Context, tx *sql.Tx, a Audit, j JobSnapshot, e taskEnvelope, c taskCursor, plan recordPlan, molecule string, families bool) error {
	now := time.Now().UTC()
	for _, artifact := range plan.Artifacts {
		if err := s.inject("artifact"); err != nil {
			return err
		}
		if err := s.recordArtifactTx(ctx, tx, artifact.Row, artifact.SchemaID, artifact.Version, artifact.Content, now); err != nil {
			return err
		}
		// The session and the external call an artifact names are projected
		// with it (FAE-CP-011, FAE-EXT-006).
		if !families {
			continue
		}
		if err := s.upsertSessionTx(ctx, tx, artifact.Row); err != nil {
			return err
		}
		if artifact.Row.Primitive == string(workflow.ExternalEffect) {
			var fields map[string]any
			if json.Unmarshal(artifact.Content, &fields) == nil {
				if effect, ok := effectFromArtifact(fields); ok {
					effect.Task = j.ID
					if err := s.upsertEffectTx(ctx, tx, effect, now); err != nil {
						return err
					}
				}
			}
		}
	}
	message := auditMessage(ctx, a)
	for _, t := range plan.Transitions {
		evidence, content, err := ContentHash(t.Evidence)
		if err != nil {
			return err
		}
		if err = s.putBlob(ctx, tx, content); err != nil {
			return err
		}
		if err = s.inject("transition"); err != nil {
			return err
		}
		note, err := s.inlineOrRef(ctx, tx, []byte(t.Note))
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO transitions (task_id,from_state,to_state,owner,attempt,evidence_digest,note,message_id,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, j.ID, t.From, t.To, bounded(t.Owner, 255), t.Attempt, evidence, note, message, now); err != nil {
			return err
		}
	}
	superseded := map[string]string{}
	for i, d := range plan.Decisions {
		if err := s.inject("decision"); err != nil {
			return err
		}
		// Microsecond offsets keep one checkpoint's decisions in history order.
		if err := s.recordDecisionTx(ctx, tx, j.ID, d, superseded, now.Add(time.Duration(i)*time.Microsecond)); err != nil {
			return err
		}
	}
	if err := s.inject("task"); err != nil {
		return err
	}
	job := e.Job
	disposition, err := json.Marshal(job.Disposition)
	if err != nil {
		return err
	}
	var escalation, commit, review any
	if job.EscalationKind != "" {
		escalation = string(job.EscalationKind)
	}
	if sha := job.LatestCommitHead(); sha != "" {
		commit = bounded(sha, 255)
	}
	if job.ReviewID != "" {
		review = bounded(job.ReviewID, 255)
	}
	reason, err := s.inlineOrRef(ctx, tx, []byte(job.Reason))
	if err != nil {
		return err
	}
	columns, values, updates := `task_id,run_id,molecule_id,target_id,target_revision,state,attempt,attempt_limit,priority,disposition,escalation_kind,escalation_reason,commit_ref,review_ref,history_cursor,provenance_cursor,created_at,updated_at`, `?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?`, `run_id=VALUES(run_id),molecule_id=VALUES(molecule_id),target_id=VALUES(target_id),target_revision=VALUES(target_revision),state=VALUES(state),attempt=VALUES(attempt),attempt_limit=VALUES(attempt_limit),priority=VALUES(priority),disposition=VALUES(disposition),escalation_kind=VALUES(escalation_kind),escalation_reason=VALUES(escalation_reason),commit_ref=VALUES(commit_ref),review_ref=VALUES(review_ref),history_cursor=VALUES(history_cursor),provenance_cursor=VALUES(provenance_cursor),updated_at=VALUES(updated_at)`
	args := []any{j.ID, bounded(e.Admission.RunID, 255), molecule, j.TargetID, j.TargetRevision, j.State, job.Attempts, max(job.AttemptLimit(), 1), max(e.Admission.Priority, 10), string(disposition), escalation, reason, commit, review, len(job.History), len(job.Provenance), now, now}
	// The Board columns (schema 11) exist only once the plane is current; the
	// version-8 backfill runs before them.
	if families {
		kind, mode := BoardColumns(job.ExecutionKind, e.Admission.Mode, e.Admission.RunID, e.Admission.Priority)
		columns, values, updates = columns+`,execution_kind,mode`, values+`,?,?`, updates+`,execution_kind=VALUES(execution_kind),mode=VALUES(mode)`
		args = append(args, kind, mode)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tasks (`+columns+`) VALUES (`+values+`) ON DUPLICATE KEY UPDATE `+updates, args...); err != nil {
		return err
	}
	if families {
		if err := s.writeTreeIndex(ctx, tx, j, job); err != nil {
			return err
		}
	}
	// The backlog row mirrors the deferral so backlog reads need no snapshot.
	var deferred any
	if job.Deferred {
		deferred = indefinitely
		if !job.DeferredUntil.IsZero() {
			deferred = job.DeferredUntil.UTC()
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE backlog_items SET deferred_until=? WHERE item_id=?`, deferred, j.ID); err != nil {
		return err
	}
	if c.Exists && c.Molecule != molecule {
		if err = endMoleculeTx(ctx, tx, c.Molecule); err != nil {
			return err
		}
	}
	if job.State.Terminal() {
		return endMoleculeTx(ctx, tx, molecule)
	}
	return nil
}

// recordDecisionTx appends one decision; it supersedes the task's previous
// decision of the same kind.
func (s *Store) recordDecisionTx(ctx context.Context, tx *sql.Tx, task string, d plannedDecision, superseded map[string]string, now time.Time) error {
	hash, content, err := ContentHash(d.Payload)
	if err != nil {
		return err
	}
	if err = s.putBlob(ctx, tx, content); err != nil {
		return err
	}
	previous, known := superseded[d.Kind]
	if !known {
		err = tx.QueryRowContext(ctx, `SELECT id FROM decisions WHERE task_id=? AND kind=? ORDER BY created_at DESC,id DESC LIMIT 1`, task, bounded(d.Kind, 255)).Scan(&previous)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	id := Digest([]byte(fmt.Sprintf("%s\x00decision\x00%d", task, d.Index)))
	var supersedes any
	if previous != "" {
		supersedes = previous
	}
	superseded[d.Kind] = id
	rationale, err := s.inlineOrRef(ctx, tx, []byte(d.Note))
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO decisions (id,task_id,kind,payload_digest,rationale,role,session_id,supersedes_id,created_at) VALUES (?,?,?,?,?,?,?,?,?)`, id, task, bounded(d.Kind, 255), hash, rationale, bounded(d.Role, 255), bounded(d.Session, 255), supersedes, now)
	return err
}

// ProvenanceRecord is one immutable provenance row (FAE-WF-051).
type ProvenanceRecord struct {
	ID         string             `json:"provenance_id,omitempty"`
	ArtifactID string             `json:"artifact_id"`
	TaskID     string             `json:"task_id"`
	MoleculeID string             `json:"molecule_id"`
	Kind       string             `json:"kind"`
	DispatchID string             `json:"dispatch_id"`
	Stage      string             `json:"stage"`
	Primitive  string             `json:"primitive"`
	Role       string             `json:"role"`
	Provider   string             `json:"provider,omitempty"`
	Model      string             `json:"model,omitempty"`
	SessionID  string             `json:"session_id"`
	Transcript *engine.Transcript `json:"transcript,omitempty"`
	// inputsRef is an input list stored by reference, until resolved.
	inputsRef             *BlobRef
	Tree                  string               `json:"tree"`
	Inputs                []string             `json:"inputs"`
	CommitRef             string               `json:"commit_ref,omitempty"`
	ExecutionKind         string               `json:"execution_kind,omitempty"`
	WorkflowDigest        string               `json:"workflow_digest,omitempty"`
	WorkflowRevision      int64                `json:"workflow_revision,omitempty"`
	ProducedAt            time.Time            `json:"produced_at"`
	CheckpointSession     string               `json:"checkpoint_session,omitempty"`
	CheckpointTree        string               `json:"checkpoint_tree,omitempty"`
	ResumedFromCheckpoint bool                 `json:"resumed_from_checkpoint,omitempty"`
	ReadRoots             map[string]string    `json:"read_roots,omitempty"`
	Reads                 *workflow.ReadPolicy `json:"reads,omitempty"`
}

// RefusedReadRecord is an audited refused-read entry (FAE-SEC-008).
type RefusedReadRecord struct {
	Task string `json:"task"`
	Role string `json:"role"`
	Path string `json:"path"`
}

// RecordRefusedRead writes an audit record naming (task, role, path) for a read
// refused by the effective read policy (FAE-SEC-008).
func (s *Store) RecordRefusedRead(ctx context.Context, task, role, path string) error {
	raw, err := json.Marshal(RefusedReadRecord{Task: task, Role: role, Path: path})
	if err != nil {
		return err
	}
	return s.Mutate(ctx, Audit{
		Actor:   "daemon",
		Action:  "read-policy-refusal",
		Inputs:  raw,
		Outcome: fmt.Sprintf("refused (%s, %s, %s)", task, role, path),
	}, func(context.Context, *sql.Tx) error { return nil })
}

// recordArtifactTx writes the artifact once per (content, task) and appends a
// provenance row for this production. Provenance is never updated: the plane's
// triggers refuse an update, a delete or a replacement (Migration8).
func (s *Store) recordArtifactTx(ctx context.Context, tx *sql.Tx, row ProvenanceRecord, schemaID string, version int64, content []byte, now time.Time) error {
	if row.Inputs == nil {
		row.Inputs = []string{}
	}
	row.ID = ""
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	id, record, err := ContentHash(raw)
	if err != nil {
		return err
	}
	// Oversized provenance is stored by reference (FAE-CP-012).
	stored, err := s.inlineOrRef(ctx, tx, record)
	if err != nil {
		return err
	}
	if err = s.putBlob(ctx, tx, content); err != nil {
		return err
	}
	var present int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE artifact_id=? AND task_id=?`, row.ArtifactID, row.TaskID).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO artifacts (artifact_id,task_id,kind,schema_id,schema_version,content_digest,provenance,created_at) VALUES (?,?,?,?,?,?,?,?)`, row.ArtifactID, row.TaskID, bounded(row.Kind, 255), bounded(schemaID, 255), version, row.ArtifactID, stored, now); err != nil {
			return err
		}
	}
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM provenance WHERE provenance_id=?`, id).Scan(&present); err != nil || present > 0 {
		return err
	}
	rawInputs, err := json.Marshal(row.Inputs)
	if err != nil {
		return err
	}
	inputs, err := s.inlineOrRef(ctx, tx, rawInputs)
	if err != nil {
		return err
	}
	transcript := ""
	if row.Transcript != nil {
		b, err := json.Marshal(row.Transcript)
		if err != nil {
			return err
		}
		transcript = string(b)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO provenance (provenance_id,artifact_id,task_id,molecule_id,kind,dispatch_id,stage,primitive,role,provider,model,session_id,transcript,tree_hash,inputs,commit_ref,execution_kind,workflow_digest,workflow_revision,produced_at,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, row.ArtifactID, row.TaskID, row.MoleculeID, bounded(row.Kind, 255), bounded(row.DispatchID, 1024), bounded(row.Stage, 255), bounded(row.Primitive, 64), bounded(row.Role, 255), bounded(row.Provider, 255), bounded(row.Model, 255), bounded(row.SessionID, 1024), transcript, bounded(row.Tree, 255), inputs, bounded(row.CommitRef, 255), bounded(row.ExecutionKind, 64), bounded(row.WorkflowDigest, 64), row.WorkflowRevision, row.ProducedAt, now)
	return err
}

// textLimit is the capacity of a TEXT column.
const textLimit = 65535

// bounded truncates to at most n bytes on a rune boundary.
func bounded(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// auditMessage is the authorizing message Mutate records for this write.
func auditMessage(ctx context.Context, a Audit) string {
	if authority, ok := ctx.Value(actionAuthorityKey{}).(actionAuthority); ok && a.MessageID == "" {
		return authority.Message
	}
	return a.MessageID
}
