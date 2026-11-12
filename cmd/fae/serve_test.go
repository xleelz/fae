package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/xleelz/fae/daemon"
)

func TestServeDoltRestartAttemptsFlag(t *testing.T) {
	home := shortHome(t)
	sentinel := errors.New("captured config")
	var captured []daemon.Config
	old := startDaemon
	startDaemon = func(_ context.Context, cfg daemon.Config) (*daemon.Daemon, error) {
		captured = append(captured, cfg)
		return nil, sentinel
	}
	t.Cleanup(func() { startDaemon = old })

	// Default (omitted): DoltRestartAttempts is 0 (daemon.Config defaults to 3).
	var out bytes.Buffer
	err := execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted",
	}, &out)
	if !errors.Is(err, sentinel) {
		t.Fatalf("default serve err = %v, want sentinel", err)
	}
	if len(captured) != 1 || captured[0].DoltRestartAttempts != 0 {
		t.Fatalf("default DoltRestartAttempts = %+v, want 0", captured)
	}

	// Explicit positive value: wired to cfg.DoltRestartAttempts.
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted", "--dolt-restart-attempts", "5",
	}, &out)
	if !errors.Is(err, sentinel) {
		t.Fatalf("explicit serve err = %v, want sentinel", err)
	}
	if len(captured) != 2 || captured[1].DoltRestartAttempts != 5 {
		t.Fatalf("explicit DoltRestartAttempts = %d, want 5", captured[len(captured)-1].DoltRestartAttempts)
	}

	// Negative value: usage exit code 2 without invoking startDaemon.
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted", "--dolt-restart-attempts", "-1",
	}, &out)
	if exitCode(err) != 2 {
		t.Fatalf("negative --dolt-restart-attempts exitCode = %d (%v), want 2", exitCode(err), err)
	}
	if len(captured) != 2 {
		t.Fatalf("negative --dolt-restart-attempts called startDaemon (%d calls)", len(captured))
	}

	// Non-integer value: usage exit code 2.
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted", "--dolt-restart-attempts", "bogus",
	}, &out)
	if exitCode(err) != 2 {
		t.Fatalf("non-integer --dolt-restart-attempts exitCode = %d (%v), want 2", exitCode(err), err)
	}

	// RelaunchArgs / recordedServeArgs preserves --dolt-restart-attempts across restarts.
	relaunch := recordedServeArgs([]string{"--workspace", home, "--adapter", "scripted", "--dolt-restart-attempts", "5", "--quarantine-unrunnable-revisions"})
	if !slices.Contains(relaunch, "--dolt-restart-attempts") || !slices.Contains(relaunch, "5") {
		t.Fatalf("recordedServeArgs = %v, want --dolt-restart-attempts 5 preserved", relaunch)
	}
	if slices.Contains(relaunch, "--quarantine-unrunnable-revisions") {
		t.Fatalf("recordedServeArgs = %v, want one-shot --quarantine-unrunnable-revisions stripped", relaunch)
	}
}

func TestServeTUIFlagsDefaultAndOverrides(t *testing.T) {
	home := shortHome(t)
	sentinel := errors.New("captured tui config")
	var captured []daemon.Config
	old := startDaemon
	startDaemon = func(_ context.Context, cfg daemon.Config) (*daemon.Daemon, error) {
		captured = append(captured, cfg)
		return nil, sentinel
	}
	t.Cleanup(func() { startDaemon = old })

	// Default (omitted): TUI is enabled by default with 2s refresh and read-only.
	var out bytes.Buffer
	err := execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted",
	}, &out)
	if !errors.Is(err, sentinel) {
		t.Fatalf("default serve err = %v, want sentinel", err)
	}
	if len(captured) != 1 || captured[0].TUI == nil || captured[0].TUI.AllowWrites || captured[0].TUI.Refresh != 2*time.Second {
		t.Fatalf("default TUI = %+v, want non-nil default TUIConfig", captured[0].TUI)
	}

	// Explicit --tui=false disables TUI supervision.
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted", "--tui=false",
	}, &out)
	if !errors.Is(err, sentinel) {
		t.Fatalf("--tui=false serve err = %v, want sentinel", err)
	}
	if captured[len(captured)-1].TUI != nil {
		t.Fatalf("--tui=false TUI = %+v, want nil", captured[len(captured)-1].TUI)
	}

	// Subflags with --tui=false are rejected with usage exit 2.
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted", "--tui=false", "--tui-allow-writes",
	}, &out)
	if exitCode(err) != 2 {
		t.Fatalf("--tui=false --tui-allow-writes exitCode = %d (%v), want 2", exitCode(err), err)
	}

	// Out-of-range --tui-refresh is rejected with usage exit 2.
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted", "--tui-refresh", "50ms",
	}, &out)
	if exitCode(err) != 2 {
		t.Fatalf("--tui-refresh 50ms exitCode = %d (%v), want 2", exitCode(err), err)
	}

	// Explicit --tui-bin, --tui-allow-writes, --tui-refresh.
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted", "--tui-bin", "/bin/true", "--tui-allow-writes", "--tui-refresh", "500ms",
	}, &out)
	if !errors.Is(err, sentinel) {
		t.Fatalf("explicit TUI serve err = %v, want sentinel", err)
	}
	got := captured[len(captured)-1].TUI
	if got == nil || got.Binary != "/bin/true" || !got.AllowWrites || got.Refresh != 500*time.Millisecond {
		t.Fatalf("explicit TUI = %+v, want Binary=/bin/true AllowWrites=true Refresh=500ms", got)
	}
}

