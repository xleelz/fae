package processowner

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Where the registry's filesystem has no hard links, a record is placed by a
// rename that refuses an existing name; it still never replaces a record, and
// a filesystem with neither fails with an error that says so.
func TestPublishWithoutHardLinksRenamesWithoutReplacing(t *testing.T) {
	linkRecord = func(old, new string) error { return &os.LinkError{Op: "link", Old: old, New: new, Err: syscall.EPERM} }
	defer func() { linkRecord = os.Link }()
	dir := testDir(t)
	cmd := exec.Command("/bin/sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e := cmd.Start(); e != nil {
		t.Fatal(e)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait() }()
	r, e := RegisterRole(cmd.Process.Pid, dir, "owner", "session", "host-command")
	if e != nil {
		t.Fatal(e)
	}
	if got, e := read(r.Path); e != nil || got.PID != cmd.Process.Pid || got.Role != "host-command" {
		t.Fatal("renamed record unreadable", got, e)
	}
	if pending, _ := filepath.Glob(filepath.Join(dir, pendingRecordPrefix+"*")); len(pending) != 0 {
		t.Fatal("pending file left after the rename", pending)
	}
	existing, _ := os.ReadFile(r.Path)
	pending := filepath.Join(dir, pendingRecordPrefix+"1-1.json")
	if e = os.WriteFile(pending, []byte(`{"replacement":true}`), 0600); e != nil {
		t.Fatal(e)
	}
	if e = placeRecord(pending, r.Path); !errors.Is(e, os.ErrExist) {
		t.Fatal("an existing record was not refused", e)
	}
	if now, _ := os.ReadFile(r.Path); string(now) != string(existing) {
		t.Fatal("an existing record was replaced", string(now))
	}
	if _, e = os.Stat(pending); !os.IsNotExist(e) {
		t.Fatal("refused pending file kept", e)
	}
	if e = os.WriteFile(pending, []byte(`{}`), 0600); e != nil {
		t.Fatal(e)
	}
	e = placeRecord(pending, filepath.Join(dir, "missing", "process-1.json"))
	if e == nil || !strings.Contains(e.Error(), "neither a hard link nor a no-replace rename") || !errors.Is(e, syscall.EPERM) {
		t.Fatal("unplaceable record not reported clearly", e)
	}
}
