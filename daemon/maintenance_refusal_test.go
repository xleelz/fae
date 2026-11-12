package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
)

// beginMaintenance enters maintenance through the production controller RPC,
// which writes the plane marker and its controller-maintenance.json mirror.
// Drain is lifted so that only the marker holds the instance.
func beginMaintenance(t *testing.T, d *Daemon) *controller.Marker {
	t.Helper()
	if _, e := call(t, d, "Message", map[string]string{"id": "maintenance-grant", "text": `{"text":"Planned maintenance of this test instance.","authorize":{"classes":["controller"]}}`}, ""); e != nil {
		t.Fatal(e)
	}
	h, e := controllerCall(t, d, controller.Params{Action: "prepare", AuthorizingMessage: "maintenance-grant", Note: "planned", Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}})
	if e != nil || h.Maintenance == nil {
		t.Fatal("maintenance not entered", e)
	}
	d.mu.Lock()
	d.draining = false
	d.mu.Unlock()
	return h.Maintenance
}

// endMaintenance records the acceptance a relaunched daemon would and removes
// the mirror exactly as the accept action does; the in-process fixture cannot
// pass accept's new-process identity check.
func endMaintenance(t *testing.T, d *Daemon) {
	t.Helper()
	m, e := d.maintenanceMarker()
	if e != nil || m == nil {
		t.Fatal(m, e)
	}
	accepted := *m
	accepted.State, accepted.NextAction = "accepted", "none"
	body, _ := json.Marshal(&accepted)
	if e = d.store.ControllerAction(context.Background(), controlplane.Audit{Actor: "operator", Action: "controller-maintenance-accepted", MessageID: m.Message, Outcome: "ok"}, body, false); e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(d.controllerMarkerPath()); e != nil {
		t.Fatal(e)
	}
	d.mu.Lock()
	e = d.adoptMaintenance(body)
	d.draining = false
	d.mu.Unlock()
	if e != nil {
		t.Fatal(e)
	}
}

func rpcCode(e error) string {
	var typed *api.RPCError
	if errors.As(e, &typed) {
		return typed.Code
	}
	return ""
}

// FAE-API-011: while a maintenance marker is present every mutating RPC is
// refused with a typed error before authorization or any write, except the
// maintenance lifecycle itself; reads keep answering.
func TestMutationsRefusedTypedUnderMaintenanceMarker(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	marker := beginMaintenance(t, d)
	if _, e = os.Stat(d.controllerMarkerPath()); e != nil {
		t.Fatal("controller marker mirror missing", e)
	}
	var mutating []string
	for method, p := range actionPermissions {
		if !p.Read && method != "Controller" && method != "Schedule" {
			mutating = append(mutating, method)
		}
	}
	sort.Strings(mutating)
	for _, method := range mutating {
		if _, e = call(t, d, method, map[string]string{"authorizing_message": "message"}, ""); rpcCode(e) != "maintenance" {
			t.Errorf("%s under maintenance: %v", method, e)
		}
	}
	for _, action := range []string{"rearm", "stop-drive", "watchdog-revoke"} {
		if _, e = controllerCall(t, d, controller.Params{Action: action, AuthorizingMessage: "message"}); rpcCode(e) != "maintenance" {
			t.Errorf("Controller.%s under maintenance: %v", action, e)
		}
	}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "create", AuthorizingMessage: "message"}, ""); rpcCode(e) != "maintenance" {
		t.Errorf("Schedule.create under maintenance: %v", e)
	}
	if _, e = call(t, d, "Status", map[string]any{}, ""); e != nil {
		t.Fatal("read refused under maintenance", e)
	}
	if _, e = call(t, d, "Schedule", ScheduleParams{Action: "list"}, ""); e != nil {
		t.Fatal("schedule read refused under maintenance", e)
	}
	if h, e := controllerCall(t, d, controller.Params{Action: "health", AuthorizingMessage: "message"}); e != nil || h.Maintenance == nil || h.Maintenance.ID != marker.ID {
		t.Fatal("maintenance health", e)
	}
	if h, e := controllerCall(t, d, controller.Params{Action: "prepare", AuthorizingMessage: "maintenance-grant", Note: "retry", Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}}); e != nil || h.Maintenance == nil || h.Maintenance.ID != marker.ID {
		t.Fatal("maintenance stop retry refused", e)
	}
	if _, e = controllerCall(t, d, controller.Params{Action: "abort", AuthorizingMessage: "maintenance-grant", Note: "operator: inspect"}); e != nil {
		t.Fatal("maintenance abort refused", e)
	}
	endMaintenance(t, d)
	if _, e = call(t, d, "Message", map[string]string{"id": "after", "text": "after maintenance"}, ""); e != nil {
		t.Fatal("mutation refused after maintenance", e)
	}
	// A failed preflight leaves the daemon running and does not hold it.
	if _, e = controllerCall(t, d, controller.Params{Action: "preflight-failed", AuthorizingMessage: "message", Note: "gated proof unavailable"}); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "Message", map[string]string{"id": "after-preflight", "text": "still running"}, ""); e != nil {
		t.Fatal("failed preflight held the instance", e)
	}
	// The file mirror is not the authority: a stray or garbled one neither
	// holds nor releases the instance (TestMaintenanceGateFollowsThePlaneNotTheMirror
	// covers the unknown-commit case, which fails closed).
	if e = os.WriteFile(d.controllerMarkerPath(), []byte("{"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "Message", map[string]string{"id": "garbled-mirror", "text": "x"}, ""); e != nil {
		t.Fatal("a garbled mirror without a plane marker held the instance", e)
	}
	if e = os.Remove(d.controllerMarkerPath()); e != nil {
		t.Fatal(e)
	}
	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	x := &Daemon{ctx: stopped, layout: d.layout}
	if e = x.mutationRefusal("Admit", false); rpcCode(e) != "shutting-down" {
		t.Fatal("shutdown refusal untyped", e)
	}
	if e = x.mutationRefusal("Controller.accept", false); rpcCode(e) != "shutting-down" {
		t.Fatal("maintenance verb bypassed shutdown", e)
	}
}