func TestServeWorkflowMDAndSymphonyHTTPAddrFlags(t *testing.T) {
	home := shortHome(t)
	sentinel := errors.New("captured symphony config")
	var captured []daemon.Config
	old := startDaemon
	startDaemon = func(_ context.Context, cfg daemon.Config) (*daemon.Daemon, error) {
		captured = append(captured, cfg)
		return nil, sentinel
	}
	t.Cleanup(func() { startDaemon = old })

	validWF := filepath.Join(home, "WORKFLOW.md")
	wfBody := "---\ntracker:\n  kind: memory\nfae:\n  workflow_id: default\n  target_config: linear-target\n  authorizing_message: linear-authority\npolling:\n  interval_ms: 15000\nserver:\n  port: 0\n---\nResolve {{ issue.identifier }}\n"
	if err := os.WriteFile(validWF, []byte(wfBody), 0600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	// 1. --symphony-http-addr without --workflow-md is rejected with usage exit 2.
	err := execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted", "--symphony-http-addr", "127.0.0.1:0",
	}, &out)
	if exitCode(err) != 2 {
		t.Fatalf("--symphony-http-addr without --workflow-md exitCode = %d (%v), want 2", exitCode(err), err)
	}

	// 2. Malformed --symphony-http-addr is rejected with usage exit 2.
	for _, badAddr := range []string{"not-host-port", "127.0.0.1:99999", "127.0.0.1:-1"} {
		err = execute(context.Background(), []string{
			"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted",
			"--workflow-md", validWF, "--symphony-http-addr", badAddr,
		}, &out)
		if exitCode(err) != 2 {
			t.Fatalf("--symphony-http-addr %q exitCode = %d (%v), want 2", badAddr, exitCode(err), err)
		}
	}

	// 3. Missing WORKFLOW.md is rejected with exit 1 before starting daemon.
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted",
		"--workflow-md", filepath.Join(home, "MISSING_WORKFLOW.md"),
	}, &out)
	if exitCode(err) != 1 {
		t.Fatalf("missing --workflow-md exitCode = %d (%v), want 1", exitCode(err), err)
	}

	// 4. Invalid WORKFLOW.md (missing tracker.kind) is rejected with exit 1.
	badWF := filepath.Join(home, "BAD_WORKFLOW.md")
	if err := os.WriteFile(badWF, []byte("---\npolling:\n  interval_ms: 1000\n---\nPrompt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted",
		"--workflow-md", badWF,
	}, &out)
	if exitCode(err) != 1 {
		t.Fatalf("invalid --workflow-md exitCode = %d (%v), want 1", exitCode(err), err)
	}

	// 5. Valid --workflow-md and --symphony-http-addr wire SymphonyConfig alongside TUI.
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted",
		"--workflow-md", validWF, "--symphony-http-addr", "127.0.0.1:43210",
	}, &out)
	if !errors.Is(err, sentinel) {
		t.Fatalf("valid --workflow-md serve err = %v, want sentinel", err)
	}
	cfg := captured[len(captured)-1]
	if cfg.Symphony == nil || cfg.WorkflowMD != validWF || cfg.SymphonyHTTPAddr != "127.0.0.1:43210" {
		t.Fatalf("captured Symphony = %+v (WorkflowMD=%q SymphonyHTTPAddr=%q)", cfg.Symphony, cfg.WorkflowMD, cfg.SymphonyHTTPAddr)
	}
	if cfg.Symphony.ServiceConfig.Server.Host != "127.0.0.1" || cfg.Symphony.ServiceConfig.Server.Port == nil || *cfg.Symphony.ServiceConfig.Server.Port != 43210 {
		t.Fatalf("captured Server = %+v, want 127.0.0.1:43210", cfg.Symphony.ServiceConfig.Server)
	}
	if cfg.TUI == nil {
		t.Fatalf("expected supervised TUI config alongside Symphony, got nil")
	}

	// 6. --workflow alias works and kind: linear wires the real *tracker.LinearAdapter.
	linearWF := filepath.Join(home, "LINEAR_WORKFLOW.md")
	linearBody := "---\ntracker:\n  kind: linear\n  api_key: lin_test_token\n  project_slug: fae-proj\nfae:\n  workflow_id: default\n  target_config: linear-target\n  authorizing_message: linear-authority\npolling:\n  interval_ms: 15000\n---\nResolve {{ issue.identifier }}\n"
	if err := os.WriteFile(linearWF, []byte(linearBody), 0600); err != nil {
		t.Fatal(err)
	}
	err = execute(context.Background(), []string{
		"serve", "--home", home, "--workspace", home, "--foreground", "--adapter", "scripted",
		"--workflow", linearWF,
	}, &out)
	if !errors.Is(err, sentinel) {
		t.Fatalf("valid --workflow linear serve err = %v, want sentinel", err)
	}
	linCfg := captured[len(captured)-1]
	if linCfg.Symphony == nil || fmt.Sprintf("%T", linCfg.Symphony.Adapter) != "*tracker.LinearAdapter" {
		t.Fatalf("expected *tracker.LinearAdapter for kind: linear, got %T", linCfg.Symphony.Adapter)
	}
}
