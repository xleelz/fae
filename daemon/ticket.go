package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/external"
	"github.com/xleelz/fae/workflow"
)

// TicketAction names an installed tool and a runtime room action. Binding keys
// are vendor schema keys; values name daemon-owned task, room or ticket identity.
type TicketAction struct {
	Tool     string            `json:"tool"`
	Action   string            `json:"action"`
	Bindings map[string]string `json:"bindings,omitempty"`
}
type TicketWorkflowConfig struct {
	Actions        map[string]TicketAction `json:"actions"`
	RefreshSeconds int                     `json:"refresh_seconds"`
	Workflows      []string                `json:"workflows"`
}
type ticketIntent struct {
	Schema external.ToolSchema `json:"schema"`
	Tool   external.Tool       `json:"tool"`
	Intent external.Intent     `json:"intent"`
}
type ticketStatus struct {
	controlplane.TicketOutboxStats
	Error string `json:"error,omitempty"`
}

func validateTicketConfig(c LiveConfig) error {
	if c.TicketWorkflow == nil {
		return nil
	}
	cfg := c.TicketWorkflow
	if cfg.RefreshSeconds < 1 || cfg.RefreshSeconds > 300 {
		return errors.New("ticket room refresh_seconds must be 1..300")
	}
	if len(cfg.Workflows) == 0 {
		return errors.New("ticket workflows must be explicitly enabled")
	}
	if len(cfg.Actions) == 0 {
		return errors.New("ticket actions required")
	}
	if _, ok := cfg.Actions[string(domain.Pending)]; !ok {
		return errors.New("ticket Pending creation action required")
	}
	for state, a := range cfg.Actions {
		if state == "" || a.Action == "" {
			return errors.New("ticket state and action required")
		}
		if _, ok := c.ExternalTools[a.Tool]; !ok {
			return fmt.Errorf("ticket action %s requires installed tool %s", state, a.Tool)
		}
		bound := map[string]bool{}
		for key, source := range a.Bindings {
			bound[source] = true
			if key == "" || (source != "task" && source != "room" && source != "ticket" && source != "destination" && source != "push_namespace") {
				return fmt.Errorf("invalid ticket binding %s", key)
			}
			if state == string(domain.Pending) && source == "ticket" {
				return errors.New("ticket creation cannot require an existing ticket")
			}
		}
		for _, source := range []string{"room", "destination", "push_namespace"} {
			if !bound[source] {
				return fmt.Errorf("ticket action %s missing configured %s binding", state, source)
			}
		}
	}
	return nil
}

// ticketTransitions replays persisted history rather than sampling current
// state: fast transitions and daemon crashes cannot erase lifecycle emissions.
func ticketTransitions(j engine.Job, cfg TicketWorkflowConfig) []controlplane.TicketEmission {
	if !j.Disposition.AllowsExternalEffects() {
		return nil
	}
	states := []domain.State{domain.Pending}
	for _, e := range j.History {
		if e.To != "" && e.To != e.From {
			states = append(states, e.To)
		}
	}
	out := []controlplane.TicketEmission{}
	for n, state := range states {
		action, ok := cfg.Actions[string(state)]
		if !ok {
			continue
		}
		create := cfg.Actions[string(domain.Pending)]
		if n > 0 && action.Tool == create.Tool && action.Action == create.Action {
			continue
		}
		if state == domain.Done && j.ReviewMergeRequired && !j.ReviewMerged {
			continue
		}
		raw, _ := json.Marshal([]any{j.TaskID, j.Disposition.TicketRoom, n, string(state)})
		key, _ := domain.ContentHash(raw)
		out = append(out, controlplane.TicketEmission{Key: key, Task: j.TaskID, Room: j.Disposition.TicketRoom, Step: string(state), Position: int64(n)})
	}
	return out
}

