package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
	"github.com/xleelz/fae/worktree"
)

func externalIntent(c LiveConfig, disposition domain.Disposition, dispatch engine.Dispatch) (external.Tool, external.Intent, error) {
	if isFleetProvisionTool(c, dispatch.Config.Subject) {
		return external.Tool{}, external.Intent{}, errors.New("fleet provisioning is operator-only outside task execution")
	}
	t, ok := c.ExternalTools[dispatch.Config.Subject]
	if !ok {
		return t, external.Intent{}, errors.New("external stage requires installed subject tool")
	}
	if disposition.Mode != domain.Normal {
		return t, external.Intent{}, errors.New("external effect forbidden in Local mode")
	}
	if len(dispatch.Sessions) != 1 || dispatch.Sessions[0].Role != dispatch.Config.Role {
		return t, external.Intent{}, errors.New("external effect requires exact role session")
	}
	if disposition.Live {
		if e := validateExternalDestinations(t, dispatch.Config.Pins, disposition); e != nil {
			return t, external.Intent{}, e
		}
	}

	parts := strings.Split(dispatch.ID, "/")
	if len(parts) < 3 {
		return t, external.Intent{}, errors.New("invalid dispatch identity")
	}
	// Stable across retries, but isolated by workflow revision/hash and stage.
	i, e := external.NewIntent(t, dispatch.TaskID, parts[len(parts)-2]+"/"+dispatch.Stage, dispatch.Config.IdempotenceKey, dispatch.Config.Role, dispatch.Tree, dispatch.Sessions[0].ID, dispatch.Config.Pins)
	return t, i, e
}
func (x *liveExecutor) executeExternal(ctx context.Context, dispatch engine.Dispatch) (any, error) {
	x.daemon.mu.Lock()
	snapshot, ok := x.daemon.jobs[dispatch.TaskID]
	x.daemon.mu.Unlock()
	if !ok {
		return nil, errors.New("external task missing")
	}
	var env envelope
	if e := decodeEnvelope(snapshot.Payload, &env); e != nil {
		return nil, e
	}
	if err := x.daemon.checkPinnedTarget(ctx, env.Admission, env.Job.Document); err != nil {
		return nil, err
	}
	selected, err := x.daemon.liveForAdmission(ctx, env.Admission)
	if err != nil {
		return nil, err
	}
	copyX := *x
	copyX.config = selected.forTask(dispatch.TaskID).config
	x = &copyX
	if isFleetProvisionTool(x.config, dispatch.Config.Subject) {
		return nil, errors.New("fleet provisioning is operator-only outside task execution")
	}
	if t, exists := x.config.ExternalTools[dispatch.Config.Subject]; exists && reviewOperation(x.config.reviewOperation(dispatch.TaskID, dispatch.Config.Subject)) {
		if env.Job.Disposition.Mode != domain.Normal || len(dispatch.Sessions) != 1 || dispatch.Sessions[0].Role != t.Role || dispatch.Config.Role != t.Role {
			return nil, errors.New("review effect requires Normal mode and exact role grant")
		}
		a, _ := json.Marshal(t.Arguments)
		b, _ := json.Marshal(dispatch.Config.Pins)
		if string(a) != string(b) {
			return nil, errors.New("review arguments differ from installed pins")
		}
		if env.Job.Disposition.Live {
			if e := validateExternalDestinations(t, dispatch.Config.Pins, env.Job.Disposition); e != nil {
				return nil, e
			}
		}
		return x.executeReview(ctx, dispatch, env, t)
	}
	if output, managed, err := x.executeManagedTicket(ctx, env.Job, dispatch); managed {
		return output, err
	}
	t, i, e := externalIntent(x.config, env.Job.Disposition, dispatch)
	if e != nil {
		return nil, e
	}
	raw, _ := json.Marshal(i)
	if e = x.daemon.store.RecordEffect(ctx, effectKey(dispatch)+"/external-intent", dispatch.TaskID, "intent", raw); e != nil {
		return nil, e
	}
	if !env.Job.Disposition.Live {
		return externalOutput(i, external.Receipt{}, "dry-run"), nil
	}
	receipt, found, e := external.Query(ctx, t, i)
	if save := x.daemon.recordExternalObservation(ctx, dispatch, i, receipt); save != nil {
		return nil, save
	}
	if e != nil {
		return nil, e
	}
	if !found {
		receipt, e = external.Perform(ctx, t, i)
		if save := x.daemon.recordExternalObservation(ctx, dispatch, i, receipt); save != nil {
			return nil, save
		}
		if e != nil {
			return nil, e
		}
	}
	if e = external.Match(t, i, receipt); e != nil {
		return nil, e
	}
	return externalOutput(i, receipt, "reconciled"), nil
}
func externalOutput(i external.Intent, r external.Receipt, state string) map[string]any {
	return map[string]any{"tool": i.Tool, "argument_digest": i.Digest, "external_id": r.ExternalID, "reconciliation_state": state, "idempotency_key": i.Key}
}

