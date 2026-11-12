package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/workflow"
)

// routeExhaustionFixture is a live instance whose Claude-only bridge
// (TestLiveBindingRefusalBridgeFixture) refuses both routes of the workflow's
// one role: a Codex binding it does not serve, and the Claude binding for
// want of credentials in the target environment. Every task it admits parks
// all-routes-unavailable.
func routeExhaustionFixture(t *testing.T, bound time.Duration) (Config, context.Context) {
	t.Helper()
	cfg, ctx, _ := liveDaemonFixture(t)
	scratch := filepath.Dir(cfg.Root)
	cli, log, noCredentials := filepath.Join(scratch, "claude"), filepath.Join(scratch, "claude.log"), filepath.Join(scratch, "no-credentials")
	if err := os.WriteFile(cli, []byte(refusalCLIScript(log)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(noCredentials, nil, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Live.Command = []string{exe, "-test.run=^TestLiveBindingRefusalBridgeFixture$", "--", "{config}"}
	cfg.Live.Environment = map[string]string{"LIVE_BINDING_BRIDGE": "1", "LIVE_BINDING_CLI": cli, "LIVE_BINDING_NO_CREDENTIALS": noCredentials, "CLAUDE_CODE_OAUTH_TOKEN": "fixture-token"}
	cfg.RouteExhaustion = bound
	return cfg, ctx
}

// routeExhaustionAdmit installs the fixture workflow and admits task id.
func routeExhaustionAdmit(t *testing.T, d *Daemon, id string) {
	t.Helper()
	accepted := workflow.Expr{Op: "eq", Args: []workflow.Expr{{Op: "field", Stage: "intake", Path: []string{"accepted"}}, {Op: "literal", Value: true}}}
	routes := []workflow.Binding{{Provider: "codex", Model: "fixture-codex", Effort: "medium"}, {Provider: "claude", Model: "fixture-model"}}
	doc := currentWorkflowFixture(workflow.Document{SchemaVersion: 1, ID: "route-exhaustion", Start: "intake", Limits: workflow.Limits{Attempts: 8, TestRuns: 2, ScopeWidens: 1},
		Roles:     map[string]workflow.Role{"intake": {Instructions: "Accept the item.", Briefs: map[string]string{"default": "Item {{task_id}}."}, Routes: routes, Scope: "task-bound", Cap: 1, Outputs: []string{"IntakeReport"}}},
		Artifacts: map[string]workflow.ArtifactType{"IntakeReport": {Version: 1, Schema: json.RawMessage(`{"type":"object","required":["accepted"],"additionalProperties":false,"properties":{"accepted":{"type":"boolean"}}}`)}},
		Stages:    map[string]workflow.Stage{"intake": {Primitive: workflow.AgentTurn, Role: "intake", Output: "IntakeReport"}, "intake-gate": {Primitive: workflow.Gate, Expr: &accepted, Failure: "stop"}, "stop": {Primitive: workflow.Escalate}},
		Edges:     []workflow.Edge{{From: "intake", To: "intake-gate"}, {From: "intake-gate", To: "Done"}, {From: "intake-gate", To: "stop"}}})
	if _, err := call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, d, "Admit", AdmitParams{ID: id, RunID: id, WorkflowID: doc.ID, AuthorizingMessage: "message"}, id); err != nil {
		t.Fatal(err)
	}
}

// routeExhaustionParked waits until task id is parked with refused counts
// route refusals in its history, and until the status snapshot the observer
// reads shows that checkpoint.
func routeExhaustionParked(t *testing.T, d *Daemon, id string, refused int) envelope {
	t.Helper()
	s, e := awaitJob(t, d, id, 2*time.Minute, func(s controlplane.JobSnapshot, e envelope) bool {
		n := 0
		for _, event := range e.Job.History {
			if event.Kind == "route-refused" {
				n++
			}
		}
		return e.Job.Parked && n == refused || e.Job.State == domain.Escalated
	})
	if !e.Job.Parked || !strings.HasPrefix(e.Job.Reason, "all-routes-unavailable: every route for role intake was refused") || e.Job.RoutesUnavailableSince.IsZero() {
		t.Fatalf("task %s did not park on refused routes: state=%s reason=%q since=%v", id, e.Job.State, e.Job.Reason, e.Job.RoutesUnavailableSince)
	}
	routeExhaustionPublished(t, d, s)
	return e
}

// routeExhaustionPublished waits until the status snapshot holds checkpoint s.
func routeExhaustionPublished(t *testing.T, d *Daemon, s controlplane.JobSnapshot) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for d.statusTasks()[s.ID].Version != s.Version {
		if time.Now().After(deadline) {
			t.Fatalf("status never showed %s at version %d: %+v", s.ID, s.Version, d.statusTasks()[s.ID])
		}
		d.signal()
		time.Sleep(20 * time.Millisecond)
	}
}

// routeExhaustionObserve runs the observer's route-exhaustion step at the
// given time.
func routeExhaustionObserve(t *testing.T, ctx context.Context, d *Daemon, at time.Time) {
	t.Helper()
	d.obs.round.Lock()
	defer d.obs.round.Unlock()
	if err := d.routeExhaustion(ctx, d.statusTasks(), at); err != nil {
		t.Fatal(err)
	}
}

// routeExhaustionDetail is a route-exhaustion alert's detail.
type routeExhaustionDetail struct {
	Task, Stage, Since, Reason string
	BoundSeconds               float64 `json:"bound_seconds"`
}

func routeExhaustionAlerts(t *testing.T, d *Daemon) []routeExhaustionDetail {
	t.Helper()
	var out []routeExhaustionDetail
	for _, a := range rpcResult[AlertsPage](t, d, "Alerts", AlertsParams{}).Alerts {
		if a.Source != "route-exhaustion" {
			continue
		}
		var detail routeExhaustionDetail
		if err := json.Unmarshal(a.Detail, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	return out
}

// FAE-OBS-005, FAE-RT-010, Appendix B ProviderUnavailable: the
// route-exhaustion alert follows the engine's clock. Resume starts a new
// episode: the alert raised after it counts from the re-park, not from the
// park the observer saw before (whose clock it no longer holds). A daemon
// restart retries the refused routes; refused again, the task parks on the
// clock it had, so the restart neither restarts the engine's bound nor
// raises the episode's alert a second time.
func TestDaemonRouteExhaustionAlertRestartsOnResume(t *testing.T) {
	cfg, ctx := routeExhaustionFixture(t, 0) // the default 30-minute bound
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	routeExhaustionAdmit(t, d, "exhausted")
	first := routeExhaustionParked(t, d, "exhausted", 2)
	since := first.Job.RoutesUnavailableSince
	routeExhaustionObserve(t, ctx, d, since.Add(29*time.Minute))
	if alerts := routeExhaustionAlerts(t, d); len(alerts) != 0 {
		t.Fatalf("alerted inside the bound: %+v", alerts)
	}
	// Resume forgets the refusals; both routes are refused again and the
	// task parks on a new clock.
	time.Sleep(1100 * time.Millisecond)
	if _, err = call(t, d, "ResumeRun", ResumeRunParams{Task: "exhausted", AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}
	resumed := routeExhaustionParked(t, d, "exhausted", 4)
	resumedSince := resumed.Job.RoutesUnavailableSince
	if !resumedSince.After(since.Add(time.Second)) {
		t.Fatalf("resume did not restart the engine's clock: %v then %v", since, resumedSince)
	}
	routeExhaustionObserve(t, ctx, d, resumedSince.Add(29*time.Minute))
	if alerts := routeExhaustionAlerts(t, d); len(alerts) != 0 {
		t.Fatalf("alerted inside the resumed episode's bound: %+v", alerts)
	}
	routeExhaustionObserve(t, ctx, d, resumedSince.Add(30*time.Minute))
	alerts := routeExhaustionAlerts(t, d)
	if len(alerts) != 1 || alerts[0].Since != resumedSince.Format(time.RFC3339Nano) || alerts[0].Task != "exhausted" || alerts[0].Stage != "intake" || alerts[0].BoundSeconds != 1800 || alerts[0].Reason != resumed.Job.Reason {
		t.Fatalf("alert after resume does not date the resumed episode (since %s): %+v", resumedSince.Format(time.RFC3339Nano), alerts)
	}

	// A restart is a new route generation: both routes are tried and
	// refused again, and the clock carries on.
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	restarted := routeExhaustionParked(t, d, "exhausted", 6)
	if !restarted.Job.RoutesUnavailableSince.Equal(resumedSince) {
		t.Fatalf("a restart restarted the route-exhaustion clock: %v, want %v", restarted.Job.RoutesUnavailableSince, resumedSince)
	}
	routeExhaustionObserve(t, ctx, d, resumedSince.Add(31*time.Minute))
	if alerts := routeExhaustionAlerts(t, d); len(alerts) != 1 {
		t.Fatalf("the episode alerted again after a restart: %+v", alerts)
	}
}

// FAE-OBS-005, FAE-RT-010, Appendix B ProviderUnavailable: the alert uses
// the engine's configured route-exhaustion bound, not a fixed 30 minutes,
// and alerts the episode once: raised while the task is still parked past
// the bound (here with the drive stopped), it is the same row after the
// engine escalates the task ProviderUnavailable, and after a daemon restart.
// The escalation keeps the episode's clock and names it.
func TestDaemonRouteExhaustionAlertUsesTheEngineBound(t *testing.T) {
	const bound = 8 * time.Second
	cfg, ctx := routeExhaustionFixture(t, bound)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			stop(t, d)
		}
	}()
	authorize(t, d)
	routeExhaustionAdmit(t, d, "exhausted")
	parked := routeExhaustionParked(t, d, "exhausted", 2)
	since := parked.Job.RoutesUnavailableSince
	// Hold the drive so the engine cannot escalate the task yet.
	if _, err = controllerCall(t, d, controller.Params{Action: "stop-drive", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	awaitDrive(t, d, false, 1)
	held, err := d.store.GetJob(ctx, "exhausted")
	if err != nil {
		t.Fatal(err)
	}
	var e envelope
	if err = json.Unmarshal(held.Payload, &e); err != nil {
		t.Fatal(err)
	}
	stillParked := e.Job.Parked
	if stillParked {
		// (The background observer runs on real time and may raise it
		// first; the episode is one alert either way.)
		routeExhaustionPublished(t, d, held)
		routeExhaustionObserve(t, ctx, d, since.Add(bound))
		if alerts := routeExhaustionAlerts(t, d); len(alerts) != 1 {
			t.Fatalf("no alert past the configured %v bound of a parked task: %+v", bound, alerts)
		}
	} else {
		t.Logf("the engine escalated the task before the drive stopped; only the escalated episode is observed")
	}
	// The drive decides the task again: past the bound it escalates.
	if _, err = controllerCall(t, d, controller.Params{Action: "rearm", AuthorizingMessage: "message"}); err != nil {
		t.Fatal(err)
	}
	s, final := awaitJob(t, d, "exhausted", 2*time.Minute, func(s controlplane.JobSnapshot, e envelope) bool {
		return e.Job.State == domain.Escalated
	})
	if !final.Job.Escalated(domain.ProviderUnavailable) || !final.Job.RoutesUnavailableSince.Equal(since) || !strings.Contains(final.Job.EscalationDetail, "no eligible route for role intake since "+since.Format(time.RFC3339)) {
		t.Fatalf("escalation: %s %q since %v, want ProviderUnavailable dated %v", final.Job.EscalationKind, final.Job.EscalationDetail, final.Job.RoutesUnavailableSince, since)
	}
	routeExhaustionPublished(t, d, s)
	routeExhaustionObserve(t, ctx, d, time.Now().UTC())
	alerts := routeExhaustionAlerts(t, d)
	if len(alerts) != 1 || alerts[0].Since != since.Format(time.RFC3339Nano) || alerts[0].Stage != "intake" || alerts[0].BoundSeconds != bound.Seconds() || alerts[0].Reason != parked.Job.Reason {
		t.Fatalf("escalated episode alerts (want one, dated %s, bound %v, the park's reason): %+v", since.Format(time.RFC3339Nano), bound, alerts)
	}
	stop(t, d)
	if d, err = Start(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := d.store.GetJob(ctx, "exhausted")
	if err != nil {
		t.Fatal(err)
	}
	routeExhaustionPublished(t, d, again)
	routeExhaustionObserve(t, ctx, d, time.Now().UTC())
	if alerts := routeExhaustionAlerts(t, d); len(alerts) != 1 {
		t.Fatalf("the escalated episode alerted again after a restart: %+v", alerts)
	}
}
