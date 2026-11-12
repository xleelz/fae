package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/api"
)

func TestNotifyAndFollowUpCLIFlagsAndPayloads(t *testing.T) {
	home := shortHome(t)
	c := captureDaemon(t, home)
	run := func(args ...string) api.Request {
		t.Helper()
		var out bytes.Buffer
		if err := execute(context.Background(), append([]string{"--home", home, "--instance", "conf"}, args...), &out); err != nil {
			t.Fatal(args, err)
		}
		return c.last(t)
	}
	params := func(r api.Request) map[string]any {
		t.Helper()
		var p map[string]any
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	// 1. fae notify with --from-task, --delivery in-flight, --actor, --reason.
	req := run("notify", "--task", "task-target", "--kind", "breaking-change", "--body", "API changed", "--from-task", "task-src", "--delivery", "in-flight", "--actor", "operator", "--reason", "contract update")
	if req.Method != "Notify" {
		t.Fatalf("expected Notify method, got %s", req.Method)
	}
	np := params(req)
	if np["task"] != "task-target" || np["kind"] != "breaking-change" || np["body"] != "API changed" || np["from_task"] != "task-src" || np["delivery"] != "in-flight" || np["actor"] != "operator" || np["reason"] != "contract update" {
		t.Fatalf("unexpected Notify params: %#v", np)
	}

	// 2. fae follow-up with --wall-seconds, --max-turns, --actor, --reason, --idempotence-key.
	req2 := run("follow-up", "--task", "task-done-1", "--id", "task-fu-1", "--text", "Fix edge case", "--wall-seconds", "120", "--max-turns", "3", "--actor", "operator", "--reason", "post-done polish", "--idempotence-key", "fu-key-1")
	if req2.Method != "FollowUp" || req2.Key != "fu-key-1" {
		t.Fatalf("expected FollowUp method with key fu-key-1, got method=%s key=%s", req2.Method, req2.Key)
	}
	fp := params(req2)
	if fp["task"] != "task-done-1" || fp["id"] != "task-fu-1" || fp["text"] != "Fix edge case" || fp["actor"] != "operator" || fp["reason"] != "post-done polish" {
		t.Fatalf("unexpected FollowUp params: %#v", fp)
	}
	budget, ok := fp["budget"].(map[string]any)
	if !ok || budget["wall_seconds"] != float64(120) || budget["max_turns"] != float64(3) {
		t.Fatalf("unexpected FollowUp budget: %#v", fp["budget"])
	}
}