// Persist the reported identity before validating it. Even a mismatched response
// remains discoverable as unreconciled evidence; operators cannot inject it.
func (d *Daemon) recordExternalObservation(ctx context.Context, dispatch engine.Dispatch, i external.Intent, r external.Receipt) error {
	if r.ExternalID == "" {
		return nil
	}
	raw, _ := json.Marshal(map[string]any{"intent": i, "reported": r, "reconciliation_state": "unreconciled"})
	hash, _ := domain.ContentHash(raw)
	return d.store.RecordEffect(ctx, effectKey(dispatch)+"/observed/"+hash, dispatch.TaskID, "observed", raw)
}
func (d *Daemon) reconcileExternal(ctx context.Context, dispatch engine.Dispatch, w worktree.Binding) (engine.Result, error) {
	var zero engine.Result
	snapshot, ok := d.jobs[dispatch.TaskID]
	if !ok {
		return zero, errors.New("external task missing")
	}
	var env envelope
	if e := decodeEnvelope(snapshot.Payload, &env); e != nil {
		return zero, e
	}
	if err := d.checkPinnedTarget(ctx, env.Admission, env.Job.Document); err != nil {
		return zero, err
	}
	selected, err := d.liveForAdmission(ctx, env.Admission)
	if err != nil {
		return zero, err
	}
	if isFleetProvisionTool(selected.config, dispatch.Config.Subject) {
		return zero, errors.New("fleet provisioning is operator-only outside task execution")
	}
	if managedTicket(selected.config, env.Job, dispatch.Config.Subject) {
		tree, err := d.live.broker.Tree(ctx, w)
		if err != nil {
			return zero, err
		}
		if tree != dispatch.Tree {
			return zero, errors.New("ticket recovery tree changed")
		}
		head, err := effectGit(ctx, w, "rev-parse", "HEAD")
		if err != nil {
			return zero, err
		}
		raw, err := d.store.GetMarker(ctx, effectKey(dispatch)+"/intent")
		if err != nil {
			return zero, err
		}
		var intent effectIntent
		if err = decodeEffect(raw, &intent); err != nil {
			return zero, err
		}
		if intent.Head != head || intent.Worktree != w {
			return zero, errors.New("ticket recovery worktree/head changed")
		}
		output, _, err := selected.executeManagedTicket(ctx, env.Job, dispatch)
		if err != nil {
			return zero, err
		}
		raw, _ = json.Marshal(output)
		return engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: dispatch.TaskID, Tree: dispatch.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: raw}, nil
	}
	if !env.Job.Disposition.AllowsExternalEffects() {
		return zero, errors.New("external recovery requires live Normal disposition")
	}
	t, i, e := externalIntent(selected.config, env.Job.Disposition, dispatch)
	managedReview := reviewOperation(selected.config.reviewOperation(dispatch.TaskID, dispatch.Config.Subject))
	if managedReview {
		t, i, e = selected.reviewRecoveryIntent(ctx, dispatch, t)
	}
	if e != nil {
		return zero, e
	}
	raw, e := d.store.GetMarker(ctx, effectKey(dispatch)+"/external-intent")
	if e != nil {
		return zero, e
	}
	expected, _ := json.Marshal(i)
	var recorded external.Intent
	if e = decodeEffect(raw, &recorded); e != nil {
		return zero, e
	}
	actual, _ := json.Marshal(recorded)
	if string(expected) != string(actual) {
		return zero, errors.New("external intent changed")
	}
	tree, e := d.live.broker.Tree(ctx, w)
	if e != nil {
		return zero, e
	}
	if tree != dispatch.Tree {
		return zero, errors.New("external recovery tree changed")
	}
	head, e := effectGit(ctx, w, "rev-parse", "HEAD")
	if e != nil {
		return zero, e
	}
	raw, e = d.store.GetMarker(ctx, effectKey(dispatch)+"/intent")
	if e != nil {
		return zero, e
	}
	var local effectIntent
	if e = decodeEffect(raw, &local); e != nil {
		return zero, e
	}
	if local.Head != head || local.Worktree != w {
		return zero, errors.New("external recovery head/worktree changed")
	}
	if managedReview && selected.config.reviewOperation(dispatch.TaskID, dispatch.Config.Subject) == "review_read" {
		value, err := selected.executeReview(ctx, dispatch, env, t)
		if err != nil {
			return zero, err
		}
		output, _ := json.Marshal(value)
		return engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: dispatch.TaskID, Tree: dispatch.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: output}, nil
	}
	receipt, found, e := external.Query(ctx, t, i)
	if save := d.recordExternalObservation(ctx, dispatch, i, receipt); save != nil {
		return zero, save
	}
	if e != nil {
		return zero, e
	}
	if !found {
		return zero, errors.New("external query absent; request may still complete, replay refused")
	}
	if e = external.Match(t, i, receipt); e != nil {
		return zero, e
	}
	var value any = externalOutput(i, receipt, "reconciled")
	if managedReview {
		s, err := d.reviewCheckpoint(ctx, dispatch.TaskID)
		if err != nil {
			return zero, err
		}
		value, e = selected.finishReview(ctx, dispatch, i, receipt, s)
		if e != nil {
			return zero, e
		}
	}
	output, _ := json.Marshal(value)
	r := engine.Result{ExecutionKind: "live", DispatchID: dispatch.ID, TaskID: dispatch.TaskID, Tree: dispatch.Tree, Session: dispatch.Sessions[0].ID, ProducedAt: time.Now().UTC(), Output: output}
	return r, nil
}

