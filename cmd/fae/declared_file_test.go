package main

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/xleelz/fae/daemon"
)

// --declared-file is repeatable on run and start-work-item and fills the
// admission's declared files, from which a live instance without pinned
// files takes the task's scope; a path admission would refuse (the one
// declared-path rule: never in .git at any depth) is a usage error.
func TestDeclaredFileSubmissionFlag(t *testing.T) {
	home, requests := recordingDaemon(t, "decl", map[string]any{"admitted": 1, "updated": 0, "refused": 0})
	submit := func(args ...string) error {
		return execute(context.Background(), append(args, "--home", home, "--instance", "decl", "--authorizing-message", "m"), &bytes.Buffer{})
	}
	if err := submit("run", "--prompt", "x", "--id", "t1", "--declared-file", "pkg/a.go", "--declared-file", "pkg/a_test.go"); err != nil {
		t.Fatal(err)
	}
	var admit daemon.AdmitParams
	if r := <-requests; r.Method != "Admit" || json.Unmarshal(r.Params, &admit) != nil || !slices.Equal(admit.DeclaredFiles, []string{"pkg/a.go", "pkg/a_test.go"}) {
		t.Fatalf("run declared files %s", r.Params)
	}
	if err := submit("start-work-item", "--id", "item", "--content", "x", "--declared-file", "b.go"); err != nil {
		t.Fatal(err)
	}
	var backlog daemon.BacklogParams
	if r := <-requests; r.Method != "BacklogAdmit" || json.Unmarshal(r.Params, &backlog) != nil || len(backlog.Items) != 1 || !slices.Equal(backlog.Items[0].DeclaredFiles, []string{"b.go"}) {
		t.Fatalf("start-work-item declared files %s", r.Params)
	}
	if err := submit("run", "--prompt", "x", "--id", "t2"); err != nil {
		t.Fatal(err)
	}
	if r := <-requests; params(t, r)["declared_files"] != nil {
		t.Fatalf("no declared file sent declared files %s", r.Params)
	}
	for _, bad := range []string{"", "/abs.go", "../up.go", "a/../b.go", "./a.go", ".", "..", ".git", ".git/config", "sub/.git/config", "sub/.git", ".GIT/config", "a//b.go", "a\x00.go"} {
		if err := submit("run", "--prompt", "x", "--declared-file", bad); exitCode(err) != 2 {
			t.Errorf("declared file %q: %v", bad, err)
		}
	}
	if err := submit("run", "--prompt", "x", "--declared-file", "a.go", "--declared-file", "a.go"); exitCode(err) != 2 {
		t.Error("repeated declared file accepted", err)
	}
}
