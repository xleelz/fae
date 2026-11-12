package daemon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/commandshim"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/effects"
)

// The live config's nested_commands and max_output_bytes are checked offline
// (fae doctor) with one named issue each; valid values and omission pass.
func TestCheckLiveConfigNamesNestedModeAndOutputBound(t *testing.T) {
	bounds := func(c LiveConfig) []string {
		var named []string
		for _, issue := range CheckLiveConfig(c) {
			if strings.Contains(issue, "nested_commands") || strings.Contains(issue, "max_output_bytes") {
				named = append(named, issue)
			}
		}
		return named
	}
	issues := bounds(LiveConfig{NestedCommands: "acount", MaxOutputBytes: 512 << 10})
	if len(issues) != 2 || !strings.Contains(issues[0], `nested_commands "acount" must be "refuse" or "account"`) || !strings.Contains(issues[1], "max_output_bytes 524288 must be between 1048576 (1 MiB) and 67108864 (64 MiB)") {
		t.Fatalf("issues %q", issues)
	}
	for _, bad := range []int{-1, 1<<20 - 1, 64<<20 + 1} {
		if len(bounds(LiveConfig{MaxOutputBytes: bad})) != 1 {
			t.Fatal("out-of-range max_output_bytes passed", bad)
		}
	}
	saved := liveNestedBroker
	defer func() { liveNestedBroker = saved }()
	liveNestedBroker = func() string { return "/opt/fae/bin/fae" }
	for _, c := range []LiveConfig{{}, {NestedCommands: "refuse", MaxOutputBytes: 1 << 20}, {NestedCommands: "account", MaxOutputBytes: 64 << 20}} {
		if issues := bounds(c); len(issues) != 0 {
			t.Fatalf("%+v: %q", c, issues)
		}
	}
	// Account mode's policy runs in the fae executable; a daemon running as
	// anything else (a test binary) cannot apply it, and says so.
	liveNestedBroker = func() string { return "" }
	if issues := bounds(LiveConfig{NestedCommands: "account"}); len(issues) != 1 || !strings.Contains(issues[0], `nested_commands "account" needs the fae executable`) {
		t.Fatalf("account mode without the fae executable: %q", issues)
	}
	if issues := bounds(LiveConfig{NestedCommands: "refuse"}); len(issues) != 0 {
		t.Fatalf("refuse mode needs no broker: %q", issues)
	}
}

// FAE-HOST-014 acceptance: "a test with a nested invocation shows it
// accounted". Setup refuses a bad nested_commands mode or output bound. In
// account mode the workspace's own test runs a nested git through the live
// broker and the fae nested-command policy: git --version runs, and git push
// in this Local task exits 78 (FAE-MODE-002). The task reaches Done, and every
// engine run of the task has a durable nested-commands record of its own,
// naming its task, dispatch, command and output digest; the QA run's names
// the nested calls. The default refuse mode would exit 77 instead.
func TestLiveAccountModeRecordsNestedCommandsDurably(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	nested := "package sample\n\nimport (\n\t\"os/exec\"\n\t\"strings\"\n\t\"testing\"\n)\n\nfunc TestNestedToolCall(t *testing.T) {\n\tif out, err := exec.Command(\"git\", \"--version\").CombinedOutput(); err != nil {\n\t\tt.Fatalf(\"nested git: %v %s\", err, out)\n\t}\n\tcmd := exec.Command(\"git\", \"push\", \"origin\", \"HEAD\")\n\tout, _ := cmd.CombinedOutput()\n\tif cmd.ProcessState.ExitCode() != 78 || !strings.Contains(string(out), \"Local mode\") {\n\t\tt.Fatalf(\"nested git push in a Local task: exit %d %s\", cmd.ProcessState.ExitCode(), out)\n\t}\n}\n"
	if err := os.WriteFile(filepath.Join(cfg.Workspace, "nested_test.go"), []byte(nested), 0600); err != nil {
		t.Fatal(err)
	}
	liveGit(t, cfg.Workspace, "add", "nested_test.go")
	liveGit(t, cfg.Workspace, "commit", "-m", "a test with a nested tool call")
	cfg.Live.Base = liveGit(t, cfg.Workspace, "rev-parse", "HEAD")
	liveGit(t, cfg.Workspace, "branch", "-f", "fae/integration", cfg.Live.Base)
	useBuiltFaeBroker(t)

	cfg.Live.NestedCommands, cfg.Live.MaxOutputBytes = "acount", 512<<10
	if _, err := Start(ctx, cfg); err == nil || !strings.Contains(err.Error(), `nested_commands "acount"`) || !strings.Contains(err.Error(), "max_output_bytes 524288") {
		t.Fatal("bad nested mode and output bound started", err)
	}
	cfg.Live.NestedCommands, cfg.Live.MaxOutputBytes = "account", 65<<20
	if _, err := Start(ctx, cfg); err == nil || !strings.Contains(err.Error(), "max_output_bytes 68157440 must be between") {
		t.Fatal("output bound above 64 MiB started", err)
	}
	cfg.Live.MaxOutputBytes = 8 << 20
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	if b := d.live.broker; b.NestedCommands != effects.NestedAccount || b.MaxOutput != 8<<20 || b.RecordNested == nil {
		t.Fatalf("live broker not configured: mode %q bound %d", b.NestedCommands, b.MaxOutput)
	}
	authorize(t, d)
	if _, err = call(t, d, "Admit", AdmitParams{ID: "nested-one", AuthorizingMessage: "message"}, "nested-one"); err != nil {
		t.Fatal(err)
	}
	awaitState(t, d, "nested-one", "Done")
	runs, err := d.store.EngineRuns(ctx, "nested-one", 1000)
	if err != nil || len(runs) == 0 {
		t.Fatal(len(runs), err)
	}
	markers, err := d.store.MarkersWithPrefix(ctx, "nested-commands/", 1000)
	if err != nil {
		t.Fatal(err)
	}
	// One record per run, even where two runs printed the same output.
	if len(markers) != len(runs) {
		t.Fatalf("%d engine runs, %d nested-commands records", len(runs), len(markers))
	}
	records := []nestedRecordView{}
	for key, raw := range markers {
		var rec nestedRecordView
		if err = json.Unmarshal(raw, &rec); err != nil || key != "nested-commands/"+controlplane.EngineRunID(controlplane.EngineRun{DispatchID: rec.DispatchID, Command: rec.Command, Started: rec.Started}) || rec.TaskID != "nested-one" || rec.Count != len(rec.Calls) {
			t.Fatalf("record %s %s: %v", key, raw, err)
		}
		records = append(records, rec)
	}
	accounted := false
	for _, run := range runs {
		var command []string
		if err = json.Unmarshal(run.Command, &command); err != nil {
			t.Fatal(err)
		}
		i := slices.IndexFunc(records, func(rec nestedRecordView) bool {
			return rec.DispatchID == run.DispatchID && slices.Equal(rec.Command, command) && rec.OutputDigest == run.OutputDigest && rec.Tree == run.Tree
		})
		if i < 0 {
			t.Fatalf("engine run %s %v has no nested-commands record", run.RunID, command)
		}
		for _, c := range records[i].Calls {
			if reflect.DeepEqual(c.Argv, []string{"git", "--version"}) && c.Dir != "" && !c.At.IsZero() {
				accounted = true
			}
		}
		records = slices.Delete(records, i, i+1)
	}
	if !accounted {
		t.Fatalf("no engine run accounted the nested git call: %d runs, %d records", len(runs), len(markers))
	}
}

