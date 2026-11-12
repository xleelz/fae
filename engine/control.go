package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

type ContextEntry struct {
	Kind   string `json:"kind"`
	Text   string `json:"text"`
	SHA256 string `json:"sha256"`
}
type Control struct {
	Method, Reason, Information, Context string
	Until                                time.Time
	Priority, AttemptLimit               int
	// RouteGeneration is the caller's current route generation (Options):
	// Resume accepts a park for want of a route only when the bridge
	// refused one of the stage's routes in this generation (FAE-RT-010).
	RouteGeneration string
}

func (j Job) AttemptLimit() int {
	if j.TaskAttemptLimit > 0 {
		return j.TaskAttemptLimit
	}
	return j.Document.Limits.Attempts
}
func (j Job) LaterContext() string {
	var b strings.Builder
	for _, x := range j.ContextEntries {
		b.WriteString("\n\n" + x.Kind + ":\n" + x.Text)
	}
	return b.String()
}
func ContextHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

// ApplyControl is the task-control state machine. A held in-flight dispatch is
// retained verbatim until its result is collected or explicitly reconciled.
func ApplyControl(input Job, c Control, now time.Time) (Job, []Event, error) {
	j := clone(input)
	events := []Event{}
	fail := func(s string) (Job, []Event, error) { return input, nil, errors.New(s) }
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	allows := c.Method == "Escalate" || c.Method == "Block" || c.Method == "Reprioritize" || c.Method == "SetAttemptLimit" || c.Method == "ResumeWithContext"
	if !CanChangeTask(j.State.Terminal(), j.InFlight != nil, allows) {
		return fail("task control illegal for terminal or in-flight task")
	}
	from := j.State
	pass := ""
	// Every state change below goes through enter, the kernel graph's single
	// legality check (FAE-WF-008); an illegal source is refused unchanged.
	switch c.Method {
	case "ResumeAtStage":
		if j.State != domain.Escalated || j.InFlight != nil || strings.TrimSpace(c.Reason) == "" {
			return fail("resume-at-stage requires Escalated, no unresolved dispatch, and reason")
		}
		switch j.EscalationKind {
		case domain.NoRedViolation, domain.ScopeWidenRejected, domain.CoordinatorVeto, domain.StaleBranchResidue, domain.MergeOwnHunkLoss, domain.ResumeEvidence, domain.ExternalEffectRefused:
			return fail("floor or authority escalation requires full restart")
		}
		stage := j.EscalatedStage
		if j.LastFan != nil && j.Document.Stages[stage].Primitive == workflow.Escalate && j.Document.Stages[j.LastFan.Join].Failure == stage {
			j.Fan = j.LastFan
			j.LastFan = nil
			stage = j.Fan.Stage
			if j.Fan.Baseline == nil {
				stage = j.Fan.Join
			}
		}
		if _, ok := j.Document.Stages[stage]; !ok {
			return fail("escalation stage is unavailable; use full restart")
		}
		for _, a := range j.Evidence {
			if a.Tree != j.Tree {
				for _, in := range j.Document.Stages[stage].Inputs {
					if in.Stage == a.Stage && in.Type == a.Type {
						return fail("resume-at-stage evidence tree changed; full revalidation required")
					}
				}
			}
		}
		j.resumeStageAuthorization = stage
		if err := enter(&j, domain.State(stage)); err != nil {
			return fail(err.Error())
		}
		for key := range j.Failures {
			if failureCounterAtStage(key, stage) {
				delete(j.Failures, key)
			}
		}
		delete(j.PriorRejections, stage)
		j.Parked = false
		j.Reason = ""
		j.EscalationKind = ""
		j.EscalationDetail = ""
		j.NewAttempt = true
		if j.Attempts >= j.AttemptLimit() {
			return fail("raise task attempt limit before resume-at-stage")
		}
		if j.Fan != nil {
			if err := resumeFanBranches(&j); err != nil {
				return fail(err.Error())
			}
		}

	case "Escalate", "Block":
		if strings.TrimSpace(c.Reason) == "" {
			return fail("reason required")
		}
		if c.Method == "Block" {
			stage, ok := j.Document.Stages[string(j.State)]
			if !ok {
				return fail("block requires stage state")
			}
			j.BlockedFrom = string(j.State)
			j.BlockedRoute = stage.Failure
			if err := enter(&j, domain.Blocked); err != nil {
				return fail(err.Error())
			}
		} else {
			// An Escalated task is retyped in place; Escalated has no self-edge.
			if j.State != domain.Escalated {
				if err := enter(&j, domain.Escalated); err != nil {
					return fail(err.Error())
				}
			}
			j.EscalationKind, j.EscalationDetail = domain.OperatorEscalate, c.Reason
		}
		j.Child = nil
		j.Reason = "Operator" + c.Method + ": " + c.Reason
		j.Parked = true
		if j.InFlight != nil {
			j.OperatorStage = j.InFlight.Stage
			j.OperatorReason = j.Reason
			if input.Reason == "UncertainDispatch" {
				j.Reason = "UncertainDispatch"
			}
		}
	case "Retire":
		if j.State != domain.Escalated {
			return fail("retire requires Escalated")
		}
		if _, _, err := ParseRetirement(c.Reason); err != nil {
			return fail(err.Error())
		}
		if err := enter(&j, domain.Retired); err != nil {
			return fail(err.Error())
		}
		j.Reason = strings.TrimSpace(c.Reason)
		j.Parked = false
	case "Cancel":
		if j.State != domain.Pending && j.State != domain.Blocked {
			return fail("cancel requires Pending or Blocked")
		}
		if strings.TrimSpace(c.Reason) == "" {
			return fail("reason required")
		}
		if err := enter(&j, domain.Cancelled); err != nil {
			return fail(err.Error())
		}
		j.Reason = c.Reason
		j.Parked = false
	case "Defer":
		if j.State != domain.Pending {
			return fail("defer requires Pending")
		}
		if !c.Until.IsZero() && !c.Until.After(now) {
			return fail("defer time must be in future")
		}
		j.Deferred = true
		j.DeferredUntil = c.Until
	case "Undefer":
		if j.State != domain.Pending || !j.Deferred {
			return fail("undefer requires deferred Pending")
		}
		j.Deferred = false
		j.DeferredUntil = time.Time{}
	case "Reprioritize":
		if c.Priority < 10 {
			return fail("priority must be at least 10")
		}
	case "SetAttemptLimit":
		if c.AttemptLimit < 1 {
			return fail("attempt limit must be positive")
		}
		j.TaskAttemptLimit = c.AttemptLimit
	case "RestartWithInformation":
		if j.State != domain.Pending && j.State != domain.Escalated {
			return fail("restart requires Pending or Escalated")
		}
		if strings.TrimSpace(c.Information) == "" {
			return fail("information required")
		}
		if j.State == domain.Escalated {
			if j.Escalated(domain.AttemptCap) && j.AttemptLimit() <= j.Attempts {
				return fail("raise attempt limit before restart")
			}
			if err := enter(&j, domain.State(j.Document.Start)); err != nil {
				return fail(err.Error())
			}
			j.NewAttempt = true
			if j.Fan != nil {
				event(&j, Event{Kind: "fan-abandoned", Stage: j.Fan.Stage, Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("visit %d left at %s", j.Fan.Visit, j.State)}, &events)
				j.Fan = nil
			}
			j.Parked = false
			j.Reason = ""
			j.EscalationKind, j.EscalationDetail = "", ""
			pass = carriedCounters(j)
		}
		// The operator may have fixed what the bridge refused: every
		// declared binding is eligible for selection again, and a later
		// park for want of a route starts a new route-exhaustion clock
		// (a ProviderUnavailable escalation keeps the old one).
		j.Child = nil
		j.RefusedRoutes = nil
		j.RoutesUnavailableSince = time.Time{}
		j.ContextEntries = append(j.ContextEntries, ContextEntry{"information", c.Information, ContextHash(c.Information)})
	case "Resume":
		if j.InFlight != nil {
			return fail("resume refuses unresolved dispatch")
		}
		switch j.State {
		case domain.Blocked:
			if _, ok := j.Document.Stages[j.BlockedRoute]; !ok {
				return fail("missing remediation route")
			}
			if err := enter(&j, domain.State(j.BlockedRoute)); err != nil {
				return fail(err.Error())
			}
		case domain.Escalated:
			if !j.Escalated(domain.AttemptCap) {
				return fail("non-attempt escalation requires restart with information")
			}
			if j.AttemptLimit() <= j.Attempts {
				return fail("raise attempt limit before resume")
			}
			// Escalated has exactly two out-edges (FAE-WF-021): the raised
			// limit re-grants attempts from the first stage, never from the
			// stage that exhausted them (FAE-OPS-010, FAE-CLI-041). It
			// re-grants attempts only: every other per-task budget carries
			// over, as on a restart with information (FAE-WF-074).
			if err := enter(&j, domain.State(j.Document.Start)); err != nil {
				return fail(err.Error())
			}
			j.NewAttempt = true
			pass = carriedCounters(j)
		default:
			// A task parked for want of a route, some of the stage's routes
			// refused by the bridge in the caller's current route
			// generation, resumes in place: the refusals are forgotten, so
			// its next turn tries every route again at this stage, and the
			// route-exhaustion bound starts again (FAE-RT-010). A park on
			// unhealthy routes alone is refused (health heals by decay), and
			// so is one whose only refusals come from an earlier generation:
			// they no longer apply, and the drive already tries those
			// routes again.
			if !j.Parked || !RoutesUnavailableReason(j.Reason) || !j.refusedAtStage(c.RouteGeneration) {
				return fail("resume requires Blocked, AttemptCap escalation, or a park on routes the bridge refused in the current route generation (since the daemon started); a park on unhealthy routes alone heals by decay")
			}
		}
		// Every resume starts a new route-exhaustion episode.
		j.Child = nil
		j.RoutesUnavailableSince = time.Time{}
		j.RefusedRoutes = nil
		j.Parked = false
		j.Reason = ""
		j.EscalationKind, j.EscalationDetail = "", ""
	case "ResumeWithContext":
		if strings.TrimSpace(c.Context) == "" {
			return fail("context required")
		}
		j.ContextEntries = append(j.ContextEntries, ContextEntry{"context", c.Context, ContextHash(c.Context)})
		// Resume attaches information without changing stage. Restart is the
		// separate explicit transition out of escalation.
	default:
		return fail("unknown task control")
	}
	if j.State != domain.Pending {
		j.Deferred = false
		j.DeferredUntil = time.Time{}
	}
	if from != j.State {
		kind := domain.EscalationKind("")
		if j.State == domain.Escalated {
			kind = j.EscalationKind
		}
		event(&j, Event{Kind: "transition", Stage: string(from), From: from, To: j.State, Attempt: j.stagedAttempt(), Cause: "Operator" + c.Method + ": " + c.Reason, EscalationKind: kind}, &events)
	}
	if c.Method == "Retire" {
		event(&j, Event{Kind: "retired", From: from, To: j.State, Cause: j.Reason, Attempt: j.stagedAttempt()}, &events)
	}
	reason := c.Reason
	if pass != "" && strings.TrimSpace(reason) != "" {
		reason += "; " + pass
	} else if pass != "" {
		reason = pass
	}
	event(&j, Event{Kind: "task-control", Stage: string(from), Attempt: j.stagedAttempt(), Cause: fmt.Sprintf("%s: %s", c.Method, reason)}, &events)
	// A control never produces a row the drive would refuse: its result
	// passes the same validation as every other checkpoint.
	if err := Validate(j); err != nil {
		return input, nil, fmt.Errorf("task control %s refused: result invalid: %w", c.Method, err)
	}
	return j, events, nil
}

