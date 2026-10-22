package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHerdrInventoryParsing(t *testing.T) {
	paths, e := herdrSockets([]byte("p123\nf4\nn/private/tmp/mux.sock\nf5\nn/private/tmp/mux-client.sock\nf6\nn->0xabc\n"))
	if e != nil || len(paths) != 1 || paths[0] != "/private/tmp/mux.sock" {
		t.Fatal(paths, e)
	}
	for _, s := range []string{"p123\nn->0xabc\n", "pwat\n", "p12\nn/tmp/../socket\n"} {
		if _, e := herdrSockets([]byte(s)); e == nil {
			t.Fatal(s)
		}
	}
	for _, s := range []string{`{}`, `{"result":{"type":"pane_list"}}`, `{"result":{"type":"pane_list","panes":[{}]}}`, `{"result":{"type":"pane_list","panes":[{"pane_id":"x"},{"pane_id":"x"}]}}`} {
		if _, e := herdrPaneCount([]byte(s)); e == nil {
			t.Fatal(s)
		}
	}
	n, e := herdrPaneCount([]byte(`{"result":{"type":"pane_list","panes":[]}}`))
	if e != nil || n != 0 {
		t.Fatal(n, e)
	}
	t.Setenv("HERDR_WORKSPACE_ID", "foreign")
	for _, env := range herdrInventoryEnv("/chosen") {
		if strings.HasPrefix(env, "HERDR_") && env != "HERDR_SOCKET_PATH=/chosen" {
			t.Fatal(env)
		}
	}
}
func TestHerdrInventoryCountsForeignNativeWorkspaces(t *testing.T) {
	binary, e := exec.LookPath("herdr")
	if e != nil {
		t.Skip("herdr not installed")
	}
	if _, e = exec.LookPath("lsof"); e != nil {
		t.Skip("lsof not installed")
	}
	base := "/tmp"
	if _, e = os.Stat("/private/tmp"); e == nil {
		base = "/private/tmp"
	}
	dir, e := os.MkdirTemp(base, "fae-census-")
	if e != nil {
		t.Fatal(e)
	}
	defer os.RemoveAll(dir)
	config := filepath.Join(dir, "herdr.toml")
	socket := filepath.Join(dir, "mux.sock")
	if e = os.WriteFile(config, []byte("onboarding=false\n[terminal]\ndefault_shell=\"/bin/sh\"\nshell_mode=\"non_login\"\n[update]\nversion_check=false\n"), 0600); e != nil {
		t.Fatal(e)
	}
	env := append(herdrInventoryEnv(socket), "HERDR_CONFIG_PATH="+config, "HOME="+dir)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	server := exec.CommandContext(ctx, binary, "server")
	server.Env = env
	if e = server.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { q := exec.Command(binary, "server", "stop"); q.Env = env; _ = q.Run(); _ = server.Wait() }()
	for i := 0; i < 100; i++ {
		if _, e = os.Stat(socket); e == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, label := range []string{"foreign-one", "foreign-two"} {
		q := exec.CommandContext(ctx, binary, "workspace", "create", "--cwd", dir, "--label", label, "--no-focus")
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