// startTicketOutbox is called only by the serialized drive before shutdown joins
// workers. Vendor I/O never holds the writer mutex or stalls provider collection.
func (d *Daemon) startTicketOutbox() {
	if d.live == nil || d.ctx.Err() != nil {
		return
	}
	d.mu.Lock()
	if d.draining || !d.ticketActive.CompareAndSwap(false, true) {
		d.mu.Unlock()
		return
	}
	jobs := make([]engine.Job, 0, len(d.jobs))
	for _, snapshot := range d.jobs {
		var e envelope
		if decodeEnvelope(snapshot.Payload, &e) == nil && e.Job.Disposition.AllowsExternalEffects() {
			jobs = append(jobs, e.Job)
		}
	}
	d.mu.Unlock()
	d.workers.Add(1)
	go func() {
		defer d.workers.Done()
		defer d.statusChanged()
		defer d.ticketActive.Store(false)
		ctx, cancel := context.WithTimeout(d.ctx, 15*time.Second)
		defer cancel()
		status := ticketStatus{}
		jobs = ticketRoundOrder(jobs, d.ticketRound.Add(1)-1)
		for _, j := range jobs {
			if err := d.driveTaskTickets(ctx, j); err != nil {
				status.Error = err.Error()
			}
			if ctx.Err() != nil {
				break
			}
		}
		if stats, err := d.store.TicketStats(ctx); err == nil {
			status.TicketOutboxStats = stats
		} else {
			status.Error = err.Error()
			status.Uncertain = 1
		}
		d.ticketStatus.Store(status)
	}()
}
func (d *Daemon) driveTaskTickets(ctx context.Context, j engine.Job) error {
	if !j.Disposition.AllowsExternalEffects() {
		return nil
	}
	selected, err := d.liveForTask(ctx, j.TaskID)
	if err != nil {
		return err
	}
	if selected.config.TicketWorkflow == nil {
		return nil
	}
	cfg := *selected.config.TicketWorkflow
	enabled := false
	for _, id := range cfg.Workflows {
		if id == j.Document.ID {
			enabled = true
		}
	}
	if !enabled {
		return nil
	}
	if j.Disposition.TicketRoom == "" {
		return errors.New("ticket workflow missing per-task ticket room")
	}
	for _, action := range cfg.Actions {
		tool := selected.config.ExternalTools[action.Tool]
		granted := false
		for _, stage := range j.Document.Stages {
			if string(stage.Primitive) == "ExternalEffect" && stage.Subject == action.Tool && stage.Role == tool.Role {
				granted = true
			}
		}
		if !granted {
			return fmt.Errorf("ticket tool %s has no workflow role grant", action.Tool)
		}
	}
	// Immutable per-task configuration makes restart/config edits fail closed.
	tools := map[string]external.Tool{}
	for _, a := range cfg.Actions {
		tools[a.Tool] = selected.config.ExternalTools[a.Tool]
	}
	raw, _ := json.Marshal(struct {
		Config TicketWorkflowConfig
		Tools  map[string]external.Tool
	}{cfg, tools})
	recorded, readErr := d.store.GetMarker(ctx, "ticket-config/"+j.TaskID)
	if readErr == nil {
		oldHash, _ := domain.ContentHash(recorded)
		newHash, _ := domain.ContentHash(raw)
		if oldHash != newHash {
			return errors.New("immutable ticket configuration changed")
		}
	} else if !errors.Is(readErr, sql.ErrNoRows) {
		return readErr
	} else if err := d.store.RecordEffect(ctx, "ticket-config/"+j.TaskID, j.TaskID, "intent", raw); err != nil {
		return err
	}
	for _, row := range ticketTransitions(j, cfg) {
		if err := d.store.EnqueueTicket(ctx, row); err != nil {
			return err
		}
	}
	rows, err := d.store.TicketEmissions(ctx, j.TaskID, j.Disposition.TicketRoom)
	if err != nil {
		return err
	}
	ticketID := ""
	for _, row := range rows {
		if row.Emitted {
			if row.TicketID != "" {
				ticketID = row.TicketID
			}
			continue
		}
		return d.emitTicket(ctx, j, row, ticketID, cfg.Actions[row.Step])
	}
	return nil
}
func (d *Daemon) emitTicket(ctx context.Context, j engine.Job, row controlplane.TicketEmission, ticketID string, action TicketAction) (err error) {
	selected, err := d.liveForTask(ctx, j.TaskID)
	if err != nil {
		return err
	}
	discovery := d.ticketDiscovery
	if selected != d.live || discovery == nil {
		if selected.config.TicketWorkflow == nil {
			return errors.New("ticket adapter policy missing")
		}
		discovery = &external.Discovery{TTL: time.Duration(selected.config.TicketWorkflow.RefreshSeconds) * time.Second}
	}
	if err = d.checkTaskTarget(ctx, j.TaskID); err != nil {
		return err
	}
	prior := row
	row.Attempts++
	row.LastError = ""
	if err = d.store.SaveTicket(ctx, prior, row); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			next := row
			next.LastError = err.Error()
			if len(next.LastError) > 2048 {
				next.LastError = next.LastError[:2048]
			}
			err = errors.Join(err, d.store.SaveTicket(ctx, row, next))
		}
	}()
	var pinned ticketIntent
	if len(row.Intent) == 0 {
		base := selected.config.ExternalTools[action.Tool]
		bindings := map[string]string{}
		for key, source := range action.Bindings {
			switch source {
			case "task":
				bindings[key] = j.TaskID
			case "room":
				bindings[key] = row.Room
			case "destination":
				bindings[key] = j.Disposition.Destination
			case "push_namespace":
				bindings[key] = j.Disposition.PushNamespace
			case "ticket":
				if ticketID == "" {
					return errors.New("ticket transition requires prior emitted ticket identity")
				}
				bindings[key] = ticketID
			}
		}
		tool, e := discovery.ResolveRoomActionWithBindings(ctx, base, row.Room, action.Action, bindings)
		if e != nil {
			return e
		}
		if tool.Name != base.Name {
			return fmt.Errorf("room %s action %s differs from exact installed tool grant", row.Room, action.Action)
		}
		for key, want := range base.Arguments {
			if tool.Arguments[key] != want {
				return fmt.Errorf("room %s action overrides installed pin %s", row.Room, key)
			}
		}
		for key, value := range bindings {
			if value == "" || tool.Arguments[key] != value {
				return fmt.Errorf("room %s missing task disposition pin %s", row.Room, key)
			}
		}
		intent, e := external.NewIntent(tool, j.TaskID, "ticket-outbox/"+row.Step, row.Key, tool.Role, j.Tree, "outbox/"+row.Key, tool.Arguments)
		if e != nil {
			return e
		}
		// Stable task/outbox key independent of schema arguments or current revision.
		identity, _ := json.Marshal([]string{j.TaskID, row.Key, tool.Server, tool.Name})
		intent.Key, _ = domain.ContentHash(identity)
		pinned = ticketIntent{Schema: *tool.DiscoveredSchema, Tool: tool, Intent: intent}
		raw, _ := json.Marshal(pinned)
		next := row
		next.Intent = raw
		if e = d.store.SaveTicket(ctx, row, next); e != nil {
			return e
		}
		row = next
	} else if err = decodeEffect(row.Intent, &pinned); err != nil {
		return err
	}
	if !j.Disposition.AllowsExternalEffects() {
		return errors.New("ticket emission requires Normal live")
	}
	pinned.Tool.DiscoveredSchema = &pinned.Schema
	schema, e := discovery.Discover(ctx, pinned.Tool)
	if e != nil {
		return e
	}
	digest, e := domain.ContentHash(schema.InputSchema)
	if e != nil || digest != pinned.Intent.SchemaDigest {
		return errors.New("ticket tool schema changed from durable intent")
	}
	receipt, found, e := external.Query(ctx, pinned.Tool, pinned.Intent)
	if save := d.recordTicketObservation(ctx, j.TaskID, row.Key, receipt); save != nil {
		return save
	}
	if e != nil {
		return e
	}
	if !found {
		if !external.CanEmitTicket(row.Started, row.Emitted, true, j.Disposition.AllowsExternalEffects()) {
			return errors.New("ticket outcome uncertain; query absent, mutation replay refused")
		}
		next := row
		next.Started = true
		if e = d.store.SaveTicket(ctx, row, next); e != nil {
			return e
		}
		row = next
		receipt, e = external.Perform(ctx, pinned.Tool, pinned.Intent)
	}
	if save := d.recordTicketObservation(ctx, j.TaskID, row.Key, receipt); save != nil {
		return save
	}
	if e != nil {
		return e
	}
	if e = external.Match(pinned.Tool, pinned.Intent, receipt); e != nil {
		return e
	}
	if ticketID != "" && receipt.ExternalID != ticketID {
		return errors.New("ticket receipt changed existing ticket identity")
	}
	next := row
	next.Emitted = true
	next.TicketID = receipt.ExternalID
	next.LastError = ""
	if e = d.store.SaveTicket(ctx, row, next); e != nil {
		return e
	}
	row = next
	return nil
}