func failureCounterAtStage(key, stage string) bool {
	for _, part := range strings.Split(key, ":") {
		if part == stage {
			return true
		}
	}
	return false
}

// Failed enclosing branches retain their execution frame: passed descendants,
// child visits and earlier stage evidence remain available on selective resume.
func resumeFanBranches(j *Job) error {
	if j.Fan == nil {
		return nil
	}
	j.Fan.IntegratedTree = ""
	for i := range j.Fan.Branches {
		b := &j.Fan.Branches[i]
		if b.Status == BranchPassed {
			continue
		}
		if b.Status != BranchFailed && b.Status != BranchSkipped && b.Status != BranchCancelled {
			continue
		}
		b.Status = BranchPending
		b.InFlight = nil
		b.Error = ""
		if b.Frame == nil {
			b.Stage = b.Start
			continue
		}
		frame := b.Frame
		stage := frame.EscalatedStage
		if frame.LastFan != nil && (frame.Document.Stages[frame.LastFan.Join].Failure == stage || frame.Document.Stages[frame.LastFan.Join].Failure == string(frame.State)) {
			frame.Fan = frame.LastFan
			frame.LastFan = nil
			stage = frame.Fan.Stage
			if frame.Fan.Baseline == nil {
				stage = frame.Fan.Join
			}
		}
		if frame.Fan != nil {
			if err := resumeFanBranches(frame); err != nil {
				return err
			}
			stage = frame.Fan.Stage
		}
		if stage == "" {
			stage = string(frame.State)
		}
		if frame.Document.Stages[stage].Primitive == workflow.Escalate {
			return fmt.Errorf("failed branch %s needs full restart", b.ID)
		}
		frame.resumeStageAuthorization = stage
		frame.fanCursorAuthorization = stage // audited branch recovery may re-enter a handler, not only its region start
		if err := enter(frame, domain.State(stage)); err != nil {
			return err
		}
		frame.InFlight = nil
		frame.Parked = false
		frame.Reason = ""
		frame.EscalationKind = ""
		frame.EscalationDetail = ""
		frame.NewAttempt = true
		frame.BranchAttemptPaid = false
		for key := range frame.Failures {
			if failureCounterAtStage(key, stage) {
				delete(frame.Failures, key)
			}
		}
		delete(frame.PriorRejections, stage)
		b.Status = BranchRunning
		b.Stage = stage
	}
	return nil
}

