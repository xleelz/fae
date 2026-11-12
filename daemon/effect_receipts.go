package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/effects"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/kernel"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

type effectIntent struct {
	Dispatch     engine.Dispatch         `json:"dispatch"`
	Worktree     worktree.Binding        `json:"worktree"`
	Head         string                  `json:"head"`
	TargetRef    string                  `json:"target_ref,omitempty"`
	TargetBefore string                  `json:"target_before,omitempty"`
	TargetAfter  string                  `json:"target_after,omitempty"`
	Commit       *effects.PreparedCommit `json:"commit,omitempty"`
}
type effectReceipt struct {
	Intent string        `json:"intent"`
	Result engine.Result `json:"result"`
	Head   string        `json:"head"`
	Target string        `json:"target,omitempty"`
}

func supportedEffect(p workflow.Primitive) bool {
	switch p {
	case workflow.ExternalEffect, workflow.HostCommand, workflow.FreezeArtifact, workflow.CompareFrozen, workflow.VcsCommit, workflow.Integrate:
		return true
	}
	return false
}
func effectKey(d engine.Dispatch) string {
	raw, _ := json.Marshal(d)
	h, _ := domain.ContentHash(raw)
	return "effect/" + h
}
func effectGit(ctx context.Context, w worktree.Binding, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = w.Path
	cmd.Env = cleanGitEnv()
	out, e := cmd.Output()
	return strings.TrimSpace(string(out)), e
}
func (x *liveExecutor) effectIntent(ctx context.Context, d engine.Dispatch, w worktree.Binding, p *effects.PreparedCommit, start func() error) (*effectIntent, error) {
	if !supportedEffect(d.Config.Primitive) {
		return nil, nil
	}
	head, e := effectGit(ctx, w, "rev-parse", "HEAD")
	if e != nil {
		return nil, e
	}
	i := &effectIntent{Dispatch: d, Worktree: w, Head: head, Commit: p}
	if d.Config.Primitive == workflow.VcsCommit {
		if p == nil {
			return nil, errors.New("commit intent requires prepared object")
		}
		i.TargetRef = "refs/heads/" + w.Branch
		i.TargetBefore = head
		i.TargetAfter = p.SHA
	}
	if d.Config.Primitive == workflow.Integrate {
		i.TargetRef = x.config.IntegrationRef
		i.TargetBefore, e = effectGit(ctx, w, "rev-parse", i.TargetRef)
		if e != nil {
			return nil, e
		}
		for _, input := range d.Inputs {
			if input.Type == "Commit" {
				i.TargetAfter, _ = input.Fields["sha"].(string)
			}
		}
		if i.TargetAfter == "" || head != i.TargetAfter {
			return nil, errors.New("integration intent lacks exact task commit")
		}
	}
	raw, _ := json.Marshal(i)
	if e = x.daemon.store.RecordEffect(ctx, effectKey(d)+"/intent", d.TaskID, "intent", raw); e != nil {
		return nil, e
	}
	if start != nil {
		if e = start(); e != nil {
			return nil, e
		}
	}
	if e = x.daemon.store.RecordEffect(ctx, effectKey(d)+"/started", d.TaskID, "started", json.RawMessage(`true`)); e != nil {
		return nil, e
	}
	return i, nil
}
func (x *liveExecutor) recordEffectResult(ctx context.Context, i *effectIntent, r engine.Result) error {
	if i == nil {
		return nil
	}
	head, e := effectGit(ctx, i.Worktree, "rev-parse", "HEAD")
	if e != nil {
		return e
	}
	receipt := effectReceipt{Intent: effectKey(i.Dispatch), Result: r, Head: head}
	if i.TargetRef != "" {
		receipt.Target, e = effectGit(ctx, i.Worktree, "rev-parse", i.TargetRef)
		if e != nil {
			return e
		}
	}
	raw, _ := json.Marshal(receipt)
	return x.daemon.store.RecordEffect(ctx, effectKey(i.Dispatch)+"/result", i.Dispatch.TaskID, "performed", raw)
}