func (d *Daemon) recordTicketObservation(ctx context.Context, task, key string, receipt external.Receipt) error {
	if receipt.ExternalID == "" {
		return nil
	}
	raw, _ := json.Marshal(receipt)
	hash, _ := domain.ContentHash(raw)
	return d.store.RecordEffect(ctx, "ticket-observed/"+key+"/"+hash, task, "observed", raw)
}

// executeManagedTicket routes a workflow ticket stage through the same durable
// row as the lifecycle mirror; it never creates a second mutation path.
func (x *liveExecutor) executeManagedTicket(ctx context.Context, j engine.Job, dispatch engine.Dispatch) (any, bool, error) {
	cfg := x.config.TicketWorkflow
	if cfg == nil || !j.Disposition.AllowsExternalEffects() {
		return nil, false, nil
	}
	enabled := false
	for _, id := range cfg.Workflows {
		if id == j.Document.ID {
			enabled = true
		}
	}
	if !enabled {
		return nil, false, nil
	}
	managed := false
	for _, a := range cfg.Actions {
		if a.Tool == dispatch.Config.Subject {
			managed = true
		}
	}
	if !managed {
		return nil, false, nil
	}
	action, ok := cfg.Actions[dispatch.Stage]
	if !ok || action.Tool != dispatch.Config.Subject {
		return nil, true, errors.New("managed ticket stage lacks matching lifecycle action")
	}
	for !x.daemon.ticketActive.CompareAndSwap(false, true) {
		select {
		case <-ctx.Done():
			return nil, true, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer x.daemon.ticketActive.Store(false)
	defer x.daemon.refreshTicketStatus()
	targetStep := dispatch.Stage
	create := cfg.Actions[string(domain.Pending)]
	if action.Tool == create.Tool && action.Action == create.Action {
		targetStep = string(domain.Pending)
	}
	for n := 0; n <= len(j.History)+1; n++ {
		if err := x.daemon.driveTaskTickets(ctx, j); err != nil {
			return nil, true, err
		}
		rows, err := x.daemon.store.TicketEmissions(ctx, j.TaskID, j.Disposition.TicketRoom)
		if err != nil {
			return nil, true, err
		}
		for i := len(rows) - 1; i >= 0; i-- {
			r := rows[i]
			if r.Step != targetStep {
				continue
			}
			if r.Emitted {
				var pinned ticketIntent
				if err = decodeEffect(r.Intent, &pinned); err != nil {
					return nil, true, err
				}
				return externalOutput(pinned.Intent, external.Receipt{ExternalID: r.TicketID}, "reconciled"), true, nil
			}
			break
		}
	}
	return nil, true, errors.New("managed ticket stage has no durable lifecycle transition")
}

func managedTicket(c LiveConfig, j engine.Job, alias string) bool {
	if c.TicketWorkflow == nil || !j.Disposition.AllowsExternalEffects() {
		return false
	}
	for _, id := range c.TicketWorkflow.Workflows {
		if id == j.Document.ID {
			for _, a := range c.TicketWorkflow.Actions {
				if a.Tool == alias {
					return true
				}
			}
		}
	}
	return false
}

func validateTicketAdmission(c LiveConfig, doc workflow.Document, p AdmitParams) error {
	// Local tasks never mirror tickets and pin no Normal-only ticket stage.
	if c.TicketWorkflow == nil || p.Mode == "Local" {
		return nil
	}
	enabled := false
	for _, id := range c.TicketWorkflow.Workflows {
		if id == doc.ID {
			enabled = true
		}
	}
	if !enabled {
		return nil
	}
	if missing := missingLiveValues(p, true); p.CommitExternal && len(missing) > 0 {
		return missingLiveError(missing)
	}
	for _, action := range c.TicketWorkflow.Actions {
		tool := c.ExternalTools[action.Tool]
		granted := false
		for _, stage := range doc.Stages {
			if stage.Primitive == workflow.ExternalEffect && stage.Subject == action.Tool && stage.Role == tool.Role {
				granted = true
			}
		}
		if !granted {
			return fmt.Errorf("ticket tool %s has no workflow role grant", action.Tool)
		}
	}
	return nil
}

func (d *Daemon) refreshTicketStatus() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stats, err := d.store.TicketStats(ctx)
	status := ticketStatus{TicketOutboxStats: stats}
	if err != nil {
		status.Error = err.Error()
		status.Uncertain = 1
	}
	d.ticketStatus.Store(status)
}

// Each bounded round starts at another task, so one slow adapter cannot starve
// unrelated rooms even when its call consumes the entire round budget.
func ticketRoundOrder(jobs []engine.Job, round uint64) []engine.Job {
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].TaskID < jobs[j].TaskID })
	if len(jobs) == 0 {
		return jobs
	}
	start := int(round % uint64(len(jobs)))
	out := append([]engine.Job{}, jobs[start:]...)
	return append(out, jobs[:start]...)
}