// carriedCounters records, for the audited control, the per-task budgets a
// restart from Escalated at the first stage carries over unrefunded. Resume
// of an AttemptCap escalation re-grants attempts against the raised limit and
// nothing else; restart with information re-grants nothing. Neither refunds
// the test runs, the governed-request counts, the Loop iteration counts or
// the loop-progress streaks (FAE-WF-074, Appendix A: test runs and
// scope-widen requests are per task), so raising the attempt limit never
// bypasses TestRunCap, LoopCap or ScopeWidenRejected. The agent-start,
// failover and stall-retry counters are not listed: each bounds one turn, not
// the task, and every end of that turn has already cleared them
// (clearTurnBounds), so a restart neither carries nor refunds them.
func carriedCounters(j Job) string {
	carried := struct {
		Attempts     int                     `json:"attempts"`
		AttemptLimit int                     `json:"attempt_limit"`
		TestRuns     int                     `json:"test_runs"`
		Loops        map[string]int          `json:"loops"`
		LoopProgress map[string]LoopProgress `json:"loop_progress,omitempty"`
		Requests     map[string]int          `json:"requests,omitempty"`
	}{j.Attempts, j.AttemptLimit(), j.TestRuns, map[string]int{}, j.LoopProgress, j.RequestCounts}
	for id, n := range j.Counts {
		if j.Document.Stages[id].Primitive == workflow.Loop {
			carried.Loops[id] = n
		}
	}
	b, _ := json.Marshal(carried)
	return "restart from " + j.Document.Start + " refunds nothing; carries counters " + string(b)
}

