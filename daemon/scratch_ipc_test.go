package daemon

import (
	"context"
	"encoding/json"
	"github.com/xleelz/fae/processowner"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func scratchTestFIFO(t *testing.T, root string) string {
	t.Helper()
	dir := filepath.Join(root, "sessions", "session-test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := map[string]string{"protocol": "fae-agent-v1", "session_id": "turn", "task_id": "task", "log_directory": dir, "handoff": filepath.Join(dir, "handoff.json"), "worktree": filepath.Join(root, "worktrees", "task")}
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "start.pipe")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestScratchIPCSnapshotAndRefusals(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := scratchTestFIFO(t, root)
	files, err := scratchFiles(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(files["sessions/session-test/start.pipe"], "ipc:v1:") {
		t.Fatal("missing IPC identity")
	}
	dest := filepath.Join(t.TempDir(), "copy")
	if err = scratchCopy(ctx, root, dest); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Lstat(filepath.Join(dest, "sessions/session-test/start.pipe")); !os.IsNotExist(err) {
		t.Fatal("copied FIFO")
	}
	fd, err := syscall.Open(path, syscall.O_RDWR|syscall.O_NONBLOCK, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = scratchFiles(ctx, root); err == nil {
		t.Fatal("open FIFO accepted")
	}
	syscall.Close(fd)
	if err = os.Rename(path, path+".unknown"); err != nil {
		t.Fatal(err)
	}
	if _, err = scratchFiles(ctx, root); err == nil {
		t.Fatal("unknown FIFO accepted")
	}
	if err = os.Rename(path+".unknown", path); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err = scratchFiles(ctx, root); err == nil {
		t.Fatal("public FIFO accepted")
	}
	os.Chmod(path, 0600)
	if err = os.WriteFile(filepath.Join(filepath.Dir(path), "config.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = scratchCopy(ctx, root, filepath.Join(t.TempDir(), "copy")); err == nil {
		t.Fatal("foreign marker accepted")
	}
}
func TestScratchIPCUnknownAndLiveSocket(t *testing.T) {
	// A short physical path keeps the UNIX socket under the sun_path limit;
	// /tmp is a symlink to /private/tmp on macOS.
	base := "/var/tmp"
	if st, err := os.Stat("/private/tmp"); err == nil && st.IsDir() {
		base = "/private/tmp"
	}
	root, err := os.MkdirTemp(base, "fipc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	dir := filepath.Join(root, "sessions", "resident-test")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "mux-client.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	defer os.Remove(path)
	os.Chmod(path, 0600)
	if _, err = scratchFiles(context.Background(), root); err == nil {
		t.Fatal("live socket accepted")
	}
	listener.Close()
	if _, err = scratchFiles(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if err = scratchCopy(context.Background(), root, filepath.Join(t.TempDir(), "copy")); err == nil {
		t.Fatal("socket without marker accepted")
	}
	if err = os.Rename(path, filepath.Join(dir, "unknown.sock")); err != nil {
		t.Fatal(err)
	}
	if _, err = scratchFiles(context.Background(), root); err == nil {
		t.Fatal("unknown socket accepted")
	}
}

func TestScratchIPCResidentOwnerAndIdentityDrift(t *testing.T) {
	root := t.TempDir()
	fifo := scratchTestFIFO(t, root)
	before, err := scratchFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(fifo, fifo+".old"); err != nil {
		t.Fatal(err)
	}
	if err = syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	os.Remove(fifo + ".old")
	after, err := scratchFiles(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if before["sessions/session-test/start.pipe"] == after["sessions/session-test/start.pipe"] {
		t.Fatal("endpoint replacement invisible")
	}
	dir := filepath.Join(root, "sessions", "resident-test")
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "mux-client.sock")
	birth, err := processowner.Birth(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	marker := map[string]any{"Owner": filepath.Join(root, "sessions"), "Session": "resident-test", "ParentPID": os.Getpid(), "ParentBirth": birth, "Socket": filepath.Join(dir, "mux.sock"), "Request": filepath.Join(dir, "request.sock")}
	raw, _ := json.Marshal(marker)
	os.WriteFile(filepath.Join(dir, "resident-owner.json"), raw, 0600)
	if err = scratchIPCMarkers(root, path); err == nil {
		t.Fatal("living resident owner accepted")
	}
	marker["ParentBirth"] = "different-birth"
	raw, _ = json.Marshal(marker)
	os.WriteFile(filepath.Join(dir, "resident-owner.json"), raw, 0600)
	if err = scratchIPCMarkers(root, path); err != nil {
		t.Fatal(err)
	}
	marker["Owner"] = "foreign"
	raw, _ = json.Marshal(marker)
	os.WriteFile(filepath.Join(dir, "resident-owner.json"), raw, 0600)
	if err = scratchIPCMarkers(root, path); err == nil {
		t.Fatal("foreign owner accepted")
	}
}

func TestScratchIPCRejectsEscapedScanPaths(t *testing.T) {
	for _, path := range []string{"/tmp/a\\b", "/tmp/a\nb", "/tmp/é"} {
		if err := scratchIPCUnused(context.Background(), []string{path}); err == nil {
			t.Fatal("ambiguous lsof path accepted")
		}
	}
}

func TestScratchIPCProphetBinding(t *testing.T) {
	root := t.TempDir()
	path := scratchTestFIFO(t, root)
	dir := filepath.Dir(path)
	marker := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]string
	if err = json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	c["worktree"] = filepath.Join(root, "prophet-context")
	c["role"] = "prophet"
	c["workflow_id"] = "fae-prophet"
	c["session_id"] = "prophet-test"
	raw, _ = json.Marshal(c)
	os.WriteFile(marker, raw, 0600)
	if err = scratchIPCMarkers(root, path); err != nil {
		t.Fatal(err)
	}
	c["role"] = "executor"
	raw, _ = json.Marshal(c)
	os.WriteFile(marker, raw, 0600)
	if err = scratchIPCMarkers(root, path); err == nil {
		t.Fatal("non-Prophet context accepted")
	}
}