func validateExternalStages(c LiveConfig, doc workflow.Document, p AdmitParams) error {
	if err := validateTicketAdmission(c, doc, p); err != nil {
		return err
	}
	for name, s := range doc.Stages {
		if s.Primitive != workflow.ExternalEffect {
			continue
		}
		if isFleetProvisionTool(c, s.Subject) {
			return errors.New("fleet provisioning is operator-only outside task execution")
		}
		t, ok := c.ExternalTools[s.Subject]
		if !ok {
			return fmt.Errorf("stage %s external subject not installed", name)
		}
		probe := engine.Job{Document: doc, Disposition: domain.Disposition{Mode: domain.Normal, Live: true}}
		if managedTicket(c, probe, s.Subject) {
			a, _ := json.Marshal(t.Arguments)
			b, _ := json.Marshal(s.Pins)
			if s.Role != t.Role || string(a) != string(b) {
				return errors.New("ticket stage differs from exact installed role/static pins")
			}
			continue
		}
		validationTool := t
		if reviewOperation(c.reviewOperation(p.ID, s.Subject)) {
			validationTool.DiscoveredSchema = nil
		}
		_, e := external.NewIntent(validationTool, "admission", name, s.IdempotenceKey, s.Role, "tree", "session", s.Pins)
		if e != nil {
			return e
		}
		if p.CommitExternal {
			if e := validateExternalDestinations(t, s.Pins, domain.Disposition{TicketRoom: p.TicketRoom, Destination: p.Destination, PushNamespace: p.PushNamespace}); e != nil {
				return e
			}
		}

	}
	return nil
}

func validateExternalDestinations(t external.Tool, pins map[string]string, d domain.Disposition) error {
	required := map[string]string{"destination": d.Destination, "push_namespace": d.PushNamespace}
	_, hasTicketPin := pins["ticket_room"]
	if hasTicketPin || strings.HasPrefix(t.Name, "ticket_") {
		required["ticket_room"] = d.TicketRoom
	}
	for key, want := range required {
		if want == "" || pins[key] != want {
			return fmt.Errorf("external %s missing or differs from admission", key)
		}
	}
	return nil
}