// Budget is one per-task budget: the count spent and its limit.
type Budget struct {
	Used  int `json:"used"`
	Limit int `json:"limit"`
}

// Budgets are the per-task budgets a restart from Escalated at the first
// stage carries (carriedCounters), each with its limit: attempts against the
// current limit, test runs, every Loop's iterations, every progress-aware
// Loop's no-progress streak, and the governed-request counts. The daemon
// states them in the reply to Resume and restart with information, so the
// operator sees a spent budget before the new pass escalates on it
// (FAE-OPS-010, FAE-WF-074).
type Budgets struct {
	Attempts    Budget            `json:"attempts"`
	TestRuns    Budget            `json:"test_runs"`
	Loops       map[string]Budget `json:"loops"`
	LoopStreaks map[string]Budget `json:"loop_streaks,omitempty"`
	Requests    map[string]Budget `json:"requests,omitempty"`
}

// CarriedBudgets reports the task's per-task budgets with their limits.
func CarriedBudgets(j Job) Budgets {
	b := Budgets{Attempts: Budget{j.Attempts, j.AttemptLimit()}, TestRuns: Budget{j.TestRuns, j.Document.Limits.TestRuns}, Loops: map[string]Budget{}}
	for id, s := range j.Document.Stages {
		if s.Primitive != workflow.Loop {
			continue
		}
		b.Loops[id] = Budget{j.Counts[id], s.MaxIterations}
		if s.Progress != nil {
			if b.LoopStreaks == nil {
				b.LoopStreaks = map[string]Budget{}
			}
			b.LoopStreaks[id] = Budget{j.LoopProgress[id].Streak, s.Progress.Streak}
		}
	}
	if p := j.Document.Requests; p != nil {
		b.Requests = map[string]Budget{workflow.RequestScopeWiden: {j.RequestCounts[workflow.RequestScopeWiden], requestCap(*p, j.Document.Limits, workflow.RequestScopeWiden)}}
		if p.Rework != "" {
			b.Requests[workflow.RequestRedRework] = Budget{j.RequestCounts[workflow.RequestRedRework], requestCap(*p, j.Document.Limits, workflow.RequestRedRework)}
		}
	}
	return b
}

