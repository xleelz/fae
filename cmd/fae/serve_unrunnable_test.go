package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// FAE-WF-093: `serve --quarantine-unrunnable-revisions` is the one-start
// recovery the daemon's refusal names: it reaches the daemon's configuration
// and is not recorded for `run --start`, which would otherwise quarantine
// silently at every restart. Before, the flag did not exist.
func TestServeQuarantineUnrunnableRevisionsFlagIsOneShot(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "instances")
	config, _, _ := stubServe(t)
	serve := func(args ...string) error {
		return execute(context.Background(), append([]string{"--home", home, "serve", "--workspace", root, "--adapter", "scripted", "--foreground"}, args...), &bytes.Buffer{})
	}
	if err := serve("--quarantine-unrunnable-revisions"); !strings.Contains(fmt.Sprint(err), "stub start") || !config.QuarantineUnrunnableRevisions {
		t.Fatalf("flag: %v, configured %t", err, config.QuarantineUnrunnableRevisions)
	}
	if err := serve(); !strings.Contains(fmt.Sprint(err), "stub start") || config.QuarantineUnrunnableRevisions {
		t.Fatalf("without the flag: %v, configured %t", err, config.QuarantineUnrunnableRevisions)
	}
	for _, args := range [][]string{
		{"--workspace", root, "--quarantine-unrunnable-revisions", "--adapter", "scripted"},
		{"--workspace", root, "--quarantine-unrunnable-revisions=true", "--adapter", "scripted"},
		{"-quarantine-unrunnable-revisions", "--workspace", root, "--adapter", "scripted"},
	} {
		if got := fmt.Sprint(recordedServeArgs(args)); got != fmt.Sprint([]string{"--workspace", root, "--adapter", "scripted"}) {
			t.Errorf("recorded %v as %s", args, got)
		}
	}
	var out bytes.Buffer
	if err := execute(context.Background(), []string{"serve", "--help", "--json"}, &out); err != nil || !strings.Contains(out.String(), `"name": "quarantine-unrunnable-revisions"`) {
		t.Fatalf("serve help %v %s", err, out.String())
	}
}

// FAE-OPS-011: `fae topology --include-retired` is the read's retired opt-in,
// named as Status, Board, History and Events name theirs.
func TestTopologyIncludeRetiredCLI(t *testing.T) {
	home, requests := operatorFake(t)
	checkThinRPCs(t, home, requests, []thinCase{
		{[]string{"topology"}, "Topology", map[string]any{"workflow_id": "", "after": "", "limit": float64(100), "include_retired": false}},
		{[]string{"topology", "--include-retired", "--workflow", "w"}, "Topology", map[string]any{"workflow_id": "w", "after": "", "limit": float64(100), "include_retired": true}},
	})
}