// FAE-GEN-011, FAE-API-011: the plane is the maintenance authority. A
// maintenance marker that the plane committed holds the instance even when
// its file mirror could not be written, and a commit whose outcome is unknown
// fails closed until a later commit settles it.
func TestMaintenanceGateFollowsThePlaneNotTheMirror(t *testing.T) {
	cfg, ctx := fixture(t)
	d, e := Start(ctx, cfg)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { stop(t, d) })
	authorize(t, d)
	if _, e = call(t, d, "Message", map[string]string{"id": "maintenance-grant", "text": `{"text":"Planned maintenance of this test instance.","authorize":{"classes":["controller"]}}`}, ""); e != nil {
		t.Fatal(e)
	}
	fault := func(at string) {
		d.mu.Lock()
		d.testFault = func(point, id string) error {
			if point == at {
				return errors.New("injected " + point + " failure")
			}
			return nil
		}
		d.mu.Unlock()
	}
	prepare := controller.Params{Action: "prepare", AuthorizingMessage: "maintenance-grant", Note: "planned", Marker: &controller.Marker{Launcher: "/private/tmp/launcher.sock"}}
	fault("maintenance-mirror")
	if _, e = controllerCall(t, d, prepare); e == nil || !strings.Contains(e.Error(), "injected") {
		t.Fatal("prepare with a failed mirror write", e)
	}
	if _, e = os.Stat(d.controllerMarkerPath()); !os.IsNotExist(e) {
		t.Fatal("mirror written despite the injected failure", e)
	}
	if raw, e := d.store.GetMarker(ctx, "controller-maintenance"); e != nil || !strings.Contains(string(raw), `"draining"`) {
		t.Fatalf("plane marker %s %v", raw, e)
	}
	if _, e = call(t, d, "Message", map[string]string{"id": "during", "text": "mutation during maintenance"}, ""); rpcCode(e) != "maintenance" {
		t.Fatal("plane maintenance marker without a mirror admitted a mutation", e)
	}
	fault("")
	if _, e = controllerCall(t, d, controller.Params{Action: "abort", AuthorizingMessage: "maintenance-grant", Note: "operator: inspect"}); e != nil {
		t.Fatal("abort", e)
	}
	if m, e := controller.ReadMarker(d.controllerMarkerPath()); e != nil || m == nil || m.State != "failed" {
		t.Fatal("mirror not repaired by the next commit", m, e)
	}
	endMaintenance(t, d)
	if _, e = call(t, d, "Message", map[string]string{"id": "after", "text": "after maintenance"}, ""); e != nil {
		t.Fatal("mutation refused after maintenance", e)
	}
	// An unknown commit outcome while entering maintenance holds the instance;
	// the next committed marker settles it without a restart.
	fault("maintenance-commit")
	if _, e = controllerCall(t, d, prepare); e == nil || !strings.Contains(e.Error(), "injected") {
		t.Fatal("prepare with a failed commit", e)
	}
	if _, e = call(t, d, "Message", map[string]string{"id": "uncertain", "text": "x"}, ""); rpcCode(e) != "maintenance" {
		t.Fatal("unknown maintenance commit outcome admitted a mutation", e)
	}
	fault("")
	if _, e = controllerCall(t, d, controller.Params{Action: "preflight-failed", AuthorizingMessage: "message", Note: "roll abandoned"}); e != nil {
		t.Fatal(e)
	}
	if _, e = call(t, d, "Message", map[string]string{"id": "settled", "text": "x"}, ""); e != nil {
		t.Fatal("settled maintenance still held", e)
	}
}