// Complete preserves operator holds while collecting the original dispatch's
// evidence and effect outcome; it never launches a replacement turn.
func Complete(input Job, id string, r Result) (Job, []Event, error) {
	if input.Fan != nil && input.Fan.Baseline != nil && input.OperatorStage == "" {
		return completeNestedFan(input, "", id, r)
	}
	if input.OperatorStage == "" {
		out, _, err := complete(input, id, r)
		if err != nil {
			return input, nil, err
		}
		return out, append([]Event(nil), out.History[len(input.History):]...), nil
	}
	if err := Validate(input); err != nil {
		return input, nil, err
	}
	// The held turn is evaluated at its dispatch stage; the durable state is
	// restored below, so a held completion never changes state.
	original := clone(input)
	original.State = domain.State(input.OperatorStage)
	original.OperatorStage = ""
	original.OperatorReason = ""
	original.EscalationKind, original.EscalationDetail = "", ""
	var out Job
	var events []Event
	var err error
	if original.Fan != nil && original.Fan.Baseline != nil {
		out, events, err = completeNestedFan(original, "", id, r)
	} else {
		out, events, err = complete(original, id, r)
	}
	if err != nil {
		return input, nil, err
	}
	for n := range events {
		if events[n].Kind == "transition" || events[n].Kind == "fan-cursor" {
			events[n].Kind = "held-completion-transition"
			events[n].From = ""
			events[n].To = ""
		}
	}
	for n := len(input.History); n < len(out.History); n++ {
		if out.History[n].Kind == "transition" || out.History[n].Kind == "fan-cursor" {
			out.History[n].Kind = "held-completion-transition"
			out.History[n].From = ""
			out.History[n].To = ""
		}
	}
	out.State = input.State
	// A held turn is never retried: the task leaves its hold only for a new
	// turn, so collecting the held dispatch ends the turn and its per-turn
	// agent-start, failover and stall-retry bounds (FAE-RT-004, -005, -010).
	clearTurnBounds(&out)
	out.EscalationKind, out.EscalationDetail = input.EscalationKind, input.EscalationDetail
	out.Parked = true
	if out.InFlight != nil {
		out.OperatorStage = out.InFlight.Stage
		out.OperatorReason = input.OperatorReason
	} else {
		out.OperatorStage = ""
		out.OperatorReason = ""
	}
	if out.Reason != "UncertainDispatch" {
		out.Reason = input.OperatorReason
	}
	return out, events, nil
}

