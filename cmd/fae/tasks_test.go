package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/xleelz/fae/api"
	"github.com/xleelz/fae/daemon"
)

// capturedRequests is a fake daemon that records each request's method, key
// and params and answers {"ok": true}.
type capturedRequests struct {
	mu   sync.Mutex
	reqs []api.Request
}

func (c *capturedRequests) last(t *testing.T) api.Request {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reqs) == 0 {
		t.Fatal("no request reached the daemon")
	}
	r := c.reqs[len(c.reqs)-1]
	c.reqs = nil
	return r
}

func captureDaemon(t *testing.T, home string) *capturedRequests {
	t.Helper()
	layout, err := daemon.Paths(home, "conf")
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
	c := &capturedRequests{}
	server, err := api.New(token, func(_ context.Context, r api.Request) (any, error) {
		c.mu.Lock()
		c.reqs = append(c.reqs, r)
		c.mu.Unlock()
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
	return c
}

// `fae task` sends exactly the fields its flags name: create builds its edges
// from --depends-on, --blocks and --relates-to; update sends only the flags
// given (a cleared declared-file list as []); delete and show send their
// parameters; --file sends the JSON as given; --idempotence-key is the
// request key. Nothing in them names a language.
func TestTaskVerbsSendTheFieldsTheirFlagsName(t *testing.T) {
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
		var p map[string]any
		if err := json.Unmarshal(r.Params, &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	r := run("task", "create", "--id", "n", "--title", "New", "--content", "describe it", "--priority", "20", "--depends-on", "a", "--blocks", "b", "--relates-to", "c", "--declared-file", "docs/x.md", "--test-filter", "^x", "--idempotence-key", "k1", "--authorizing-message", "m")
	p := params(r)
	edges, _ := json.Marshal(p["edges"])
	if r.Method != "TaskCreate" || r.Key != "k1" || p["id"] != "n" || p["title"] != "New" || p["content"] != "describe it" || p["priority"] != 20.0 || p["authorizing_message"] != "m" || p["test_filter"] != "^x" || !reflect.DeepEqual(p["declared_files"], []any{"docs/x.md"}) ||
		string(edges) != `[{"from":"a","kind":"blocking","to":"n"},{"from":"n","kind":"blocking","to":"b"},{"from":"n","kind":"relates","to":"c"}]` {
		t.Fatalf("create %s %s %v", r.Method, r.Key, p)
	}
	// An omitted priority is not sent (the daemon takes 10), so 0 is never
	// sent by mistake (FAE-CLI-002).
	r = run("task", "create", "--id", "n", "--content", "describe it", "--authorizing-message", "m")
	if p = params(r); r.Method != "TaskCreate" || p["priority"] != nil {
		t.Fatalf("create without --priority sent %v", p["priority"])
	}
	r = run("task", "update", "--id", "n", "--priority", "30", "--clear-declared-files", "--authorizing-message", "m")
	if p = params(r); r.Method != "TaskUpdate" || r.Key != "" || !reflect.DeepEqual(p, map[string]any{"id": "n", "priority": 30.0, "declared_files": []any{}, "authorizing_message": "m"}) {
		t.Fatalf("update %s %v", r.Method, p)
	}
	content := filepath.Join(t.TempDir(), "content.md")
	if err := os.WriteFile(content, []byte("from a file"), 0600); err != nil {
		t.Fatal(err)
	}
	r = run("task", "update", "--id", "n", "--title", "", "--content-file", content, "--authorizing-message", "m")
	if p = params(r); !reflect.DeepEqual(p, map[string]any{"id": "n", "title": "", "content": "from a file", "authorizing_message": "m"}) {
		t.Fatalf("update with an empty title and a content file %v", p)
	}
	r = run("task", "delete", "--id", "n", "--reason", "replaced", "--superseded-by", "s", "--authorizing-message", "m")
	if p = params(r); r.Method != "TaskDelete" || !reflect.DeepEqual(p, map[string]any{"id": "n", "reason": "replaced", "superseded_by": "s", "authorizing_message": "m"}) {
		t.Fatalf("delete %s %v", r.Method, p)
	}
	r = run("task", "show", "--id", "n")
	if p = params(r); r.Method != "TaskGet" || !reflect.DeepEqual(p, map[string]any{"id": "n"}) {
		t.Fatalf("show %s %v", r.Method, p)
	}
	file := filepath.Join(t.TempDir(), "update.json")
	if err := os.WriteFile(file, []byte(`{"id":"n","test_shape":"script","expected_version":7}`), 0600); err != nil {
		t.Fatal(err)
	}
	r = run("task", "update", "--file", file, "--authorizing-message", "m")
	if p = params(r); !reflect.DeepEqual(p, map[string]any{"id": "n", "test_shape": "script", "expected_version": 7.0, "authorizing_message": "m"}) {
		t.Fatalf("update --file %v", p)
	}
	// Usage errors exit 2 before the daemon is dialled; a priority below 10
	// given as a flag is one (FAE-CLI-002), while invalid content in a named
	// --file is a validation failure (exit 1).
	zero := filepath.Join(t.TempDir(), "zero.json")
	if err := os.WriteFile(zero, []byte(`{"id":"n","content":"x","priority":0}`), 0600); err != nil {
		t.Fatal(err)
	}
	nine := filepath.Join(t.TempDir(), "nine.json")
	if err := os.WriteFile(nine, []byte(`{"id":"n","priority":9}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"task", "create", "--id", "n"}, {"task", "update", "--file", file, "--title", "x"}, {"task", "update", "--id", "n", "--clear-declared-files", "--declared-file", "a.md"}, {"task", "delete", "--id", "n"}, {"task", "show"}, {"task", "create", "--id", "n", "--content", "x", "--content-file", content},
		{"task", "create", "--id", "n", "--content", "x", "--priority", "0"}, {"task", "create", "--id", "n", "--content", "x", "--priority", "9"}, {"task", "update", "--id", "n", "--priority", "0"},
		{"task", "create", "--id", "n", "--content", "x", "--mode", "normal"}, {"task", "create", "--id", "n", "--content", "x", "--mode", "local", "--commit-external"}, {"task", "create", "--id", "n", "--content", "x", "--dry-run", "--commit-external"}, {"task", "create", "--id", "n", "--content", "x", "--revision", "-1"}, {"task", "create", "--id", "n", "--content", "x", "--target-revision", "-1"}} {
		err := execute(context.Background(), append([]string{"--home", home, "--instance", "conf"}, append(args, "--authorizing-message", "m")...), &bytes.Buffer{})
		if exitCode(err) != 2 {
			t.Errorf("%v: exit %d (%v), want 2", args, exitCode(err), err)
		}
	}
	for _, args := range [][]string{{"task", "create", "--file", zero}, {"task", "update", "--file", nine}} {
		err := execute(context.Background(), append([]string{"--home", home, "--instance", "conf"}, append(args, "--authorizing-message", "m")...), &bytes.Buffer{})
		if exitCode(err) != 1 {
			t.Errorf("%v: exit %d (%v), want 1", args, exitCode(err), err)
		}
	}
	c.mu.Lock()
	if len(c.reqs) != 0 {
		t.Error("a usage error reached the daemon", c.reqs)
	}
	c.mu.Unlock()
	for _, word := range []string{"go test", "_test.go", "golang"} {
		if strings.Contains(strings.ToLower(taskUsage), word) {
			t.Errorf("task usage names %q", word)
		}
	}
}