// Two account-mode runs with identical output (any two silent runs) keep a
// record each: the key is the run's dispatch, command and start, not its
// output, and each record names its task and dispatch.
func TestNestedCommandRecordsAreKeyedPerRun(t *testing.T) {
	cfg, ctx := fixture(t)
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	digest := strings.Repeat("0", 64)
	started := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for i, task := range []string{"task-a", "task-b"} {
		run := nestedTestEvidence(digest, []string{"gofmt", "-l", "."}, started.Add(time.Duration(i)*time.Millisecond))
		if err = d.recordNestedCommands(nestedTestContext(ctx, task, "dispatch-"+task), run); err != nil {
			t.Fatal(err)
		}
	}
	markers, err := d.store.MarkersWithPrefix(ctx, "nested-commands/", 1000)
	if err != nil || len(markers) != 2 {
		t.Fatalf("two identical-output runs left %d records: %v", len(markers), err)
	}
	tasks := map[string]string{}
	for key, raw := range markers {
		var rec nestedRecordView
		if err = json.Unmarshal(raw, &rec); err != nil || rec.OutputDigest != digest || key != "nested-commands/"+controlplane.EngineRunID(controlplane.EngineRun{DispatchID: rec.DispatchID, Command: rec.Command, Started: rec.Started}) {
			t.Fatalf("record %s %s: %v", key, raw, err)
		}
		tasks[rec.TaskID] = rec.DispatchID
	}
	if tasks["task-a"] != "dispatch-task-a" || tasks["task-b"] != "dispatch-task-b" {
		t.Fatalf("records name tasks and dispatches %v", tasks)
	}
}

// nestedRecordView is the durable nested-commands record as stored.
type nestedRecordView struct {
	TaskID       string                   `json:"task_id"`
	DispatchID   string                   `json:"dispatch_id"`
	Command      []string                 `json:"command"`
	Started      time.Time                `json:"started"`
	OutputDigest string                   `json:"output_digest"`
	Tree         string                   `json:"tree"`
	Count        int                      `json:"count"`
	Calls        []commandshim.NestedCall `json:"calls"`
}

// nestedTestEvidence is an account-mode run's evidence with no nested call.
func nestedTestEvidence(digest string, command []string, started time.Time) effects.RunEvidence {
	return effects.RunEvidence{OutputDigest: digest, Tree: "tree", Command: command, Started: started}
}

// nestedTestContext is a dispatch's context, as the live executor sets it.
func nestedTestContext(ctx context.Context, task, dispatch string) context.Context {
	return effects.WithNestedRun(ctx, effects.NestedRun{TaskID: task, DispatchID: dispatch, Local: true})
}

// useBuiltFaeBroker points the live daemon's nested-command policy at a fae
// built from this module, since the daemon under test is a test binary.
func useBuiltFaeBroker(t *testing.T) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "fae")
	_, file, _, _ := runtime.Caller(0)
	cmd := exec.Command("go", "build", "-o", bin, "github.com/xleelz/fae/cmd/fae")
	cmd.Dir = filepath.Dir(file)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fae: %v %s", err, out)
	}
	saved := liveNestedBroker
	liveNestedBroker = func() string { return bin }
	t.Cleanup(func() { liveNestedBroker = saved })
}