var notifyXMLEscaper = strings.NewReplacer(
	"&", "&amp;",
	"<", "&lt;",
	">", "&gt;",
	`"`, "&quot;",
	"'", "&#39;",
)

func formatNotificationsQuoted(notifications []TaskNotification) string {
	if len(notifications) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(`<notifications data_only="true">`)
	for _, n := range notifications {
		b.WriteString("\n  <notification")
		if n.ID != "" {
			fmt.Fprintf(&b, ` id="%s"`, notifyXMLEscaper.Replace(n.ID))
		}
		fmt.Fprintf(&b, ` task="%s"`, notifyXMLEscaper.Replace(n.TaskID))
		fmt.Fprintf(&b, ` kind="%s"`, notifyXMLEscaper.Replace(n.Kind))
		if n.FromTaskID != "" {
			fmt.Fprintf(&b, ` from_task="%s"`, notifyXMLEscaper.Replace(n.FromTaskID))
		}
		delivery := n.Delivery
		if delivery == "" {
			delivery = "next-turn"
		}
		fmt.Fprintf(&b, ` delivery="%s"`, notifyXMLEscaper.Replace(delivery))
		if n.RelayedBy != "" {
			fmt.Fprintf(&b, ` relayed_by="%s"`, notifyXMLEscaper.Replace(n.RelayedBy))
		}
		b.WriteString(">")
		b.WriteString(notifyXMLEscaper.Replace(n.Body))
		b.WriteString("</notification>")
	}
	b.WriteString("\n</notifications>")
	return b.String()
}

// ValidNotificationKind reports whether kind is one of the FAE-OPS-033 kinds.
func ValidNotificationKind(kind string) bool {
	return kind == "breaking-change" || kind == "cross-area-need" || kind == "note"
}

// ValidNotificationDelivery reports whether delivery is one of the FAE-OPS-033 deliveries.
func ValidNotificationDelivery(delivery string) bool {
	return delivery == "next-turn" || delivery == "in-flight"
}

