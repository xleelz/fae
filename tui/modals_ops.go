package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/xleelz/fae/tui/client"
)

func (m *Model) cachedTaskDetail(taskID string) (client.TaskDetail, bool) {
	if taskID == "" {
		return client.TaskDetail{}, false
	}
	if m.detail != nil && m.detail.id == taskID && m.detail.data != nil {
		return *m.detail.data, true
	}
	if m.snap.details != nil {
		if d, ok := m.snap.details[taskID]; ok {
			return d, true
		}
	}
	return client.TaskDetail{}, false
}

func (m *Model) defaultPoolID() string {
	pools := m.snap.pools
	if len(pools) == 0 {
		pools = m.snap.status.ProviderPools
	}
	if len(pools) == 0 {
		return ""
	}
	idx := selectable(m.pools.cursor, len(pools))
	return pools[idx].ID
}

func (m *Model) defaultQuotaTarget() (provider, model, poolID string) {
	pools := m.snap.pools
	if len(pools) == 0 {
		pools = m.snap.status.ProviderPools
	}
	if len(pools) > 0 {
		p := pools[selectable(m.pools.cursor, len(pools))]
		poolID = p.ID
		provider = p.Provider
	}
	health := m.snap.health
	if len(health) == 0 {
		health = m.snap.status.ProviderHealth
	}
	if len(health) > 0 {
		h := health[selectable(m.pools.cursor, len(health))]
		if provider == "" {
			provider = h.Provider
		}
		model = h.Model
	}
	return provider, model, poolID
}

