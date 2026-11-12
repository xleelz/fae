package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/daemon"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/scheduler"
)

// obsWarning is a configured-value warning in the FAE-OBS-001 form every verb
// writes: RFC 3339 UTC time, the instance, the component, then the command.
var obsWarning = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z instance=[A-Za-z0-9_.-]+ component=cli warning: fae [a-z -]+: --[a-z-]+ `)

// captureWarnings collects the configured-value warnings until the test ends.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var warnings bytes.Buffer
	old := configWarnings
	configWarnings = &warnings
	t.Cleanup(func() { configWarnings = old })
	return &warnings
}

// checkWarned fails unless warnings holds exactly one line, in the OBS-001
// form, containing want.
func checkWarned(t *testing.T, label string, warnings *bytes.Buffer, want string) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(warnings.String(), "\n"), "\n")
	if len(lines) != 1 || !obsWarning.MatchString(lines[0]) || !strings.Contains(lines[0], want) {
		t.Errorf("%s: warnings %q, want one FAE-OBS-001 line containing %q", label, warnings.String(), want)
	}
}

// FAE-HOST-007, FAE-CTL-051: no invalid --memory-floor-gib lowers gate run's
// landing floor. NaN, ±Inf, a value too large to convert to bytes, a
// nonpositive floor and one below the hard memory floor all fall back to
// 12 GiB with a warning, so with 8 GiB available the command never runs and
// the wait times out (75). A finite floor from the hard floor up is accepted
// as configured (Appendix A), so 4 GiB admits it. Before, NaN, Inf and 1e19
// passed the floor check, converted to a negative floor that applies only the
// soft floor, and ran the command.
func TestGateRunMemoryFloorNeverDropsBelowTheLandingFloor(t *testing.T) {
	warnings := captureWarnings(t)
	ledger, _ := gateTestHost(t, 1)
	gateObserve = func(context.Context) (hostguard.Sample, error) {
		r := func(v float64) hostguard.Reading { return hostguard.Reading{Value: v, Known: true} }
		return hostguard.Sample{Memory: r(8 * float64(hostguard.GiB)), Swap: r(float64(hostguard.GiB)), IO10: r(1)}, nil
	}
	gate := func(floor string) (int, string) {
		warnings.Reset()
		var out bytes.Buffer
		err := execute(context.Background(), []string{"gate", "run", "--host-ledger", ledger, "--max-wait", "3m", "--slice", "1m", "--memory-floor-gib=" + floor, "--", "sh", "-c", "echo ran"}, &out)
		if err == nil {
			return 0, out.String()
		}
		return exitCode(err), out.String()
	}
	for _, floor := range []string{"NaN", "Inf", "+Inf", "-Inf", "1e19", "1e300", "2e6", "-5", "0", "1e-300", "1", "1.5", ""} {
		code, out := gate(floor)
		if code != daemon.GateRunTimeoutExit || strings.Contains(out, "ran") {
			t.Errorf("--memory-floor-gib %q: exit %d, output %q; want the 12 GiB floor to hold the command (75)", floor, code, out)
		}
		want := "--memory-floor-gib is empty"
		if v, err := strconv.ParseFloat(floor, 64); err == nil {
			want = "--memory-floor-gib " + strconv.FormatFloat(v, 'g', -1, 64) + " is out of range ("
		}
		checkWarned(t, "--memory-floor-gib "+floor, warnings, "fae gate run: "+want)
		if !strings.Contains(warnings.String(), "; using the default 12\n") {
			t.Errorf("--memory-floor-gib %q: warning %q does not name the 12 GiB default", floor, warnings.String())
		}
	}
	if code, out := gate("4"); code != 0 || !strings.Contains(out, "ran") || warnings.Len() != 0 {
		t.Errorf("--memory-floor-gib 4 with 8 GiB available: exit %d, output %q, warnings %q", code, out, warnings.String())
	}
}

// REQUIREMENTS.md conventions on every verb, not only serve and the
// controller verbs: a flag carrying a configurable default falls back to it
// when empty, out of range or not finite, and says so in one FAE-OBS-001
// warning line naming the command, the flag, the value and the default. A
// value that does not parse stays a usage error (2).
func TestConfiguredValuesFallBackOnEveryVerb(t *testing.T) {
	warnings := captureWarnings(t)

	// gate run: its wait and capacity; the command still runs.
	ledger, _ := gateTestHost(t, 1)
	for _, tc := range [][2]string{{"--max-wait=0s", "--max-wait 0s is out of range (a positive duration); using the default 30m0s"}, {"--max-wait=-1m", "--max-wait -1m0s is out of range"}, {"--max-wait=", "--max-wait is empty; using the default 30m0s"}, {"--capacity=0", "--capacity 0 is out of range (1..1000000 weight units); using the default 2"}, {"--capacity=-3", "--capacity -3 is out of range"}, {"--capacity=2000000", "--capacity 2000000 is out of range"}, {"--capacity=", "--capacity is empty; using the default 2"}} {
		warnings.Reset()
		var out bytes.Buffer
		if err := execute(context.Background(), []string{"gate", "run", "--host-ledger", ledger, tc[0], "--", "sh", "-c", "echo ran"}, &out); err != nil || !strings.Contains(out.String(), "ran") {
			t.Errorf("gate run %s: %v %q", tc[0], err, out.String())
		}
		checkWarned(t, "gate run "+tc[0], warnings, "fae gate run: "+tc[1])
	}

	// heavy-op run: its maximum lease wait.
	heavy := filepath.Join(t.TempDir(), "heavy")
	if _, err := scheduler.OpenLedger(heavy, 2); err != nil {
		t.Fatal(err)
	}
	heavyOpObserve = func(context.Context) (hostguard.Sample, error) {
		r := func(v float64) hostguard.Reading { return hostguard.Reading{Value: v, Known: true} }
		return hostguard.Sample{Memory: r(64 * float64(hostguard.GiB)), Swap: r(1), IO10: r(0), IO60: r(0), Memory10: r(0), Memory60: r(0), LoadPerCore: r(0.1)}, nil
	}
	heavyOpPolicy = hostguard.Policy{Window: 30 * time.Millisecond, MaxGap: time.Second, Interval: 3 * time.Millisecond, Checks: 10, IOGate: 5}
	t.Cleanup(func() { heavyOpObserve, heavyOpPolicy = nil, hostguard.Policy{} })
	for _, tc := range [][2]string{{"--max-wait=0s", "--max-wait 0s is out of range (a positive duration); using the default 30m0s"}, {"--max-wait=-5s", "--max-wait -5s is out of range"}, {"--max-wait=", "--max-wait is empty; using the default 30m0s"}} {
		warnings.Reset()
		if err := execute(context.Background(), []string{"heavy-op", "run", "--ledger", heavy, "--weight", "1", "--class", "operator", tc[0], "--", "true"}, &bytes.Buffer{}); err != nil {
			t.Errorf("heavy-op run %s: %v", tc[0], err)
		}
		checkWarned(t, "heavy-op run "+tc[0], warnings, "fae heavy-op run: "+tc[1])
	}

	// census: its pacing between retirements.
	home, requests := recordingDaemon(t, "conf", map[string]any{"ok": true})
	for _, tc := range [][2]string{{"--gap=-1s", "--gap -1s is out of range (0s..1m0s); using the default 2s"}, {"--gap=2m", "--gap 2m0s is out of range"}, {"--gap=", "--gap is empty; using the default 2s"}} {
		warnings.Reset()
		if err := execute(context.Background(), []string{"census", "--home", home, "--instance", "conf", tc[0]}, &bytes.Buffer{}); err != nil {
			t.Errorf("census %s: %v", tc[0], err)
		} else if r := <-requests; r.Method != "Census" {
			t.Errorf("census %s called %s", tc[0], r.Method)
		}
		checkWarned(t, "census "+tc[0], warnings, "instance=conf component=cli warning: fae census: "+tc[1])
	}

	// schedule create: a loop's safety expiry reaches the daemon as 7 days.
	for _, tc := range [][2]string{{"--expires-in=0s", "--expires-in 0s is out of range (a whole number of seconds from 1s to 8760h0m0s); using the default 168h0m0s"}, {"--expires-in=-1h", "--expires-in -1h0m0s is out of range"}, {"--expires-in=1500ms", "--expires-in 1.5s is out of range"}, {"--expires-in=9000h", "--expires-in 9000h0m0s is out of range"}, {"--expires-in=", "--expires-in is empty; using the default 168h0m0s"}} {
		warnings.Reset()
		if err := execute(context.Background(), []string{"schedule", "create", "--id", "l", "--role", "oracle", "--prompt", "p", tc[0], "--home", home, "--instance", "conf", "--authorizing-message", "m"}, &bytes.Buffer{}); err != nil {
			t.Errorf("schedule create %s: %v", tc[0], err)
			continue
		}
		var p daemon.ScheduleParams
		if err := json.Unmarshal((<-requests).Params, &p); err != nil || p.ExpiresInSeconds != 7*24*3600 {
			t.Errorf("schedule create %s: expiry %d seconds (%v), want the 7-day default", tc[0], p.ExpiresInSeconds, err)
		}
		checkWarned(t, "schedule create "+tc[0], warnings, "fae schedule create: "+tc[1])
	}

	// land --then-roll: the roll wait and overlap hold reach the roll as
	// their defaults; a negative hold made Roll's timeout negative.
	for _, tc := range [][2]string{{"--roll-max-wait=-1s", "--roll-max-wait -1s is out of range (a positive duration); using the default 5m0s"}, {"--roll-max-wait=", "--roll-max-wait is empty; using the default 5m0s"}, {"--overlap-wait=-2h", "--overlap-wait -2h0m0s is out of range (a positive duration); using the default 2h0m0s"}, {"--overlap-wait=0s", "--overlap-wait 0s is out of range"}} {
		warnings.Reset()
		f := &fakeLand{results: []controller.LandResult{{Task: "t", Outcome: controller.LandLanded, Landed: "abc"}}}
		f.install(t)
		if err := execute(context.Background(), landArgs("--task", "t", "--then-roll", "--launcher-socket", "/private/launcher.sock", tc[0]), &bytes.Buffer{}); err != nil {
			t.Errorf("land %s: %v", tc[0], err)
		}
		if len(f.rolled) != 1 || f.rolled[0].MaxWait != 300*time.Second || f.rolled[0].OverlapWait != 2*time.Hour {
			t.Errorf("land %s rolled with %+v, want the 300 s wait and 2 h hold", tc[0], f.rolled)
		}
		checkWarned(t, "land "+tc[0], warnings, "fae land: "+tc[1])
	}

	// A value that does not parse stays a usage error, with no warning.
	warnings.Reset()
	for _, args := range [][]string{
		{"gate", "run", "--host-ledger", ledger, "--memory-floor-gib", "x", "--", "true"},
		{"gate", "run", "--host-ledger", ledger, "--max-wait", "soon", "--", "true"},
		{"gate", "run", "--host-ledger", ledger, "--capacity", "1.5", "--", "true"},
		{"heavy-op", "run", "--ledger", heavy, "--max-wait", "soon", "--", "true"},
		{"census", "--home", home, "--instance", "conf", "--gap", "soon"},
		{"schedule", "create", "--id", "l", "--role", "oracle", "--prompt", "p", "--expires-in", "7d", "--home", home, "--instance", "conf", "--authorizing-message", "m"},
		{"land", "--task", "t", "--target", "main", "--then-roll", "--launcher-socket", "/l.sock", "--roll-max-wait", "soon", "--authorizing-message", "m"},
	} {
		if err := execute(context.Background(), args, &bytes.Buffer{}); exitCode(err) != 2 {
			t.Errorf("%v: exit %d (%v), want usage 2", args, exitCode(err), err)
		}
	}
	if warnings.Len() != 0 {
		t.Errorf("malformed values warned %q", warnings.String())
	}
}

// FAE-API-001 and REQUIREMENTS.md conventions: provider-health's window has
// a spec default (15 min), so an empty or out-of-range window reaches the
// daemon as 900 seconds, with one FAE-OBS-001 warning naming the value and
// the default. An in-range window is sent as given, and one that does not
// parse stays a usage error (2). Before, every out-of-range or empty window
// exited 2 and the daemon was never asked.
func TestProviderHealthWindowFallsBackToItsDefault(t *testing.T) {
	warnings := captureWarnings(t)
	home, requests := recordingDaemon(t, "health", map[string]any{"ok": true})
	window := func(args ...string) (float64, error) {
		warnings.Reset()
		err := execute(context.Background(), append([]string{"provider-health", "--home", home, "--instance", "health"}, args...), &bytes.Buffer{})
		if err != nil {
			return 0, err
		}
		r := <-requests
		if r.Method != "ProviderHealth" {
			t.Errorf("provider-health %v called %s", args, r.Method)
		}
		seconds, _ := params(t, r)["window_seconds"].(float64)
		return seconds, nil
	}
	for _, tc := range [][2]string{
		{"--window=0s", "--window 0s is out of range (1s..24h0m0s); using the default 15m0s"},
		{"--window=-1m", "--window -1m0s is out of range (1s..24h0m0s); using the default 15m0s"},
		{"--window=500ms", "--window 500ms is out of range"},
		{"--window=25h", "--window 25h0m0s is out of range"},
		{"--window=", "--window is empty; using the default 15m0s"},
	} {
		if seconds, err := window(tc[0]); err != nil || seconds != 900 {
			t.Errorf("provider-health %s: window %v seconds (%v), want the 900-second default", tc[0], seconds, err)
		}
		checkWarned(t, "provider-health "+tc[0], warnings, "instance=health component=cli warning: fae provider-health: "+tc[1])
	}
	for arg, want := range map[string]float64{"--window=1h": 3600, "--window=1s": 1, "--window=24h": 86400} {
		if seconds, err := window(arg); err != nil || seconds != want || warnings.Len() != 0 {
			t.Errorf("provider-health %s: window %v seconds (%v), warnings %q; want %v and no warning", arg, seconds, err, warnings.String(), want)
		}
	}
	if _, err := window("--window", "soon"); exitCode(err) != 2 || warnings.Len() != 0 {
		t.Errorf("provider-health --window soon: exit %d (%v), warnings %q; want usage 2 and no warning", exitCode(err), err, warnings.String())
	}
}

// FAE-CLI-001 and FAE-CTL-020: the watchdog's period has no default in the
// spec, so it keeps its own check: a nonpositive period is a usage error (2)
// with no fallback warning, before any instance is read. Before, 0 and
// negative periods were accepted and the watchdog silently ticked every
// minute; here they reached the missing instance and exited 3.
func TestWatchdogNonpositivePeriodIsAUsageError(t *testing.T) {
	warnings := captureWarnings(t)
	home := shortHome(t)
	for _, period := range []string{"0s", "-1m", "0"} {
		warnings.Reset()
		err := execute(context.Background(), []string{"watchdog", "--home", home, "--instance", "dog", "--authorizing-message", "m", "--period", period}, &bytes.Buffer{})
		if exitCode(err) != 2 || !strings.Contains(fmt.Sprint(err), "--period must be a positive duration") || warnings.Len() != 0 {
			t.Errorf("watchdog --period %s: exit %d (%v), warnings %q; want usage 2 naming --period", period, exitCode(err), err, warnings.String())
		}
	}
	// A positive period passes the check and reaches the instance, which
	// was never started: unreachable (3).
	if err := execute(context.Background(), []string{"watchdog", "--home", home, "--instance", "dog", "--authorizing-message", "m", "--period", "20ms"}, &bytes.Buffer{}); exitCode(err) != 3 {
		t.Errorf("watchdog --period 20ms on a never-started instance: exit %d (%v), want 3", exitCode(err), err)
	}
}

// FAE-CLI-001 and REQUIREMENTS.md conventions: --instance defaults to
// default and --home to ~/.fae/instances; an empty value, before or after the
// verb, keeps the default with a warning. Before, an empty instance exited 1
// (invalid instance name) and an empty home 1 (invalid absolute path).
func TestEmptyInstanceOrHomeKeepsItsDefault(t *testing.T) {
	warnings := captureWarnings(t)
	home, requests := recordingDaemon(t, "default", map[string]any{"ok": true})
	for _, args := range [][]string{
		{"--home", home, "--instance", "", "status"},
		{"--home=" + home, "--instance=", "status"},
		{"--home", home, "status", "--instance", ""},
		{"--home", home, "status", "--instance="},
		{"--home", home, "status", "--instance", " "},
	} {
		warnings.Reset()
		if err := execute(context.Background(), args, &bytes.Buffer{}); err != nil {
			t.Errorf("%v: %v", args, err)
			continue
		}
		if r := <-requests; r.Method != "Status" {
			t.Errorf("%v called %s", args, r.Method)
		}
		checkWarned(t, strings.Join(args, " "), warnings, "instance=default component=cli warning: fae status: --instance is empty; using the default default")
	}
	// The last value wins, as for any flag: a later name is not a fallback.
	warnings.Reset()
	if err := execute(context.Background(), []string{"--home", home, "--instance", "", "--instance=default", "status"}, &bytes.Buffer{}); err != nil || warnings.Len() != 0 {
		t.Errorf("--instance '' --instance=default: %v, warnings %q", err, warnings.String())
	} else if r := <-requests; r.Method != "Status" {
		t.Errorf("--instance '' --instance=default called %s", r.Method)
	}
	// The default home is $HOME/.fae/instances.
	user := shortHome(t)
	t.Setenv("HOME", user)
	userHome, userRequests := filepath.Join(user, ".fae", "instances"), make(chan api.Request, 4)
	startRecorder(t, userHome, "default", userRequests)
	for _, args := range [][]string{{"--home", "", "status"}, {"--home=", "status"}, {"status", "--home", ""}, {"status", "--home="}} {
		warnings.Reset()
		if err := execute(context.Background(), args, &bytes.Buffer{}); err != nil {
			t.Errorf("%v: %v", args, err)
			continue
		}
		if r := <-userRequests; r.Method != "Status" {
			t.Errorf("%v called %s", args, r.Method)
		}
		checkWarned(t, strings.Join(args, " "), warnings, "fae status: --home is empty; using the default "+userHome)
	}
	// Any other invalid name or home is refused, never sent to the default.
	for _, args := range [][]string{{"--home", home, "--instance", "../x", "status"}, {"--home", "relative", "status"}, {"status", "--home", home, "--instance", "a b"}} {
		warnings.Reset()
		if err := execute(context.Background(), args, &bytes.Buffer{}); exitCode(err) != 1 || warnings.Len() != 0 {
			t.Errorf("%v: exit %d (%v), warnings %q; want refusal 1", args, exitCode(err), err, warnings.String())
		}
	}
	select {
	case r := <-requests:
		t.Errorf("a refused instance reached the default instance: %s", r.Method)
	case r := <-userRequests:
		t.Errorf("a refused home reached the default home: %s", r.Method)
	default:
	}
}

// startRecorder serves instance under home, recording each request method.
func startRecorder(t *testing.T, home, instance string, requests chan api.Request) {
	t.Helper()
	layout, err := daemon.Paths(home, instance)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(layout.Run, 0700); err != nil {
		t.Fatal(err)
	}
	token, err := api.Credential(layout.Credential)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		requests <- r
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = server.Listen(layout.Socket); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve() }()
	t.Cleanup(func() { server.Shutdown(context.Background()); <-done })
}

// FAE-CLI-010: serve's adapter, default mode and UI bind have defaults; an
// empty value keeps them with a warning. Any other invalid value is still
// refused: an unknown adapter or mode is a usage error.
func TestServeEmptyAdapterModeOrBindKeepsItsDefault(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "instances")
	config, warnings, _ := stubServe(t)
	serve := func(args ...string) error {
		warnings.Reset()
		*config = daemon.Config{}
		return execute(context.Background(), append([]string{"--home", home, "serve", "--workspace", root, "--foreground"}, args...), &bytes.Buffer{})
	}
	if err := serve("--adapter", "scripted", "--default-mode", ""); !strings.Contains(err.Error(), "stub start") || config.DefaultMode != "Local" {
		t.Errorf("--default-mode '': %v, mode %q", err, config.DefaultMode)
	}
	checkWarned(t, "--default-mode ''", warnings, "fae serve: --default-mode is empty; using the default local")
	if err := serve("--adapter", "scripted", "--ui", "--ui-bin", "/bin/true", "--ui-bind="); !strings.Contains(err.Error(), "stub start") || config.UI == nil || config.UI.Bind != "127.0.0.1" {
		t.Errorf("--ui-bind '': %v, UI %+v", err, config.UI)
	}
	checkWarned(t, "--ui-bind ''", warnings, "fae serve: --ui-bind is empty; using the default 127.0.0.1")
	// The default adapter is multiplexer, which needs a live configuration.
	if err := serve("--adapter="); exitCode(err) != 2 || !strings.Contains(err.Error(), "live adapter requires --live-config") {
		t.Errorf("--adapter '': exit %d %v", exitCode(err), err)
	}
	checkWarned(t, "--adapter ''", warnings, "fae serve: --adapter is empty; using the default multiplexer")
	for _, args := range [][]string{{"--adapter", "bogus"}, {"--adapter", "scripted", "--default-mode", "bogus"}} {
		if err := serve(args...); exitCode(err) != 2 || warnings.Len() != 0 {
			t.Errorf("%v: exit %d %v, warnings %q", args, exitCode(err), err, warnings.String())
		}
	}
}

// FAE-CLI-001: commands that read no instance state run without $HOME.
func TestOfflineCommandsNeedNoHomeVariable(t *testing.T) {
	oldDoctor := doctorHost
	t.Cleanup(func() { doctorHost = oldDoctor })
	doctorHost = func(context.Context) doctorEnv { // a passing host
		return doctorEnv{lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
			version:   func(string, ...string) (string, error) { return "git version 2.45.1", nil },
			statfs:    func(string) (uint64, uint64, error) { return 64 << 30, 1 << 20, nil },
			sandboxed: func() (bool, error) { return false, nil }}
	}
	example, err := filepath.Abs("../../examples/workflow.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")
	for _, args := range [][]string{{"version"}, {"version", "--json"}, {"workflow", "default"}, {"workflow", "default", "--local"}, {"workflow", "scenario"}, {"workflow", "validate", example}, {"template", "workflow-validate", "--content", example}, {"gates", "self-test", "--offline"}, {"doctor"}} {
		var out bytes.Buffer
		if err := execute(context.Background(), args, &out); err != nil || out.Len() == 0 {
			t.Errorf("%v without $HOME: exit %d %v", args, exitCode(err), err)
		}
	}
	// doctor reports the missing home as a warning, with its remedy.
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"doctor"}, &out); err != nil || !strings.Contains(out.String(), "$HOME is not defined and no --home was given") || !strings.Contains(out.String(), "pass --home PATH") {
		t.Errorf("doctor without $HOME: %v %s", err, out.String())
	}
	// A command that needs the instance still refuses, naming $HOME.
	if err := execute(context.Background(), []string{"gates", "self-test"}, &bytes.Buffer{}); exitCode(err) != 1 || !strings.Contains(err.Error(), "HOME") {
		t.Errorf("gates self-test without $HOME: exit %d %v", exitCode(err), err)
	}
}

// FAE-CLI-001: the common flags are accepted before or after the verb, as
// --flag VALUE or --flag=VALUE, on the offline workflow forms too, which read
// no instance. Before, workflow validate FILE --instance=x exited 2 while
// --instance x exited 0.
func TestOfflineWorkflowFormsAcceptEitherCommonFlagForm(t *testing.T) {
	example, err := filepath.Abs("../../examples/workflow.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"workflow", "validate", example, "--instance", "x"},
		{"workflow", "validate", example, "--instance=x"},
		{"workflow", "validate", "--home=/nonexistent/fae", example},
		{"workflow", "validate", example, "--home", "/nonexistent/fae", "--instance=x", "--json"},
		{"--instance=x", "workflow", "validate", example},
	} {
		var out bytes.Buffer
		if err := execute(context.Background(), args, &out); err != nil || out.Len() == 0 {
			t.Errorf("%v: exit %d %v", args, exitCode(err), err)
		}
	}
	// A common flag with no value is still a usage error.
	if err := execute(context.Background(), []string{"workflow", "validate", example, "--instance"}, &bytes.Buffer{}); exitCode(err) != 2 {
		t.Errorf("workflow validate FILE --instance: exit %d %v, want usage 2", exitCode(err), err)
	}
}

// FAE-CLI-001 against FAE-HOST-011: the hook's help states its own codes, 0
// and 2 (never 1, which would let the provider run the tool); the brokers'
// help states theirs instead of the generic table, including the 1 gate run
// exits when it cannot read its working directory or make a run identity, and
// the 1 and 126 an account-mode --shim-policy run exits when cancelled before
// the tool runs or when the exec fails. Before, those three were missing.
func TestBrokerAndHookHelpStateTheirOwnExitCodes(t *testing.T) {
	for args, want := range map[string][]string{
		"heavy-op hook --help --json":    {"0", "2"},
		"heavy-op run --help --json":     {"0", "1", "2", "75", "77", "78", "126", "other"},
		"gate run --help --json":         {"0", "1", "2", "75", "76", "other"},
		"heavy-op --help --json":         {"0", "1", "2", "75", "77", "78", "126", "other"},
		"status --help --json":           {"0", "1", "2", "3"},
		"backlog edge --help --json":     {"0", "1", "2", "3"},
		"maintenance stop --help --json": {"0", "1", "2", "3"},
	} {
		var out bytes.Buffer
		if err := execute(context.Background(), strings.Fields(args), &out); err != nil {
			t.Errorf("%s: %v", args, err)
			continue
		}
		var help struct {
			ExitCodes map[string]string `json:"exit_codes"`
		}
		if err := json.Unmarshal(out.Bytes(), &help); err != nil || len(help.ExitCodes) != len(want) {
			t.Errorf("%s: exit codes %v (%v), want %v", args, help.ExitCodes, err, want)
			continue
		}
		for _, code := range want {
			if help.ExitCodes[code] == "" {
				t.Errorf("%s: exit codes %v lack %s", args, help.ExitCodes, code)
			}
		}
	}
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"heavy-op", "hook", "--help"}, &out); err != nil || !strings.Contains(out.String(), "Exit codes: 0 success: the decision is on standard output") || strings.Contains(out.String(), "refusal or validation failure") {
		t.Errorf("heavy-op hook --help: %v %q", err, out.String())
	}
}
