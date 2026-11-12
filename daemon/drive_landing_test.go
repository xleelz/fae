package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xleelz/fae/controller"
	"github.com/xleelz/fae/controlplane"
	"github.com/xleelz/fae/defaults"
	"github.com/xleelz/fae/domain"
	"github.com/xleelz/fae/engine"
	"github.com/xleelz/fae/hostguard"
	"github.com/xleelz/fae/workflow"
)

func TestDriveLandingBuildTestArgsRunAndDefaultCompatibility(t *testing.T) {
	baseArgs := []string{"go", "test", "-json", "-count=1", "./..."}

	cases := []struct {
		name string
		job  *LandingJob
		base []string
		want []string
	}{
		{
			name: "nil job returns cloned baseArgs",
			job:  nil,
			base: baseArgs,
			want: []string{"go", "test", "-json", "-count=1", "./..."},
		},
		{
			name: "empty shape and empty filter preserves baseArgs",
			job:  &LandingJob{TestShape: "", TestFilter: ""},
			base: baseArgs,
			want: []string{"go", "test", "-json", "-count=1", "./..."},
		},
		{
			name: "empty shape with non-empty filter defaults to run mode",
			job:  &LandingJob{TestShape: "", TestFilter: "TestLandingGate"},
			base: baseArgs,
			want: []string{"go", "test", "-json", "-count=1", "./...", "-run", "TestLandingGate"},
		},
		{
			name: "explicit run shape appends -run flag",
			job:  &LandingJob{TestShape: "run", TestFilter: "^TestDriveLanding$"},
			base: baseArgs,
			want: []string{"go", "test", "-json", "-count=1", "./...", "-run", "^TestDriveLanding$"},
		},
		{
			name: "explicit run shape with empty filter leaves baseArgs unchanged",
			job:  &LandingJob{TestShape: "run", TestFilter: "   "},
			base: baseArgs,
			want: []string{"go", "test", "-json", "-count=1", "./..."},
		},
		{
			name: "run shape updates existing -run flag in baseArgs",
			job:  &LandingJob{TestShape: "run", TestFilter: "TestReplacement"},
			base: []string{"go", "test", "-run", "TestOld", "./..."},
			want: []string{"go", "test", "-run", "TestReplacement", "./..."},
		},
		{
			name: "go-unit workflow shape aliases to run mode",
			job:  &LandingJob{TestShape: "go-unit", TestFilter: "TestUnitCheck"},
			base: baseArgs,
			want: []string{"go", "test", "-json", "-count=1", "./...", "-run", "TestUnitCheck"},
		},
		{
			name: "empty baseArgs defaults to go test ./...",
			job:  &LandingJob{TestShape: "run", TestFilter: "TestDefaultBase"},
			base: nil,
			want: []string{"go", "test", "./...", "-run", "TestDefaultBase"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := buildLandingTestArgs(tc.job, tc.base)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("buildLandingTestArgs(%+v, %v) = %v, want %v", tc.job, tc.base, got, tc.want)
			}
		})
	}
}