// startNotify opens the operator notification modal (n or N).
func (m *Model) startNotify(taskID string, inFlight bool) tea.Cmd {
	if !m.writable("notify tasks") {
		return nil
	}
	if taskID == "" {
		taskID = m.currentTask()
	}
	delivery := "next-turn"
	if inFlight {
		delivery = "in-flight"
	}
	fields := []*field{
		newField("task", "Task", "target task id", taskID),
		newField("kind", "Kind", "note, breaking-change, or cross-area-need", "note"),
		newField("delivery", "Delivery", "next-turn or in-flight", delivery),
		newField("summary", "Summary", "notification message text (required)", ""),
	}
	notes := []string{
		"Send an operator notification to a task (FAE-OPS-033).",
		"Delivery 'next-turn' queues for the next turn; 'in-flight' preempts the active turn at a safe checkpoint.",
	}
	f, cmd := newForm("Notify task", notes, fields, func(m *Model, f *form) tea.Cmd {
		task := f.get("task")
		if task == "" {
			f.err = "the task id is required"
			return nil
		}
		kind := firstNonEmpty(strings.ToLower(f.get("kind")), "note")
		switch kind {
		case "note", "breaking-change", "cross-area-need":
		default:
			f.err = "kind must be note, breaking-change, or cross-area-need"
			return nil
		}
		deliv := firstNonEmpty(strings.ToLower(f.get("delivery")), delivery)
		if deliv != "next-turn" && deliv != "in-flight" {
			f.err = "delivery must be next-turn or in-flight"
			return nil
		}
		summary := f.get("summary")
		if summary == "" {
			f.err = "the summary message is required"
			return nil
		}
		p := client.NotifyParams{
			Task:     task,
			Kind:     kind,
			Delivery: deliv,
			Summary:  summary,
		}
		authSummary := fmt.Sprintf("fae-tui: notify task %s (%s, %s): %s", task, kind, deliv, summary)
		return m.runWrite(writeAction{
			verb:    "Notify",
			summary: authSummary,
			class:   "task",
			task:    task,
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support Notify")
				}
				p.AuthorizingMessage = message
				if err := ext.Notify(ctx, p); err != nil {
					return nil, err
				}
				return []string{fmt.Sprintf("notified %s (%s, %s)", task, kind, deliv)}, nil
			},
		})
	})
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startFollowUp opens the FollowUp task modal (F).
func (m *Model) startFollowUp(taskID string) tea.Cmd {
	if !m.writable("create follow-up tasks") {
		return nil
	}
	if taskID == "" {
		taskID = m.currentTask()
	}
	suggestedID := ""
	if taskID != "" {
		suggestedID = taskID + "-followup"
	}
	scopePrefill := ""
	if d, ok := m.cachedTaskDetail(taskID); ok && len(d.DeclaredFiles) > 0 {
		scopePrefill = strings.Join(d.DeclaredFiles, ", ")
	}
	fields := []*field{
		newField("task", "Predecessor", "completed or escalated predecessor task id", taskID),
		newField("id", "Follow-up ID", "new task id", suggestedID),
		newField("title", "Title", "optional short title", ""),
		newField("prompt", "Prompt", "follow-up instructions (required)", ""),
		newField("scope", "Scope", "comma-separated declared files", scopePrefill),
		newField("wall_seconds", "Wall seconds", "optional: turn wall clock budget in seconds", ""),
		newField("max_turns", "Max turns", "optional: max assistant turns", ""),
	}
	notes := []string{
		"Launch a FollowUp task (FAE-OPS-034) that inherits predecessor context and resumes its provider session when available.",
	}
	f, cmd := newForm("Create FollowUp task", notes, fields, func(m *Model, f *form) tea.Cmd {
		pred := f.get("task")
		if pred == "" {
			f.err = "the predecessor task id is required"
			return nil
		}
		id := f.get("id")
		if strings.ContainsAny(id, " \t\n") {
			f.err = "the follow-up id cannot contain spaces"
			return nil
		}
		prompt := f.get("prompt")
		if prompt == "" {
			f.err = "the prompt is required"
			return nil
		}
		var budget *client.TurnBudgetView
		if ws := f.get("wall_seconds"); ws != "" {
			n, err := strconv.Atoi(ws)
			if err != nil || n <= 0 {
				f.err = "wall_seconds must be a positive whole number"
				return nil
			}
			budget = &client.TurnBudgetView{WallSeconds: n}
		}
		if mt := f.get("max_turns"); mt != "" {
			n, err := strconv.Atoi(mt)
			if err != nil || n <= 0 {
				f.err = "max_turns must be a positive whole number"
				return nil
			}
			if budget == nil {
				budget = &client.TurnBudgetView{}
			}
			budget.MaxTurns = n
			budget.MaxAssistantTurns = n
		}
		p := client.FollowUpParams{
			Task:       pred,
			ID:         id,
			Title:      f.get("title"),
			Prompt:     prompt,
			Scope:      splitList(f.get("scope")),
			TurnBudget: budget,
		}
		authSummary := fmt.Sprintf("fae-tui: follow-up on %s", pred)
		if id != "" {
			authSummary += fmt.Sprintf(" as %s", id)
		}
		return m.runWrite(writeAction{
			verb:    "FollowUp",
			summary: authSummary + ".",
			class:   "task",
			task:    pred,
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support FollowUp")
				}
				p.AuthorizingMessage = message
				r, err := ext.FollowUp(ctx, p)
				if err != nil {
					return nil, err
				}
				lines := []string{fmt.Sprintf("created follow-up %s (from %s), state %s", firstNonEmpty(r.ID, id), firstNonEmpty(r.PredecessorTask, pred), firstNonEmpty(r.State, "Pending"))}
				if r.ResumedSession != "" {
					lines = append(lines, "resumed session: "+r.ResumedSession)
				}
				return lines, nil
			},
		})
	})
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startPoolResize opens the provider pool resize modal.
func (m *Model) startPoolResize(poolID string) tea.Cmd {
	if !m.writable("resize provider pools") {
		return nil
	}
	if poolID == "" {
		poolID = m.defaultPoolID()
	}
	capPrefill := ""
	pools := m.snap.pools
	if len(pools) == 0 {
		pools = m.snap.status.ProviderPools
	}
	for _, p := range pools {
		if p.ID == poolID && p.Capacity != 0 {
			capPrefill = strconv.Itoa(p.Capacity)
			break
		}
	}
	fields := []*field{
		newField("pool", "Pool", "provider pool id", poolID),
		newField("cap", "Capacity", "1..1000000 or -1 for unbounded", capPrefill),
		newField("reason", "Reason", "authorizing reason", "resize provider pool"),
	}
	notes := []string{"Resize a provider concurrency pool (FAE-SCH-015). Use -1 for unbounded capacity."}
	f, cmd := newForm("Resize provider pool", notes, fields, func(m *Model, f *form) tea.Cmd {
		pool := f.get("pool")
		if pool == "" {
			f.err = "the pool id is required"
			return nil
		}
		capVal, err := strconv.Atoi(f.get("cap"))
		if err != nil || (capVal != -1 && (capVal < 1 || capVal > 1000000)) {
			f.err = "capacity must be 1..1000000 or -1 for unbounded"
			return nil
		}
		reason := firstNonEmpty(f.get("reason"), "resize provider pool")
		p := client.PoolResizeParams{
			Pool: pool,
			Cap:  capVal,
		}
		return m.runWrite(writeAction{
			verb:    "PoolResize",
			summary: note(fmt.Sprintf("fae-tui: resize pool %s to %d.", pool, capVal), reason),
			class:   "runtime",
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support PoolResize")
				}
				p.AuthorizingMessage = message
				if err := ext.PoolResize(ctx, p); err != nil {
					return nil, err
				}
				return []string{fmt.Sprintf("resized pool %s to %d", pool, capVal)}, nil
			},
		})
	})
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startQuotaSet opens the quota exhaustion set modal.
func (m *Model) startQuotaSet(provider, model, poolID string) tea.Cmd {
	if !m.writable("set provider quota") {
		return nil
	}
	fields := []*field{
		newField("pool", "Pool", "optional: provider pool id", poolID),
		newField("provider", "Provider", "optional: provider name", provider),
		newField("model", "Model", "optional: model name", model),
		newField("reset_at", "Reset at", "RFC3339 timestamp or duration like 30m", "30m"),
		newField("grace_seconds", "Grace seconds", "grace period before cutoff", "60"),
		newField("reason", "Reason", "authorizing reason", "operator quota set"),
	}
	notes := []string{"Mark a provider pool or binding as quota-exhausted until Reset at (FAE-RT-018)."}
	f, cmd := newForm("Set provider quota", notes, fields, func(m *Model, f *form) tea.Cmd {
		pool := f.get("pool")
		prov := f.get("provider")
		mod := f.get("model")
		if pool == "" && prov == "" {
			f.err = "pool or provider is required"
			return nil
		}
		resetAt := f.get("reset_at")
		if resetAt == "" {
			f.err = "reset_at is required (RFC3339 timestamp or duration like 30m)"
			return nil
		}
		grace := 60
		if gs := f.get("grace_seconds"); gs != "" {
			n, err := strconv.Atoi(gs)
			if err != nil || n < 0 {
				f.err = "grace_seconds must be a non-negative whole number"
				return nil
			}
			grace = n
		}
		reason := firstNonEmpty(f.get("reason"), "operator quota set")
		p := client.QuotaSetParams{
			Provider:     prov,
			Model:        mod,
			Pool:         pool,
			ResetAt:      resetAt,
			GraceSeconds: grace,
			Reason:       reason,
		}
		targetDesc := firstNonEmpty(pool, prov)
		if mod != "" {
			targetDesc += "/" + mod
		}
		return m.runWrite(writeAction{
			verb:    "QuotaSet",
			summary: note(fmt.Sprintf("fae-tui: set quota on %s until %s.", targetDesc, resetAt), reason),
			class:   "runtime",
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support QuotaSet")
				}
				p.AuthorizingMessage = message
				if err := ext.QuotaSet(ctx, p); err != nil {
					return nil, err
				}
				return []string{fmt.Sprintf("quota set on %s (reset_at %s)", targetDesc, resetAt)}, nil
			},
		})
	})
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startQuotaClear opens the quota clear modal.
func (m *Model) startQuotaClear(provider, model, poolID string) tea.Cmd {
	if !m.writable("clear provider quota") {
		return nil
	}
	fields := []*field{
		newField("pool", "Pool", "optional: provider pool id", poolID),
		newField("provider", "Provider", "optional: provider name", provider),
		newField("model", "Model", "optional: model name", model),
		newField("reason", "Reason", "authorizing reason", "operator quota clear"),
	}
	notes := []string{"Clear an active quota exhaustion window on a pool or provider binding (FAE-RT-018)."}
	f, cmd := newForm("Clear provider quota", notes, fields, func(m *Model, f *form) tea.Cmd {
		pool := f.get("pool")
		prov := f.get("provider")
		mod := f.get("model")
		if pool == "" && prov == "" {
			f.err = "pool or provider is required"
			return nil
		}
		reason := firstNonEmpty(f.get("reason"), "operator quota clear")
		p := client.QuotaClearParams{
			Provider: prov,
			Model:    mod,
			Pool:     pool,
		}
		targetDesc := firstNonEmpty(pool, prov)
		if mod != "" {
			targetDesc += "/" + mod
		}
		return m.runWrite(writeAction{
			verb:    "QuotaClear",
			summary: note(fmt.Sprintf("fae-tui: clear quota on %s.", targetDesc), reason),
			class:   "runtime",
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support QuotaClear")
				}
				p.AuthorizingMessage = message
				if err := ext.QuotaClear(ctx, p); err != nil {
					return nil, err
				}
				return []string{fmt.Sprintf("quota cleared on %s", targetDesc)}, nil
			},
		})
	})
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startPause opens the scoped dispatch pause modal.
func (m *Model) startPause(scope, target string) tea.Cmd {
	if !m.writable("pause dispatch") {
		return nil
	}
	scope = firstNonEmpty(scope, "instance")
	fields := []*field{
		newField("scope", "Scope", "instance, workflow, task_set, or pool", scope),
		newField("target", "Target", "target id (leave empty for instance scope)", target),
		newField("reason", "Reason", "why dispatch is paused", "operator pause"),
	}
	notes := []string{"Pause dispatch for a specific scope (FAE-RT-018): instance, workflow, task_set, or pool."}
	f, cmd := newForm("Pause dispatch", notes, fields, func(m *Model, f *form) tea.Cmd {
		sc := strings.ToLower(f.get("scope"))
		switch sc {
		case "instance", "workflow", "task_set", "pool":
		default:
			f.err = "scope must be instance, workflow, task_set, or pool"
			return nil
		}
		tg := f.get("target")
		if sc != "instance" && tg == "" {
			f.err = "target is required for scope " + sc
			return nil
		}
		reason := firstNonEmpty(f.get("reason"), "operator pause")
		p := client.DispatchPauseParams{
			Scope:  sc,
			Target: tg,
			Reason: reason,
		}
		desc := sc
		if tg != "" {
			desc += ":" + tg
		}
		return m.runWrite(writeAction{
			verb:    "DispatchPause",
			summary: note(fmt.Sprintf("fae-tui: pause dispatch (%s).", desc), reason),
			class:   "runtime",
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support DispatchPause")
				}
				p.AuthorizingMessage = message
				if err := ext.DispatchPause(ctx, p); err != nil {
					return nil, err
				}
				return []string{fmt.Sprintf("paused dispatch for %s", desc)}, nil
			},
		})
	})
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startUnpause opens the scoped dispatch unpause modal.
func (m *Model) startUnpause(scope, target string) tea.Cmd {
	if !m.writable("unpause dispatch") {
		return nil
	}
	scope = firstNonEmpty(scope, "instance")
	fields := []*field{
		newField("scope", "Scope", "instance, workflow, task_set, or pool", scope),
		newField("target", "Target", "target id (leave empty for instance scope)", target),
		newField("reason", "Reason", "why dispatch is unpaused", "operator unpause"),
	}
	notes := []string{"Resume dispatch for a paused scope (FAE-RT-018): instance, workflow, task_set, or pool."}
	f, cmd := newForm("Unpause dispatch", notes, fields, func(m *Model, f *form) tea.Cmd {
		sc := strings.ToLower(f.get("scope"))
		switch sc {
		case "instance", "workflow", "task_set", "pool":
		default:
			f.err = "scope must be instance, workflow, task_set, or pool"
			return nil
		}
		tg := f.get("target")
		if sc != "instance" && tg == "" {
			f.err = "target is required for scope " + sc
			return nil
		}
		reason := firstNonEmpty(f.get("reason"), "operator unpause")
		p := client.DispatchPauseParams{
			Scope:  sc,
			Target: tg,
			Reason: reason,
		}
		desc := sc
		if tg != "" {
			desc += ":" + tg
		}
		return m.runWrite(writeAction{
			verb:    "DispatchUnpause",
			summary: note(fmt.Sprintf("fae-tui: unpause dispatch (%s).", desc), reason),
			class:   "runtime",
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support DispatchUnpause")
				}
				p.AuthorizingMessage = message
				if err := ext.DispatchUnpause(ctx, p); err != nil {
					return nil, err
				}
				return []string{fmt.Sprintf("unpaused dispatch for %s", desc)}, nil
			},
		})
	})
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startApproveWiden opens a confirmation/form modal showing the task's PendingRequest paths
// and approving the scope widen via Decide.
func (m *Model) startApproveWiden(taskID string) tea.Cmd {
	if !m.writable("approve scope widen") {
		return nil
	}
	if taskID == "" {
		taskID = m.currentTask()
	}
	notes := []string{"Approve a pending scope-widen or red-rework request on the task."}
	if d, ok := m.cachedTaskDetail(taskID); ok && d.PendingRequest != nil {
		req := d.PendingRequest
		if req.Kind != "" {
			notes = append(notes, "Request kind: "+req.Kind)
		}
		paths := append([]string(nil), req.Paths...)
		paths = append(paths, req.Files...)
		if len(paths) > 0 {
			notes = append(notes, "Requested paths: "+strings.Join(paths, ", "))
		}
		if req.TestFile != "" {
			notes = append(notes, "Test file: "+req.TestFile)
		}
		if req.Reason != "" {
			notes = append(notes, "Reason: "+req.Reason)
		}
	}
	fields := []*field{
		newField("task", "Task", "task id", taskID),
		newField("note", "Note", "approval rationale", "approve scope widen"),
	}
	f, cmd := newForm("Approve scope widen", notes, fields, func(m *Model, f *form) tea.Cmd {
		task := f.get("task")
		if task == "" {
			f.err = "the task id is required"
			return nil
		}
		rationale := firstNonEmpty(f.get("note"), "approve scope widen")
		p := client.DecideParams{
			Task:     task,
			Approved: true,
			Note:     rationale,
		}
		return m.runWrite(writeAction{
			verb:    "Decide",
			summary: note(fmt.Sprintf("fae-tui: approve scope widen on task %s.", task), rationale),
			class:   "task",
			task:    task,
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support Decide")
				}
				p.AuthorizingMessage = message
				if err := ext.Decide(ctx, p); err != nil {
					return nil, err
				}
				return []string{fmt.Sprintf("approved scope widen on %s", task)}, nil
			},
		})
	})
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startDecide opens the operator decision modal.
func (m *Model) startDecide(taskID string) tea.Cmd {
	if !m.writable("record decisions") {
		return nil
	}
	if taskID == "" {
		taskID = m.currentTask()
	}
	fields := []*field{
		newField("task", "Task", "task id", taskID),
		newField("approved", "Approved", "true or false", "true"),
		newField("note", "Note", "decision rationale", ""),
	}
	notes := []string{"Record an operator decision (approve or reject) for a task waiting on a decision."}
	f, cmd := newForm("Decide on task", notes, fields, func(m *Model, f *form) tea.Cmd {
		task := f.get("task")
		if task == "" {
			f.err = "the task id is required"
			return nil
		}
		appStr := strings.ToLower(f.get("approved"))
		var approved bool
		switch appStr {
		case "true", "yes", "y", "1":
			approved = true
		case "false", "no", "n", "0":
			approved = false
		default:
			f.err = "approved must be true or false"
			return nil
		}
		rationale := f.get("note")
		p := client.DecideParams{
			Task:     task,
			Approved: approved,
			Note:     rationale,
		}
		return m.runWrite(writeAction{
			verb:    "Decide",
			summary: note(fmt.Sprintf("fae-tui: decide on task %s (approved=%v).", task, approved), rationale),
			class:   "task",
			task:    task,
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support Decide")
				}
				p.AuthorizingMessage = message
				if err := ext.Decide(ctx, p); err != nil {
					return nil, err
				}
				return []string{fmt.Sprintf("recorded decision on %s (approved=%v)", task, approved)}, nil
			},
		})
	})
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startResolveUncertain opens the uncertain-dispatch resolution modal.
func (m *Model) startResolveUncertain(taskID string) tea.Cmd {
	if !m.writable("resolve uncertain dispatches") {
		return nil
	}
	if taskID == "" {
		taskID = m.currentTask()
	}
	fields := []*field{
		newField("task", "Task", "task id", taskID),
		newField("action", "Action", "retry, accept, or escalate", "retry"),
		newField("note", "Note", "resolution rationale", ""),
	}
	notes := []string{"Resolve an UncertainDispatch task after daemon restart: retry, accept, or escalate."}
	f, cmd := newForm("Resolve uncertain dispatch", notes, fields, func(m *Model, f *form) tea.Cmd {
		task := f.get("task")
		if task == "" {
			f.err = "the task id is required"
			return nil
		}
		action := strings.ToLower(f.get("action"))
		switch action {
		case "retry", "accept", "escalate":
		default:
			f.err = "action must be retry, accept, or escalate"
			return nil
		}
		rationale := f.get("note")
		p := client.ResolveUncertainParams{
			Task:   task,
			Action: action,
			Note:   rationale,
		}
		return m.runWrite(writeAction{
			verb:    "ResolveUncertain",
			summary: note(fmt.Sprintf("fae-tui: resolve uncertain dispatch on %s (%s).", task, action), rationale),
			class:   "task",
			task:    task,
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support ResolveUncertain")
				}
				p.AuthorizingMessage = message
				if err := ext.ResolveUncertain(ctx, p); err != nil {
					return nil, err
				}
				return []string{fmt.Sprintf("resolved uncertain dispatch on %s (%s)", task, action)}, nil
			},
		})
	})
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startLifecycle opens a generic lifecycle modal for ResumeWithContext, RestartWithInformation,
// QuarantineRelease, QuarantineRetire, Retire, Land, etc.
func (m *Model) startLifecycle(taskID, verb, defaultReason string) tea.Cmd {
	if !m.writable(strings.ToLower(verb)) {
		return nil
	}
	if taskID == "" {
		taskID = m.currentTask()
	}
	if defaultReason == "" {
		defaultReason = "operator " + strings.ToLower(verb)
	}
	defaultContext := ""
	title := verb + " task"
	notes := []string{fmt.Sprintf("Execute %s on the selected task.", verb)}
	if verb == "RestartWithInformation" || (verb == "ResumeWithContext" && m.isEscalatedTask(taskID)) {
		defaultContext = "Approved by operator in TUI; continue execution"
		title = "Un-escalate & Restart task (" + verb + ")"
		notes = []string{
			"Transition the escalated task back to the workflow start with operator instructions.",
			"Press Enter or Ctrl+S to submit immediately, or edit Information below first.",
		}
	} else if verb == "ResumeWithContext" {
		defaultContext = "Approved by operator in TUI; continue execution"
	}
	fields := []*field{
		newField("task", "Task", "task id", taskID),
		newField("reason", "Reason", "authorizing reason", defaultReason),
		newField("context", "Information", "operator instructions / context", defaultContext),
	}
	f, cmd := newForm(title, notes, fields, func(m *Model, f *form) tea.Cmd {
		task := f.get("task")
		if task == "" {
			f.err = "the task id is required"
			return nil
		}
		actualVerb := verb
		if actualVerb == "ResumeWithContext" && m.isEscalatedTask(task) {
			actualVerb = "RestartWithInformation"
		}
		reason := firstNonEmpty(f.get("reason"), defaultReason)
		ctxText := firstNonEmpty(f.get("context"), defaultContext, reason)
		p := client.LifecycleParams{
			Task:    task,
			Verb:    actualVerb,
			Context: ctxText,
			Reason:  reason,
		}
		authClass := "task"
		if actualVerb == "Land" {
			authClass = "controller"
		}
		return m.runWrite(writeAction{
			verb:    actualVerb,
			summary: note(fmt.Sprintf("fae-tui: %s task %s.", actualVerb, task), reason),
			class:   authClass,
			task:    task,
			run: func(ctx context.Context, d Daemon, message string) ([]string, error) {
				ext, ok := d.(ExtendedDaemon)
				if !ok {
					return nil, fmt.Errorf("this daemon client does not support %s", actualVerb)
				}
				p.AuthorizingMessage = message
				if err := ext.Lifecycle(ctx, p); err != nil {
					return nil, err
				}
				return []string{fmt.Sprintf("%s completed on %s", actualVerb, task)}, nil
			},
		})
	})
	if taskID != "" && len(f.fields) > 0 {
		f.fields[f.focus].blur()
		f.focus = len(f.fields) - 1
		cmd = f.fields[f.focus].focus()
	}
	f.resize(m.width)
	m.modal = f
	return cmd
}

// startSyncSymphony triggers an immediate SymphonyRefresh via ExtendedDaemon.
func (m *Model) startSyncSymphony() tea.Cmd {
	if !m.writable("sync symphony") {
		return nil
	}
	return m.runWrite(writeAction{
		verb:    "SymphonyRefresh",
		summary: "fae-tui: refresh Symphony tracker state.",
		class:   "runtime",
		run: func(ctx context.Context, d Daemon, _ string) ([]string, error) {
			ext, ok := d.(ExtendedDaemon)
			if !ok {
				return nil, fmt.Errorf("this daemon client does not support SymphonyRefresh")
			}
			if err := ext.SymphonyRefresh(ctx); err != nil {
				return nil, err
			}
			return []string{"Symphony tracker refresh triggered"}, nil
		},
	})
}
