package runtime

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func herdrArgsFixture(args map[int]string) func(int) ([]string, error) {
	return func(pid int) ([]string, error) {
		a, ok := args[pid]
		if !ok {
			return nil, errors.New("no such process")
		}
		return strings.Fields(a), nil
	}
}
func TestHerdrInventoryParsing(t *testing.T) {
	server := herdrArgsFixture(map[int]string{123: "/bin/herdr server", 168797: "herdr server", 9: "herdr server", 12: "herdr server"})
	paths, e := herdrSockets([]byte("p123\nf4\nn/private/tmp/mux.sock\nf5\nn/private/tmp/mux-client.sock\nf6\nn->0xabc\n"), server)
	if e != nil || len(paths) != 1 || paths[0] != "/private/tmp/mux.sock" {
		t.Fatal(paths, e)
	}
	// Linux lsof output: unnamed sockets, type suffixes and listen state.
	paths, e = herdrSockets([]byte("p168797\nntype=STREAM\nn/tmp/a/mux.sock type=STREAM\nntype=STREAM\nn/tmp/a/mux-client.sock type=STREAM\np9\nn/tmp/b/api.sock type=STREAM (LISTEN)\n"), server)
	sort.Strings(paths)
	if e != nil || strings.Join(paths, ",") != "/tmp/a/mux.sock,/tmp/b/api.sock" {
		t.Fatal(paths, e)
	}
	for _, s := range []string{"p123\nn->0xabc\n", "pwat\n", "p12\nn/tmp/../socket\n"} {
		if _, e := herdrSockets([]byte(s), server); e == nil {
			t.Fatal(s)
		}
	}
	// The operator's remote attach: a socketless bridge client spawned the
	// server, which alone owns the API socket. Clients host no panes.
	operator := "p695589\nntype=STREAM\nntype=STREAM\np695590\nn/home/u/.config/herdr/herdr.sock type=STREAM\nntype=STREAM\nn/home/u/.config/herdr/herdr-client.sock type=STREAM\nn/home/u/.config/herdr/herdr-client.sock type=STREAM\n"
	paths, e = herdrSockets([]byte(operator), herdrArgsFixture(map[int]string{695589: "/home/u/.local/bin/herdr remote-client-bridge", 695590: "/home/u/.local/bin/herdr server"}))
	if e != nil || strings.Join(paths, ",") != "/home/u/.config/herdr/herdr.sock" {
		t.Fatal(paths, e)
	}
	for _, client := range []string{
		"herdr", "herdr --session work", "herdr --remote host", "herdr pane list",
		"herdr --session work workspace create --label x", "herdr server stop",
		"herdr server reload-config", "herdr server live-handoff --import-exe /x",
		"herdr-core", "herdr-core ls --json", "herdr-core run --bg --name x --json",
		"herdr-core get t1 --json", "herdr-core kill t1 --json", "herdr-core rm t1 --json",
		"herdr-core server stop", "herdr-core server status", "herdr-core connect",
	} {
		paths, e = herdrSockets([]byte("p7\nntype=STREAM\np8\nn/tmp/s/api.sock type=STREAM\n"), herdrArgsFixture(map[int]string{7: client, 8: "herdr-core serve"}))
		if e != nil || strings.Join(paths, ",") != "/tmp/s/api.sock" {
			t.Fatal(client, paths, e)
		}
	}
	// herdr-core holder process with its *.holder.*.sock socket is tolerated and excluded from API sockets.
	paths, e = herdrSockets([]byte("p7\nn/tmp/s/mux.holder.1234.sock type=STREAM\np8\nn/tmp/s/mux.sock type=STREAM\n"), herdrArgsFixture(map[int]string{7: "herdr-core hold --socket /tmp/s/mux.holder.1234.sock", 8: "herdr-core serve"}))
	if e != nil || strings.Join(paths, ",") != "/tmp/s/mux.sock" {
		t.Fatal(paths, e)
	}
	// A server, holder without socket, an unrecognised role or an unreadable command line without an
	// API socket may host uncounted panes: the census fails closed.
	for _, role := range []string{"herdr server", "herdr-core serve", "herdr-core hold --socket /tmp/s/mux.holder.1234.sock", "/x/herdr --session work server", "herdr server --handoff-import /x", "herdr frobnicate", "herdr-core frobnicate", "herdr server frobnicate", "not-herdr ls", ""} {
		args := map[int]string{8: "herdr-core serve"}
		if role != "" {
			args[7] = role
		}
		if paths, e = herdrSockets([]byte("p7\nntype=STREAM\nn/tmp/s/api-client.sock type=STREAM\np8\nn/tmp/s/api.sock type=STREAM\n"), herdrArgsFixture(args)); e == nil {
			t.Fatal(role, paths)
		}
	}
	if args, e := herdrProcessArgs(context.Background(), os.Getpid()); e != nil || len(args) == 0 || args[0] != os.Args[0] {
		t.Fatal(args, e)
	}
	if _, e := herdrProcessArgs(context.Background(), 1<<30); e == nil {
		t.Fatal("vanished process classified")
	}
	for _, s := range []string{`{}`, `{"result":{"type":"pane_list"}}`, `{"result":{"type":"pane_list","panes":[{}]}}`, `{"result":{"type":"pane_list","panes":[{"pane_id":"x"},{"pane_id":"x"}]}}`, `{"terminals":[{}]}`, `{"terminals":[{"address":"t1"},{"address":"t1"}]}`} {
		if _, e := herdrPaneCount([]byte(s)); e == nil {
			t.Fatal(s)
		}
	}
	n, e := herdrPaneCount([]byte(`{"result":{"type":"pane_list","panes":[]}}`))
	if e != nil || n != 0 {
		t.Fatal(n, e)
	}
	n, e = herdrPaneCount([]byte(`{"hidden_finished":0,"terminals":[]}`))
	if e != nil || n != 0 {
		t.Fatal(n, e)
	}
	n, e = herdrPaneCount([]byte(`{"hidden_finished":1,"terminals":[{"address":"t1","state":"live"},{"address":"t2","state":"live"},{"address":"t3","state":"finished"}]}`))
	if e != nil || n != 2 {
		t.Fatal(n, e)
	}
	t.Setenv("HERDR_WORKSPACE_ID", "foreign")
	t.Setenv("HC_SOCKET", "/foreign")
	for _, env := range herdrInventoryEnv("/chosen") {
		if strings.HasPrefix(env, "HERDR_") && env != "HERDR_SOCKET_PATH=/chosen" {
			t.Fatal(env)
		}
		if strings.HasPrefix(env, "HC_") && env != "HC_SOCKET=/chosen" {
			t.Fatal(env)
		}
	}
}
func TestHerdrInventoryCountsForeignNativeWorkspaces(t *testing.T) {
	binary, e := exec.LookPath("herdr-core")
	if e != nil {
		t.Skip("herdr-core not installed")
	}
	if _, e = exec.LookPath("lsof"); e != nil {
		t.Skip("lsof not installed")
	}
	base := "/var/tmp"
	if _, e = os.Stat("/private/tmp"); e == nil {
		base = "/private/tmp"
	}
	dir, e := os.MkdirTemp(base, "fae-census-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "mux.sock")
	_, extraEnv, e := writeHerdrCoreConfig(dir, socket, "census", "/bin/sh")
	if e != nil {
		t.Fatal(e)
	}
	env := cleanMultiplexerEnv(os.Environ(), append(extraEnv, "HOME="+dir, "TMPDIR="+dir)...)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, binary, "serve")
	server.Env = env
	if e = server.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { q := exec.Command(binary, "server", "stop"); q.Env = env; _ = q.Run(); _ = server.Wait() }()
	for i := 0; i < 100; i++ {
		if socketReady(socket) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, label := range []string{"foreign-one", "foreign-two"} {
		q := exec.CommandContext(ctx, binary, "run", "--bg", "--no-parent", "--no-notify", "--name", label, "--cwd", dir, "--json")
		q.Env = env
		if out, e := q.CombinedOutput(); e != nil {
			t.Fatalf("create: %s %v", out, e)
		}
	}
	counts, e := HerdrInventory(ctx, binary)
	if e != nil {
		t.Fatal(e)
	}
	if counts[socket] != 2 {
		t.Fatalf("foreign workspace count: %v", counts)
	}
}
