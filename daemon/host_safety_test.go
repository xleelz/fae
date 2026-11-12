package daemon

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/processowner"
	"github.com/xleelz/fae/scheduler"
)

func alertsBySource(t *testing.T, d *Daemon) map[string][]controlplane.Alert {
	t.Helper()
	page := rpcResult[AlertsPage](t, d, "Alerts", AlertsParams{Limit: 500})
	out := map[string][]controlplane.Alert{}
	for _, a := range page.Alerts {
		out[a.Source] = append(out[a.Source], a)
	}
	return out
}

// FAE-HOST-003, FAE-OBS-005: a hard refusal inside a brokered command (role
// shims through heavy-op run) is spooled and raised by the owning daemon as a
// host-admission alert on its single alert path; the record is consumed, and
// an unreadable record is dropped without an alert.
func TestSpooledBrokerHardRefusalRaisesAlert(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	spool := d.hostAlertSpool()
	if err = mkdirPrivate(spool); err != nil {
		t.Fatal(err)
	}
	hard := hostguard.Sample{Memory: hostguard.Reading{Value: float64(hostguard.GiB), Known: true}, Swap: hostguard.Reading{Value: 0, Known: true}}
	if err = commandshim.SpoolHostAlert(spool, "heavy-op-test", hard); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(spool, "torn.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	d.observeRound(ctx)
	entries, _ := os.ReadDir(spool)
	if len(entries) != 0 {
		t.Fatal("spooled records not consumed", entries)
	}
	alerts := alertsBySource(t, d)["host-admission"]
	if len(alerts) != 1 || !strings.Contains(string(alerts[0].Detail), `"gate":"heavy-op"`) || !strings.Contains(string(alerts[0].Detail), `"reason":"HostHardLimit"`) {
		t.Fatalf("host-admission alerts %+v", alerts)
	}
	if ev, err := d.store.LastJobEvent(ctx, "", "alert-raised"); err != nil || !strings.Contains(string(ev.Payload), "host-admission") {
		t.Fatal("alert raised without its event", err)
	}
}

// FAE-OBS-005: a drive exit and an exhausted re-arm budget each raise an alert.
// The failure cause's durable budget is already spent, so the automatic re-arm
// after an injected infrastructure failure is refused.
func TestDriveExitAndRearmExhaustionRaiseAlerts(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	awaitDrive(t, d, true, 1)
	spent, _ := json.Marshal(driveBudget{Start: time.Now().UTC(), Count: 3})
	if err = d.store.PutMarker(ctx, controlplane.Audit{Actor: "test", Action: "spend re-arm budget", Outcome: "ok"}, "drive-rearm/drive-failure", spent); err != nil {
		t.Fatal(err)
	}
	var fired atomic.Bool
	d.mu.Lock()
	d.testFault = func(point, id string) error {
		if point == "step" && fired.CompareAndSwap(false, true) {
			return &infrastructureError{errors.New("injected drive failure")}
		}
		return nil
	}
	d.mu.Unlock()
	d.signal()
	deadline := time.Now().Add(30 * time.Second)
	for {
		exited, refused := false, false
		for _, a := range alertsBySource(t, d)["controller"] {
			detail := string(a.Detail)
			exited = exited || strings.Contains(detail, `"cause":"drive-failure"`) && strings.Contains(detail, "injected drive failure")
			refused = refused || strings.Contains(detail, "budget exhausted")
		}
		if exited && refused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("drive exit %t, re-arm exhaustion %t: %+v", exited, refused, alertsBySource(t, d))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if h := awaitDrive(t, d, false, 1); h.DriveCause != "drive-failure" {
		t.Fatal("drive re-armed past its budget", h.DriveCause)
	}
}

// FAE-HOST-020: brokered command scratch left by a killed daemon is swept at
// start, unless a host command's retirement is still unproven.
func TestStartSweepsBrokeredCommandScratch(t *testing.T) {
	root := t.TempDir()
	d := &Daemon{layout: Layout{Root: root, Run: filepath.Join(root, "run"), Worktrees: filepath.Join(root, "worktrees")}}
	for _, dir := range []string{"command-tmp-1", "command-policy-2/command-shims", "task-a"} {
		if err := os.MkdirAll(filepath.Join(d.layout.Worktrees, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(d.layout.Worktrees, "command-policy-2", "command-shims", "go"), []byte("#!/bin/sh\n"), 0500); err != nil {
		t.Fatal(err)
	}
	journal, err := scheduler.OpenJournal(filepath.Join(d.layout.Run, "lease-proofs"))
	if err != nil {
		t.Fatal(err)
	}
	unproven := scheduler.OperationProof{ID: "p", Session: "s", Instance: root, Birth: "gone", Owner: scheduler.Owner{PID: 4242, Started: 77}, State: "launching", Expected: 1, Registry: filepath.Join(d.layout.Run, "effect-processes"), ProcessOwner: d.layout.Worktrees}
	if err = journal.Put(unproven); err != nil {
		t.Fatal(err)
	}
	if err = d.sweepCommandScratch(journal); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(d.layout.Worktrees, "command-tmp-1")); err != nil {
		t.Fatal("scratch swept while a host command's retirement was unproven")
	}
	if err = journal.Remove("p"); err != nil {
		t.Fatal(err)
	}
	if err = d.sweepCommandScratch(journal); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(d.layout.Worktrees)
	if len(entries) != 1 || entries[0].Name() != "task-a" {
		t.Fatal("brokered scratch not swept or a worktree removed", entries)
	}
}

// FAE-HOST-022 with FAE-ARC-002: an idle round's census reads only the
// daemon's own tree, still finds a leak there, and keeps the last host-wide
// foreign report.
func TestIdleRoundCensusesOwnTree(t *testing.T) {
	d := &Daemon{}
	d.obs.limits = processowner.LeakLimits{Processes: 4, RSS: 64 << 30, Defunct: 20}
	foreign := []processowner.CommandUsage{{Command: "elsewhere", Processes: 9999}}
	d.obs.leaks = &processowner.LeakReport{Foreign: foreign}
	chain := exec.Command("/bin/sh", "-c", "sleep 60 & sleep 60 & sleep 60 & sleep 60 & sleep 60 & wait")
	chain.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := chain.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-chain.Process.Pid, syscall.SIGKILL); _ = chain.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_ = d.detectLeaks(context.Background(), false) // no plane: the alert itself is not recorded
		r := d.processReport()
		if r != nil && len(r.Leaks) > 0 {
			if r.Leaks[0].Command != "sleep" || len(r.Foreign) != 1 || r.Foreign[0].Command != "elsewhere" {
				t.Fatalf("own-tree census %+v", r)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("own-tree census missed the chain: %+v", r)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSandboxedServeHelper is the child of TestSandboxedServeRefusedNamingCondition.
func TestSandboxedServeHelper(t *testing.T) {
	if os.Getenv("DAEMON_SANDBOX_HELPER") != "1" {
		return
	}
	_, err := Start(context.Background(), Config{})
	if err == nil {
		os.Stdout.WriteString("started\n")
		os.Exit(0)
	}
	os.Stdout.WriteString(err.Error() + "\n")
	os.Exit(3)
}

// FAE-HOST-030: a launch from a simulated sandboxed context (no-new-privileges,
// as an agent session sets it) is refused, naming the condition and the
// supported unsandboxed launcher.
func TestSandboxedServeRefusedNamingCondition(t *testing.T) {
	setpriv, err := exec.LookPath("setpriv")
	if err != nil {
		t.Skip("setpriv unavailable")
	}
	cmd := exec.Command(setpriv, "--no-new-privs", os.Args[0], "-test.run=^TestSandboxedServeHelper$")
	cmd.Env = append(os.Environ(), "DAEMON_SANDBOX_HELPER=1")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 3 {
		t.Fatalf("sandboxed serve was not refused: %v %s", err, out)
	}
	for _, want := range []string{"fae serve refused", "no-new-privileges set", "unsandboxed operator shell", "fae runtime-launcher"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("refusal %q does not name %q", out, want)
		}
	}
}

// FAE-HOST-021: the UI child is in the instance inventory under its role.
func TestUIChildInventoryRole(t *testing.T) {
	cfg, ctx := fixture(t)
	fastUISupervision(t, 10)
	cfg.UI = &UIConfig{Binary: buildUIChild(t, filepath.Dir(cfg.Workspace)), Port: freeLoopbackPort(t)}
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	healthy := awaitUI(t, d, "became healthy", func(s UIStatus) bool { return s.Health == "healthy" && s.PID != nil })
	records, err := processowner.Inventory(filepath.Join(d.layout.Run, "ui-processes"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range records {
		found = found || r.PID == *healthy.PID && r.Role == "ui"
	}
	if !found {
		t.Fatalf("UI child %d not inventoried with role ui: %+v", *healthy.PID, records)
	}
}

// FAE-OBS-001: a detached daemon's serve log (its stdout and stderr) rotates at
// the limit; the descriptors then write to a fresh private file.
func TestServeLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "census.serve.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Dup(int(f.Fd()))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	target := os.NewFile(uintptr(fd), "serve log")
	defer target.Close()
	r := &fdRotator{path: path, target: target, fds: []int{fd}, limit: 100, keep: 2}
	for _, line := range []string{"one", "two", "three", "four", "five", "six", "seven", "eight"} {
		if _, err = r.Write([]byte(line + strings.Repeat(".", 39-len(line)) + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	// Two lines fit under the limit, so each file holds one pair.
	if !strings.HasPrefix(read(path), "seven") || !strings.HasPrefix(read(path+".1"), "five") || !strings.HasPrefix(read(path+".2"), "three") {
		t.Fatalf("rotation: %q | %q | %q", read(path), read(path+".1"), read(path+".2"))
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("rotated serve log not private", info, err)
	}
	if _, err = os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatal("kept more than the bound", err)
	}
}

// FAE-SEC-003: after a scripted run with a delegated principal, a scan of the
// plane (every table, current and historical rows), the logs and every other
// file of the instance finds none of the planted secrets: the instance
// credential, the plane password, a delegated credential and a provider
// credential in the daemon's environment.
func TestScriptedRunLeaksNoPlantedSecret(t *testing.T) {
	cfg, ctx := fixture(t)
	provider := fmt.Sprintf("planted-provider-secret-%d", time.Now().UnixNano())
	t.Setenv("ANTHROPIC_API_KEY", provider)
	configPath := filepath.Join(filepath.Dir(cfg.Root), "principals.json")
	controllerPath := filepath.Join(filepath.Dir(cfg.Root), "controller-token")
	raw, _ := json.Marshal([]PrincipalCredential{{CredentialFile: controllerPath, Actor: "controller"}})
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg.PrincipalConfig = configPath
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	for _, id := range []string{"one", "two"} {
		if _, err = call(t, d, "Admit", AdmitParams{ID: id, Tree: "tree", AuthorizingMessage: "message"}, id); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"one", "two"} {
		awaitState(t, d, id, "Done")
	}
	delegated, err := os.ReadFile(controllerPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = api.Call(ctx, d.layout.Socket, delegated, api.Request{Version: api.Version, Method: "Board", Params: json.RawMessage(`{}`)}); err != nil {
		t.Fatal("delegated read", err)
	}
	if _, err = api.Call(ctx, d.layout.Socket, []byte(strings.Repeat("x", 32)), api.Request{Version: api.Version, Method: "Board", Params: json.RawMessage(`{}`)}); err == nil {
		t.Fatal("wrong credential accepted")
	}
	secrets := map[string]string{"provider credential": provider}
	for name, path := range map[string]string{"instance credential": d.layout.Credential, "plane password": filepath.Join(d.layout.Run, "plane-credential"), "delegated credential": controllerPath} {
		b, err := os.ReadFile(path)
		if err != nil || len(b) != 32 {
			t.Fatal(name, err)
		}
		secrets[name] = hex.EncodeToString(b)
	}
	var leaks []string
	check := func(where string, value []byte) {
		for name, secret := range secrets {
			if bytes.Contains(value, []byte(secret)) {
				leaks = append(leaks, name+" in "+where)
			}
		}
	}
	scanned := 0
	err = d.store.Mutate(ctx, controlplane.Audit{Actor: "test", Action: "planted secret scan", Outcome: "ok"}, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'`)
		if err != nil {
			return err
		}
		var tables []string
		for rows.Next() {
			var name string
			if err = rows.Scan(&name); err != nil {
				rows.Close()
				return err
			}
			tables = append(tables, name)
		}
		rows.Close()
		for _, table := range tables {
			for _, q := range []string{"SELECT * FROM `" + table + "`", "SELECT * FROM `dolt_history_" + table + "`"} {
				rows, err := tx.QueryContext(ctx, q)
				if err != nil {
					return fmt.Errorf("%s: %w", q, err)
				}
				columns, _ := rows.Columns()
				values := make([]sql.RawBytes, len(columns))
				dest := make([]any, len(columns))
				for i := range values {
					dest[i] = &values[i]
				}
				for rows.Next() {
					if err = rows.Scan(dest...); err != nil {
						rows.Close()
						return err
					}
					for _, v := range values {
						check(q, v)
					}
					scanned++
				}
				rows.Close()
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	private := map[string]bool{d.layout.Credential: true, filepath.Join(d.layout.Run, "plane-credential"): true}
	files := 0
	err = filepath.WalkDir(d.layout.Root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && path == d.layout.Data {
			return filepath.SkipDir // scanned through SQL above
		}
		if entry.Type().IsRegular() && !private[path] && !strings.HasPrefix(entry.Name(), "controller-environment-") {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			check(path, b)
			files++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if scanned == 0 || files == 0 {
		t.Fatal("nothing scanned", scanned, files)
	}
	if len(leaks) > 0 {
		t.Fatalf("planted secrets leaked: %v", leaks)
	}
}

// FAE-SEC-003: a delegated credential file, or the principal configuration,
// inside a repository or worktree is refused before it is created or read.
func TestPrincipalCredentialsOutsideRepositories(t *testing.T) {
	server, err := api.New(bytes.Repeat([]byte{1}, 32), func(context.Context, api.Request) (any, error) { return nil, nil })
	if err != nil {
		t.Fatal(err)
	}
	root := outsideCheckout(t)
	repo := filepath.Join(root, "repo")
	if err = os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path string, entries []PrincipalCredential) {
		raw, _ := json.Marshal(entries)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	inside := filepath.Join(repo, "controller.credential")
	write(filepath.Join(root, "principals.json"), []PrincipalCredential{{CredentialFile: inside, Actor: "controller"}})
	if err = installPrincipals(server, filepath.Join(root, "principals.json")); err == nil || !strings.Contains(err.Error(), "outside any repository") {
		t.Fatal("credential inside a repository accepted", err)
	}
	if _, err = os.Stat(inside); !os.IsNotExist(err) {
		t.Fatal("credential created inside a repository")
	}
	write(filepath.Join(repo, "principals.json"), []PrincipalCredential{{CredentialFile: filepath.Join(root, "ok.credential"), Actor: "controller"}})
	if err = installPrincipals(server, filepath.Join(repo, "principals.json")); err == nil || !strings.Contains(err.Error(), "outside any repository") {
		t.Fatal("configuration inside a repository accepted", err)
	}
	write(filepath.Join(root, "principals.json"), []PrincipalCredential{{CredentialFile: filepath.Join(root, "ok.credential"), Actor: "controller"}})
	if err = installPrincipals(server, filepath.Join(root, "principals.json")); err != nil {
		t.Fatal(err)
	}
}

// FAE-OBS-006: every refusal produces exactly one event and one line, the line
// after the event: a refused mutating RPC, and a refused daemon-internal
// host-heavy operation (which has no task park to pair with). A failed read
// is not a refusal and writes neither.
func TestRefusalsPairEventAndLine(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	lines := func(needle string) int {
		raw, _ := os.ReadFile(d.layout.Log)
		return strings.Count(string(raw), needle)
	}
	if _, err = callOnce(t, d, "Admit", AdmitParams{ID: "refused", Tree: "tree"}, "refused"); err == nil {
		t.Fatal("admit without an authorizing message accepted")
	}
	if _, err = callOnce(t, d, "History", map[string]string{"id": "no-such-task"}, ""); err == nil {
		t.Log("history of an unknown task answered")
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM events WHERE kind='rpc-refused'`); n != 1 {
		t.Fatalf("refused RPC events %d", n)
	}
	if n := lines("component=rpc refused method=Admit "); n != 1 {
		t.Fatalf("refused RPC lines %d", n)
	}
	if n := lines("component=rpc refused method=History"); n != 0 {
		t.Fatal("a read was logged as a refusal")
	}
	a := &liveAdmission{daemon: d}
	internal := context.WithValue(ctx, hostIOOperation{}, "plane-backup")
	step := hostguard.Step{Outcome: hostguard.OutcomeRefused, Reason: "IOPressure", Level: hostguard.LevelPressure}
	var refusal *scheduler.AdmissionRefusal
	if err = a.refuseIn(internal, "io", "plane-backup", "", 11, step); !errors.As(err, &refusal) || refusal.Reason != "IOPressure" {
		t.Fatal(err)
	}
	if n := countRows(t, d, `SELECT COUNT(*) FROM events WHERE kind='host-admission-refused'`); n != 1 {
		t.Fatalf("internal refusal events %d", n)
	}
	if n := lines("host admission refused gate=io outcome=refused reason=IOPressure id=plane-backup"); n != 1 {
		t.Fatalf("internal refusal lines %d", n)
	}
}

// FAE-OBS-003: an operator message attached to a task appears in both of the
// task's history views.
func TestTaskHistoryShowsOperatorMessages(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop(t, d)
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "one", Tree: "tree", AuthorizingMessage: "message"}, "one"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "one", "Done")
	if _, err = call(t, d, "Message", MessageParams{ID: "about-one", Text: "please keep the fixture small", Task: "one"}, ""); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]HistoryParams{"events": {ID: "one", Summary: true}, "records": {ID: "one", Records: true}} {
		reply, err := call(t, d, "History", p, "")
		if err != nil {
			t.Fatal(name, err)
		}
		var page struct {
			Messages []controlplane.Message `json:"messages"`
		}
		if err = json.Unmarshal(reply.Result, &page); err != nil || len(page.Messages) != 1 || page.Messages[0].ID != "about-one" || page.Messages[0].Text != "please keep the fixture small" {
			t.Fatalf("%s history messages %s %v", name, reply.Result, err)
		}
	}
}