// recordedEffect loads daemon-written evidence, never operator-supplied result
// JSON. Exact postconditions are remeasured before the engine may adopt it.
func (d *Daemon) recordedEffect(ctx context.Context, dispatch engine.Dispatch, w worktree.Binding, retired bool, doc workflow.Document) (engine.Result, error) {
	var zero engine.Result
	if dispatch.Config.Primitive == workflow.ExternalEffect {
		return d.reconcileExternal(ctx, dispatch, w)
	}
	raw, e := d.store.GetMarker(ctx, effectKey(dispatch)+"/intent")
	if e != nil {
		return zero, e
	}
	var intent effectIntent
	if e = decodeEffect(raw, &intent); e != nil {
		return zero, e
	}
	want, _ := json.Marshal(dispatch)
	got, _ := json.Marshal(intent.Dispatch)
	if string(want) != string(got) || intent.Worktree != w {
		return zero, errors.New("foreign effect intent")
	}
	tree, e := d.live.broker.Tree(ctx, w)
	if e != nil {
		return zero, e
	}
	head, e := effectGit(ctx, w, "rev-parse", "HEAD")
	if e != nil {
		return zero, e
	}
	target := ""
	if intent.TargetRef != "" {
		target, e = effectGit(ctx, w, "rev-parse", intent.TargetRef)
		if e != nil {
			return zero, e
		}
	}
	var receipt effectReceipt
	raw, e = d.store.GetMarker(ctx, effectKey(dispatch)+"/result")
	recorded := e == nil
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return zero, e
	}
	if recorded {
		if e = decode(raw, &receipt); e != nil {
			return zero, e
		}
	} else {
		// A CAS may finish before result persistence. Only its exact intent object
		// and terminal owned-process proof can reconstruct that missing receipt.
		if !retired {
			return zero, errors.New("effect retirement not proved; missing result cannot be reconstructed")
		}
		if dispatch.Config.Primitive != workflow.VcsCommit && dispatch.Config.Primitive != workflow.Integrate {
			return zero, errors.New("effect lacks completed receipt; outcome remains uncertain")
		}
		if target != intent.TargetAfter {
			return zero, errors.New("effect target differs from intended commit")
		}
		receipt = effectReceipt{Intent: effectKey(dispatch), Head: head, Target: target, Result: engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: dispatch.TaskID, Tree: dispatch.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC()}}
		if dispatch.Config.Primitive == workflow.VcsCommit {
			if intent.Commit == nil || head != intent.Commit.SHA {
				return zero, errors.New("prepared commit missing or changed")
			}
			receipt.Result.Output, _ = json.Marshal(map[string]any{"sha": intent.Commit.SHA, "files": intent.Commit.Snapshot.Files})
		} else {
			receipt.Result.Output, _ = json.Marshal(map[string]any{"landed": true, "sha": intent.TargetAfter})
		}
	}
	expectedHead := intent.Head
	if dispatch.Config.Primitive == workflow.VcsCommit {
		expectedHead = intent.TargetAfter
	}
	valid := receipt.Intent == effectKey(dispatch) && receipt.Result.DispatchID == dispatch.ID && receipt.Result.TaskID == dispatch.TaskID && receipt.Result.Tree == dispatch.Tree && receipt.Result.Session == dispatch.Sessions[0].ID && receipt.Result.ExecutionKind == "live" && receipt.Result.Class == "" && receipt.Result.NewTree == "" && receipt.Result.NewBase == "" && !receipt.Result.ProducedAt.Before(dispatch.CreatedAt) && !receipt.Result.ProducedAt.After(time.Now().UTC()) && receipt.Head == head && head == expectedHead && tree == dispatch.Tree
	schema, ok := doc.Artifacts[dispatch.Config.Output]
	if !ok || workflow.ValidateArtifact(schema.Schema, receipt.Result.Output) != nil {
		return zero, errors.New("recorded effect output schema invalid")
	}
	if intent.TargetRef != "" {
		valid = valid && receipt.Target == target && target == intent.TargetAfter
	}
	if !kernel.CanAdoptEffect(true, retired || recorded, valid, recorded || intent.TargetAfter != "") {
		return zero, errors.New("recorded effect postconditions changed or ambiguous")
	}
	if dispatch.Config.Primitive == workflow.VcsCommit {
		commitTree, e := effectGit(ctx, w, "rev-parse", intent.TargetAfter+"^{tree}")
		if e != nil || commitTree != dispatch.Tree {
			return zero, errors.New("recorded commit tree mismatch")
		}
		parent, e := effectGit(ctx, w, "rev-parse", intent.TargetAfter+"^")
		if e != nil || parent != intent.Head {
			return zero, errors.New("recorded commit parent mismatch")
		}
	}
	if dispatch.Config.Primitive == workflow.VcsCommit {
		if intent.Commit == nil {
			return zero, errors.New("prepared commit intent missing for index recovery")
		}
		if e = d.live.broker.ReconcileCommitIndex(ctx, w, *intent.Commit); e != nil {
			return zero, e
		}
	}
	if !recorded {
		raw, _ := json.Marshal(receipt)
		if e = d.store.RecordEffect(ctx, effectKey(dispatch)+"/result", dispatch.TaskID, "performed", raw); e != nil {
			return zero, e
		}
	}
	return receipt.Result, nil
}