// ApplyNotify validates and records an inbound TaskNotification on input (FAE-OPS-033).
func ApplyNotify(input Job, n TaskNotification, at time.Time) (Job, TaskNotification, []Event, error) {
	j := clone(input)
	if err := Validate(j); err != nil {
		return input, n, nil, err
	}
	if j.State.Terminal() {
		return input, n, nil, fmt.Errorf("cannot notify terminal task %s (%s)", j.TaskID, j.State)
	}
	if n.Delivery == "" {
		n.Delivery = "next-turn"
	}
	if !ValidNotificationKind(n.Kind) {
		return input, n, nil, fmt.Errorf("invalid notification kind %q: must be breaking-change, cross-area-need, or note", n.Kind)
	}
	if !ValidNotificationDelivery(n.Delivery) {
		return input, n, nil, fmt.Errorf("invalid notification delivery %q: must be next-turn or in-flight", n.Delivery)
	}
	n.Body = utf8StoredText(n.Body)
	if strings.TrimSpace(n.Body) == "" {
		return input, n, nil, errors.New("notification body required")
	}
	n.TaskID = j.TaskID
	if at.IsZero() {
		at = time.Now().UTC()
	}
	n.CreatedAt = at.UTC()
	if n.ID == "" {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s/%s/%d/%d/%s", n.TaskID, n.FromTaskID, n.Kind, n.Delivery, len(j.Notifications)+1, n.CreatedAt.UnixNano(), n.Body)))
		n.ID = "notif-" + hex.EncodeToString(sum[:8])
	}
	j.Notifications = append(j.Notifications, n)
	var events []Event
	fromClause := ""
	if n.FromTaskID != "" {
		fromClause = fmt.Sprintf(" from %s", n.FromTaskID)
	}
	if n.RelayedBy != "" {
		fromClause += fmt.Sprintf(" (relayed by %s)", n.RelayedBy)
	}
	cause := fmt.Sprintf("Notify %s [%s/%s]%s: %s", n.ID, n.Kind, n.Delivery, fromClause, bounded(n.Body))
	event(&j, Event{Kind: "task-notification", Stage: string(j.State), Attempt: j.stagedAttempt(), Cause: cause}, &events)
	if err := Validate(j); err != nil {
		return input, n, nil, err
	}
	return j, n, events, nil
}

// RecordOutboundNotification records an outbound notification event on a
// non-terminal source task's History (FAE-OPS-033).
func RecordOutboundNotification(input Job, n TaskNotification) (Job, []Event, error) {
	j := clone(input)
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	var events []Event
	relayed := ""
	if n.RelayedBy != "" {
		relayed = fmt.Sprintf(" (relayed by %s)", n.RelayedBy)
	}
	cause := fmt.Sprintf("Notify outbound %s to %s [%s/%s]%s: %s", n.ID, n.TaskID, n.Kind, n.Delivery, relayed, bounded(n.Body))
	event(&j, Event{Kind: "task-notification", Stage: string(j.State), Attempt: j.stagedAttempt(), Cause: cause}, &events)
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	return j, events, nil
}

// ResumeInFlightWithNotification checkpoints an interrupted in-flight turn so
// the stage immediately re-dispatches with queued in-flight notifications bound
// into the resumed prompt (FAE-OPS-033, FAE-RT-019).
func ResumeInFlightWithNotification(input Job, dispatchID string) (Job, []Event, error) {
	if input.Fan != nil && input.Fan.Baseline != nil {
		return branchDispatchControl(input, dispatchID, func(view Job) (Job, []Event, error) { return ResumeInFlightWithNotification(view, dispatchID) })
	}
	j := clone(input)
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	if j.InFlight == nil || j.InFlight.ID != dispatchID {
		return input, nil, errors.New("stale in-flight notification preemption")
	}
	d := *j.InFlight
	preemptedIDs := map[string]bool{}
	for _, n := range d.Notifications {
		if n.ID != "" {
			preemptedIDs[n.ID] = true
		}
	}
	for i := range j.Notifications {
		if (!d.CreatedAt.IsZero() && j.Notifications[i].DeliveredAt.Equal(d.CreatedAt)) || preemptedIDs[j.Notifications[i].ID] {
			j.Notifications[i].DeliveredAt = time.Time{}
		}
	}
	refund(&j, d)
	clearTurnBounds(&j)
	j.InFlight = nil
	j.Parked = false
	j.Reason = ""
	var events []Event
	event(&j, Event{Kind: "in-flight-notification-resume", Stage: d.Stage, DispatchID: d.ID, Attempt: j.stagedAttempt(), Cause: "checkpointed in-flight turn to resume with in-flight notification bound"}, &events)
	if err := Validate(j); err != nil {
		return input, nil, err
	}
	return j, events, nil
}