func TestDriveLandingBuildTestArgsPkgShape(t *testing.T) {
	baseArgs := []string{"go", "test", "-json", "-count=1", "./..."}

	cases := []struct {
		name string
		job  *LandingJob
		base []string
		want []string
	}{
		{
			name: "pkg shape with comma and whitespace separated package patterns",
			job: &LandingJob{
				TestShape:  "pkg",
				TestFilter: "daemon, ./controller engine/... daemon",
			},
			base: baseArgs,
			want: []string{"go", "test", "-json", "-count=1", "./daemon", "./controller", "./engine/..."},
		},
		{
			name: "pkg shape normalizes root and wildcard patterns",
			job: &LandingJob{
				TestShape:  "pkg",
				TestFilter: "., ...",
			},
			base: baseArgs,
			want: []string{"go", "test", "-json", "-count=1", ".", "./..."},
		},
		{
			name: "pkg shape with empty filter derives unique package dirs from job.Files",
			job: &LandingJob{
				TestShape:  "pkg",
				TestFilter: "",
				Files:      []string{"daemon/drive_landing.go", "daemon/live_tasks.go", "controller/land.go", "root.go"},
			},
			base: baseArgs,
			want: []string{"go", "test", "-json", "-count=1", "./daemon", "./controller", "."},
		},
		{
			name: "pkg shape appends packages when baseArgs has no default target",
			job: &LandingJob{
				TestShape:  "pkg",
				TestFilter: "daemon",
			},
			base: []string{"go", "test", "-race"},
			want: []string{"go", "test", "-race", "./daemon"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLandingTestArgsChecked(tc.job, tc.base)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	for _, badFilter := range []string{"../outside", "/abs/pkg", "-exec=/bin/sh", "daemon;rm"} {
		job := &LandingJob{TestShape: "pkg", TestFilter: badFilter}
		if got := buildLandingTestArgs(job, baseArgs); got != nil {
			t.Fatalf("expected nil for unsafe pkg filter %q, got %v", badFilter, got)
		}
	}
}

func TestDriveLandingBuildTestArgsFileShape(t *testing.T) {
	baseArgs := []string{"go", "test", "-json", "-count=1", "./..."}

	cases := []struct {
		name string
		job  *LandingJob
		want []string
	}{
		{
			name: "file shape with file paths in TestFilter derives parent package dirs",
			job: &LandingJob{
				TestShape:  "file",
				TestFilter: "daemon/drive_landing_test.go, controller/land_test.go",
			},
			want: []string{"go", "test", "-json", "-count=1", "./daemon", "./controller"},
		},
		{
			name: "file shape with root .go file in TestFilter targets root package",
			job: &LandingJob{
				TestShape:  "file",
				TestFilter: "main_test.go",
			},
			want: []string{"go", "test", "-json", "-count=1", "."},
		},
		{
			name: "file shape with empty TestFilter derives parent package dirs from job.Files",
			job: &LandingJob{
				TestShape:  "file",
				TestFilter: "",
				Files:      []string{"daemon/drive_landing.go", "daemon/drive_landing_test.go", "effects/shapes.go"},
			},
			want: []string{"go", "test", "-json", "-count=1", "./daemon", "./effects"},
		},
		{
			name: "file shape with regex or test name in TestFilter passes -run and targets job.Files packages",
			job: &LandingJob{
				TestShape:  "file",
				TestFilter: "TestDriveLanding|TestLiveTask",
				Files:      []string{"daemon/drive_landing.go", "daemon/live_tasks.go"},
			},
			want: []string{"go", "test", "-json", "-count=1", "./daemon", "-run", "TestDriveLanding|TestLiveTask"},
		},
		{
			name: "file shape combining file paths in TestFilter and job.Files deduplicates packages",
			job: &LandingJob{
				TestShape:  "file",
				TestFilter: "daemon/drive_landing_test.go",
				Files:      []string{"daemon/drive_landing.go", "engine/engine.go"},
			},
			want: []string{"go", "test", "-json", "-count=1", "./daemon", "./engine"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLandingTestArgsChecked(tc.job, baseArgs)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDriveLandingBuildTestArgsCustomShapeAndSafetyRejection(t *testing.T) {
	baseArgs := []string{"go", "test", "./..."}

	validJob := &LandingJob{
		TestShape:  "custom",
		TestFilter: "-v -race -count=1 -timeout 2m -run TestLandingCustom",
	}
	got, err := buildLandingTestArgsChecked(validJob, baseArgs)
	if err != nil {
		t.Fatalf("unexpected error for valid custom args: %v", err)
	}
	want := []string{"go", "test", "./...", "-v", "-race", "-count=1", "-timeout", "2m", "-run", "TestLandingCustom"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	unsafeFilters := []string{
		"-exec /bin/sh",
		"-exec=/bin/sh",
		"--exec /bin/sh",
		"--exec=/bin/sh",
		"-EXEC=/bin/sh",
		"-toolexec /usr/bin/env",
		"-toolexec=/usr/bin/env",
		"--toolexec /usr/bin/env",
		"--toolexec=/usr/bin/env",
		"-test.exec=/bin/sh",
		"-test.toolexec=/bin/sh",
		"-v; echo pwned",
		"-v | cat",
		"-v && true",
		"-v `id`",
		"-v $HOME",
		"-v > /tmp/out",
		"-v < /etc/passwd",
		"-v $(id)",
	}

	for _, bad := range unsafeFilters {
		job := &LandingJob{TestShape: "custom", TestFilter: bad}
		if args, err := buildLandingTestArgsChecked(job, baseArgs); err == nil {
			t.Errorf("expected custom filter %q to be rejected, but got %v", bad, args)
		}
		if args := buildLandingTestArgs(job, baseArgs); args != nil {
			t.Errorf("expected buildLandingTestArgs to return nil for unsafe custom filter %q, got %v", bad, args)
		}
	}
}

func TestDriveLandingReceiptAndEnvelopeIntegration(t *testing.T) {
	env := envelope{
		Job: engine.Job{
			TaskID:        "task-shape-pkg",
			ApprovedScope: []string{"daemon/drive_landing.go"},
		},
		Admission: AdmitParams{
			ID:            "task-shape-pkg",
			DeclaredFiles: []string{"daemon/drive_landing.go", "daemon/drive_landing_test.go"},
			TestShape:     "file",
			TestFilter:    "TestDriveLanding",
		},
	}
	scope := liveTaskScope{
		Files:  []string{"daemon/drive_landing.go"},
		TestID: "TestFallback",
	}

	req := controller.LandRequest{
		Task: "task-shape-pkg",
		QA:   [][]string{{"go", "vet", "./..."}},
	}
	baseArgs := []string{"go", "test", "-json", "-count=1", "./..."}
	receipt, err := applyLandingTestShape(&req, env, scope, baseArgs)
	if err != nil {
		t.Fatalf("applyLandingTestShape failed: %v", err)
	}
	if receipt.TaskID != "task-shape-pkg" || receipt.DeclaredShape != "file" || receipt.ResolvedShape != "file" {
		t.Fatalf("unexpected receipt metadata: %+v", receipt)
	}
	wantEffective := []string{"go", "test", "-json", "-count=1", "./daemon", "-run", "TestDriveLanding"}
	if !slices.Equal(receipt.EffectiveArgs, wantEffective) {
		t.Fatalf("receipt.EffectiveArgs = %v, want %v", receipt.EffectiveArgs, wantEffective)
	}
	if receipt.CommandLine != "go test -json -count=1 ./daemon -run TestDriveLanding" {
		t.Fatalf("receipt.CommandLine = %q", receipt.CommandLine)
	}
	logLine := receipt.FormatLogLine()
	if !strings.Contains(logLine, "task=task-shape-pkg") || !strings.Contains(logLine, "test_shape=file") || !strings.Contains(logLine, "args=\"go test -json -count=1 ./daemon -run TestDriveLanding\"") {
		t.Fatalf("unexpected logLine: %s", logLine)
	}
	if len(req.QA) != 2 || !slices.Equal(req.QA[1], wantEffective) {
		t.Fatalf("req.QA was not updated with shaped test args: %v", req.QA)
	}

	// Verify logging on Daemon and engine.Job fallback extraction.
	logPath := filepath.Join(t.TempDir(), "daemon.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	d := &Daemon{log: logFile}
	if err := d.recordLandingTestReceipt(context.Background(), "landing-1", receipt, controlplane.Audit{Actor: "operator"}); err != nil {
		t.Fatalf("recordLandingTestReceipt failed: %v", err)
	}
	loggedBytes, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loggedBytes), "test_shape=file") {
		t.Fatalf("daemon log did not record landing test receipt: %q", string(loggedBytes))
	}

	engineJob := &engine.Job{
		TaskID:        "job-evidence-task",
		ApprovedScope: []string{"daemon/drive_landing.go"},
		Evidence: []workflow.Evidence{
			{
				Type: "RedReport",
				Fields: map[string]any{
					"test_shape":  "pkg",
					"test_filter": "daemon",
				},
			},
		},
	}
	fromJobArgs, err := buildLandingTestArgsChecked(&LandingJob{Job: engineJob}, []string{"go", "test", "./..."})
	if err != nil || !slices.Equal(fromJobArgs, []string{"go", "test", "./daemon"}) {
		t.Fatalf("buildLandingTestArgsChecked from engine.Job = %v, %v", fromJobArgs, err)
	}

	rawReceipt, err := json.Marshal(receipt)
	if err != nil || !strings.Contains(string(rawReceipt), `"resolved_test_shape":"file"`) {
		t.Fatalf("receipt JSON marshal unexpected: %s (%v)", rawReceipt, err)
	}
}

func TestDriveLandingWorkflowShapesAndAdmissionVocabulary(t *testing.T) {
	baseArgs := []string{"go", "test", "-json", "-count=1", "./..."}

	cases := []struct {
		name string
		job  *LandingJob
		want []string
	}{
		{
			name: "go-unit with regex filter runs across ./...",
			job: &LandingJob{
				TestShape:  "go-unit",
				TestFilter: "^TestUnit|TestOther$",
				Files:      []string{"daemon/drive_landing.go"},
			},
			want: []string{"go", "test", "-json", "-count=1", "./...", "-run", "^TestUnit|TestOther$"},
		},
		{
			name: "go-integration with regex filter targets job.Files packages and passes -run",
			job: &LandingJob{
				TestShape:  "go-integration",
				TestFilter: "^TestIntegration|TestLive$",
				Files:      []string{"daemon/drive_landing.go", "controller/land.go"},
			},
			want: []string{"go", "test", "-json", "-count=1", "./daemon", "./controller", "-run", "^TestIntegration|TestLive$"},
		},
		{
			name: "go-integration with regex filter and empty job.Files keeps ./... and passes -run",
			job: &LandingJob{
				TestShape:  "go-integration",
				TestFilter: "^TestIntegration$",
			},
			want: []string{"go", "test", "-json", "-count=1", "./...", "-run", "^TestIntegration$"},
		},
		{
			name: "go-integration with ./ package path in TestFilter targets package path",
			job: &LandingJob{
				TestShape:  "go-integration",
				TestFilter: "./daemon",
			},
			want: []string{"go", "test", "-json", "-count=1", "./daemon"},
		},
		{
			name: "script shape derives packages from job.Files and passes -run for regex",
			job: &LandingJob{
				TestShape:  "script",
				TestFilter: "^TestScript$",
				Files:      []string{"daemon/drive_landing.go"},
			},
			want: []string{"go", "test", "-json", "-count=1", "./daemon", "-run", "^TestScript$"},
		},
		{
			name: "runner shape passes -run when TestFilter is non-empty",
			job: &LandingJob{
				TestShape:  "runner",
				TestFilter: "^TestRunner$",
			},
			want: []string{"go", "test", "-json", "-count=1", "./...", "-run", "^TestRunner$"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := buildLandingTestArgsChecked(tc.job, baseArgs)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}

	for _, shape := range []string{"go-unit", "go-integration", "script", "runner", "run", "pkg", "file", "custom"} {
		filter := "^TestValid$"
		if shape == "custom" {
			filter = "TestValid"
		}
		err := validateStructuredFields(AdmitParams{
			DeclaredFiles: []string{"daemon/drive_landing.go"},
			TestFilter:    filter,
			TestShape:     shape,
		})
		if err != nil {
			t.Fatalf("validateStructuredFields rejected valid shape %q: %v", shape, err)
		}
	}
}

func TestDriveLandingLiveDaemonTaskAndLandingReceipts(t *testing.T) {
	cfg, ctx, _ := liveDaemonFixture(t)
	_ = independentTask(t, cfg, "shape-integration")
	d, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { stop(t, d) }()
	d.live.guard.observe = func(context.Context) (hostguard.Sample, error) { return hostSample(1, 0.5), nil }
	authorize(t, d)

	doc := defaults.Workflow()
	for id, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && stage.TestRun {
			stage.Command = cfg.Live.AllowedCommands[0]
			doc.Stages[id] = stage
		}
	}
	if _, err = call(t, d, "WorkflowInstall", InstallParams{ID: doc.ID, Document: doc, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}

	target := TargetConfig{
		SchemaVersion:     4,
		Workspace:         cfg.Workspace,
		IntegrationRef:    cfg.Live.IntegrationRef,
		ProtectedBranches: []string{cfg.Live.IntegrationRef},
		AllowedCommands:   [][]string{{"gofmt", "-l", "."}},
		Landing:           &LandingPolicy{Flakes: []string{"TestNeverRuns"}, Frozen: [][]string{{"gofmt", "-l", "."}}},
	}
	for _, stage := range doc.Stages {
		if stage.Primitive == workflow.HostCommand && !slices.ContainsFunc(target.AllowedCommands, func(v []string) bool { return slices.Equal(v, stage.Command) }) {
			target.AllowedCommands = append(target.AllowedCommands, stage.Command)
		}
	}
	if _, err = call(t, d, "TargetConfigPut", TargetPutParams{ID: "shape-target", Document: target, AuthorizingMessage: "message"}, ""); err != nil {
		t.Fatal(err)
	}

	if _, err = call(t, d, "Admit", AdmitParams{
		ID:                 "shape-unit",
		TargetConfig:       "shape-target",
		TestShape:          "go-unit",
		TestFilter:         "^TestBehavior$",
		AuthorizingMessage: "message",
	}, "shape-unit"); err != nil {
		t.Fatal(err)
	}
	if _, err = call(t, d, "Admit", AdmitParams{
		ID:                 "shape-integration",
		TargetConfig:       "shape-target",
		TestShape:          "go-integration",
		TestFilter:         "^TestBehavior$",
		AuthorizingMessage: "message",
	}, "shape-integration"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Minute)
	for {
		_, errUnit := d.store.GetMarker(ctx, "landing-test-receipt-shape-unit")
		_, errInt := d.store.GetMarker(ctx, "landing-test-receipt-shape-integration")
		if errUnit == nil && errInt == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for live HostCommand test shape receipts (unit=%v, integration=%v)", errUnit, errInt)
		}
		time.Sleep(50 * time.Millisecond)
	}
	stopTheDrive(t, d)

	for _, tc := range []struct {
		id       string
		declared string
		resolved string
	}{
		{id: "shape-unit", declared: "go-unit", resolved: "run"},
		{id: "shape-integration", declared: "go-integration", resolved: "pkg"},
	} {
		raw, err := d.store.GetMarker(ctx, "landing-test-receipt-"+tc.id)
		if err != nil {
			t.Fatalf("missing live test shape receipt marker for %s: %v", tc.id, err)
		}
		var receipt LandingCheckReceipt
		if err := json.Unmarshal(raw, &receipt); err != nil {
			t.Fatalf("unmarshal receipt for %s: %v", tc.id, err)
		}
		if receipt.TaskID != tc.id || receipt.DeclaredShape != tc.declared || receipt.ResolvedShape != tc.resolved {
			t.Fatalf("unexpected receipt for %s: %+v", tc.id, receipt)
		}

		d.mu.Lock()
		snapshot := d.jobs[tc.id]
		var env envelope
		if err = decodeEnvelope(snapshot.Payload, &env); err != nil {
			d.mu.Unlock()
			t.Fatal(err)
		}
		fields := map[string]any{"sha": cfg.Live.Base, "files": []any{"answer.go"}}
		rawFields, _ := json.Marshal(fields)
		identity, _ := domain.ContentHash(rawFields)
		env.Job.InFlight = nil
		env.Job.History = append(env.Job.History, engine.Event{Kind: "transition", Stage: string(env.Job.State), From: env.Job.State, To: domain.Done, Attempt: 1, Cause: "fixture"})
		env.Job.State = domain.Done
		env.Job.Evidence = append(env.Job.Evidence, workflow.Evidence{
			ID:         identity,
			Type:       "Commit",
			Version:    env.Job.Document.Artifacts["Commit"].Version,
			Stage:      "commit",
			TaskID:     tc.id,
			Tree:       env.Job.Tree,
			Session:    "fixture",
			Role:       "host",
			Provenance: true,
			Fields:     fields,
		})
		env.Job.Provenance = append(env.Job.Provenance, engine.Provenance{
			ArtifactID:    identity,
			Stage:         "commit",
			Primitive:     workflow.VcsCommit,
			ExecutionKind: "live",
			TaskID:        tc.id,
			Tree:          env.Job.Tree,
			Session:       "fixture",
			Role:          "host",
			DispatchID:    "fixture",
			ProducedAt:    time.Now().UTC(),
		})
		snapshot.Payload, err = json.Marshal(env)
		if err != nil {
			d.mu.Unlock()
			t.Fatal(err)
		}
		snapshot.State = "Done"
		snapshot.Events = nil
		saved, saveErr := d.store.SaveJob(ctx, controlplane.Audit{Actor: "test", Action: "mark done for landing request", Outcome: "ok"}, snapshot, snapshot.Version)
		if saveErr == nil {
			d.jobs[tc.id] = saved
		}
		d.mu.Unlock()
		if saveErr != nil {
			t.Fatal(saveErr)
		}

		branch := liveGit(t, cfg.Workspace, "symbolic-ref", "--short", "HEAD")
		landingID := "landing-" + tc.id
		if _, _, err := d.landingRequest(ctx, landingID, 0, tc.id, branch, "refs/fae/gated/"+branch, LandParams{
			Tasks:              []string{tc.id},
			Target:             branch,
			AuthorizingMessage: "message",
		}); err != nil {
			t.Fatalf("landingRequest for %s failed: %v", tc.id, err)
		}
		rawLandingMarker, err := d.store.GetMarker(ctx, landingTestReceiptKey(landingID, tc.id))
		if err != nil {
			t.Fatalf("missing landingTestReceiptKey marker after landingRequest for %s: %v", tc.id, err)
		}
		var landReceipt LandingCheckReceipt
		if err := json.Unmarshal(rawLandingMarker, &landReceipt); err != nil {
			t.Fatal(err)
		}
		if landReceipt.TaskID != tc.id || landReceipt.DeclaredShape != tc.declared || landReceipt.ResolvedShape != tc.resolved {
			t.Fatalf("unexpected landing receipt after landingRequest for %s: %+v", tc.id, landReceipt)
		}
	}
}