// adoptCompletedEffect adopts, at recovery, an interrupted local effect whose
// completed daemon receipt survives (FAE-ROB-012/-016): nothing re-runs and no
// budget is charged. Leases are retired first (the receipt proves the effect's
// owned processes retired; their owner must be provably dead), then the receipt
// and every postcondition are remeasured. A receipt rebuilt from intent alone,
// an external effect or any mismatch keeps the conservative park.
func (d *Daemon) adoptCompletedEffect(ctx context.Context, e envelope) (engine.Job, bool, error) {
	j := e.Job
	dispatch := *j.InFlight
	local := supportedEffect(dispatch.Config.Primitive) && dispatch.Config.Primitive != workflow.ExternalEffect
	if !local || j.OperatorStage != "" || e.Admission.TargetConfig != "" {
		return j, false, nil
	}
	_, err := d.store.GetMarker(ctx, effectKey(dispatch)+"/result")
	if errors.Is(err, sql.ErrNoRows) {
		return j, false, nil
	}
	if err != nil {
		return j, false, err
	}
	if err = d.retireReceiptLeases(ctx, dispatch); err != nil {
		return j, false, err
	}
	w := d.live.manager.Binding(e.Admission.RunID, j.TaskID)
	if err = d.live.manager.Verify(ctx, w); err != nil {
		return j, false, err
	}
	result, err := d.recordedEffect(ctx, dispatch, w, true, j.Document)
	if !CanAutoAdoptEffect(true, true, err == nil, local) {
		return j, false, err
	}
	adopted, _, err := engine.AdoptInterruptedEffect(j, dispatch.ID, result)
	return adopted, err == nil, err
}

// retireReceiptLeases retires the lease proofs of a dispatch whose completed
// receipt or non-execution already proves its owned processes gone. Each
// proof's owner must be provably dead; a live owner may still hold the lease.
func (d *Daemon) retireReceiptLeases(ctx context.Context, dispatch engine.Dispatch) error {
	proofs, err := d.live.guard.journal.Entries()
	if err != nil {
		return err
	}
	found := false
	for _, proof := range proofs {
		for _, session := range dispatch.Sessions {
			if proof.Session != session.ID {
				continue
			}
			if !processowner.ProvedDead(proof.Owner.PID, proof.Birth) {
				return errors.New("effect lease owner death unproven")
			}
			proof.State = "retired"
			if err = d.live.guard.journal.Put(proof); err != nil {
				return err
			}
			found = true
		}
	}
	if !found {
		return nil
	}
	return d.live.guard.recoverProofs(ctx)
}

// effectRefusal is the record of an effect refused before anything ran,
// after its intent was recorded: the live executor's refusal before launch
// (FAE-RT-004), kept beside the intent so recovery reads the dispatch as a
// non-execution.
type effectRefusal struct {
	Intent string `json:"intent"`
	Detail string `json:"detail"`
}

// recordEffectRefusal records, against a recorded intent, that the effect
// was refused before its command, measurement or write started.
func (x *liveExecutor) recordEffectRefusal(ctx context.Context, i *effectIntent, cause error) error {
	return x.recordDispatchRefusal(ctx, i.Dispatch, cause)
}

func (x *liveExecutor) recordDispatchRefusal(ctx context.Context, d engine.Dispatch, cause error) error {
	raw, _ := json.Marshal(effectRefusal{Intent: effectKey(d), Detail: cause.Error()})
	return x.daemon.store.RecordEffect(ctx, effectKey(d)+"/refused-before-exec", d.TaskID, "observed", raw)
}

func (d *Daemon) hasRefusedBeforeExec(ctx context.Context, dispatch engine.Dispatch) bool {
	if _, e := d.store.GetMarker(ctx, effectKey(dispatch)+"/result"); e == nil {
		return false
	}
	raw, e := d.store.GetMarker(ctx, effectKey(dispatch)+"/refused-before-exec")
	if e != nil {
		return false
	}
	var refusal effectRefusal
	return decodeEffect(raw, &refusal) == nil && refusal.Intent == effectKey(dispatch) && d.effectNonExecution(ctx, dispatch)
}

// effectNonExecution reports positive evidence that a dispatch's effect never
// ran: its intent was recorded and the effect either never reached its
// launch boundary or was refused before anything started.
func (d *Daemon) effectNonExecution(ctx context.Context, dispatch engine.Dispatch) bool {
	raw, e := d.store.GetMarker(ctx, effectKey(dispatch)+"/intent")
	hasIntent := e == nil
	if hasIntent {
		var intent effectIntent
		if decodeEffect(raw, &intent) != nil {
			return false
		}
		expected, _ := json.Marshal(dispatch)
		recorded, _ := json.Marshal(intent.Dispatch)
		if string(expected) != string(recorded) {
			return false
		}
	} else if !errors.Is(e, sql.ErrNoRows) {
		return false
	}
	if raw, e = d.store.GetMarker(ctx, effectKey(dispatch)+"/refused-before-exec"); e == nil {
		var refusal effectRefusal
		if decodeEffect(raw, &refusal) == nil && refusal.Intent == effectKey(dispatch) {
			return true
		}
	}
	if !hasIntent {
		return false
	}
	_, e = d.store.GetMarker(ctx, effectKey(dispatch)+"/started")
	return errors.Is(e, sql.ErrNoRows)
}

// Effect identity includes artifact fields, so integers must retain their exact
// JSON representation through a crash/restart rather than round via float64.
func decodeEffect(raw []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}
