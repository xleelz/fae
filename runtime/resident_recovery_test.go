package runtime

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/xleelz/fae/processowner"
)

func retiredResidentFixture(t *testing.T) (*ResidentMultiplexer, *residentEntry, []processowner.Record) {
	t.Helper()
	m, l := residentFixture(t, "")
	l.SessionPolicy = "workflow-global"
	if _, e := m.Run(context.Background(), l); e != nil {
		t.Fatal(e)
	}
	ackResidentTurn(t, m, l.SessionID)
	entry := m.entries[residentKey(l)]
	var records []processowner.Record
	for i := len(entry.records) - 1; i >= 0; i-- {
		record := entry.records[i]
		if e := processowner.Terminate(context.Background(), record, m.Direct.Root); e != nil {
			t.Fatal(e)
		}
		records = append(records, *record)
	}
	// Abrupt exits may leave either Unix socket. Keep both as crash detritus.
	for _, path := range []string{entry.socket, entry.request} {
		if _, e := os.Lstat(path); os.IsNotExist(e) {
			listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if e != nil {
				t.Fatal(e)
			}
			listener.SetUnlinkOnClose(false)
			listener.Close()
		}
	}
	return m, entry, records
}
func TestRecoverResidentSocketsRequiresTerminalOwnership(t *testing.T) {
	m, entry, records := retiredResidentFixture(t)
	if e := RecoverResidentSockets(m.Direct.Root, records[:1]); e == nil {
		t.Fatal("incomplete retirement accepted")
	}
	if _, e := os.Lstat(entry.socket); e != nil {
		t.Fatal("unproven socket removed")
	}
	if e := RecoverResidentSockets(m.Direct.Root, records); e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{entry.socket, entry.request} {
		if _, e := os.Lstat(path); !os.IsNotExist(e) {
			t.Fatalf("crash socket remains %s %v", path, e)
		}
	}
	if e := RecoverResidentSockets(m.Direct.Root, records); e != nil {
		t.Fatalf("recovery not idempotent: %v", e)
	}
}
func TestRecoverResidentSocketsRefusesForeignTarget(t *testing.T) {
	m, entry, records := retiredResidentFixture(t)
	foreign := filepath.Join(t.TempDir(), "foreign")
	if e := os.WriteFile(foreign, []byte("preserve"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(entry.request); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(foreign, entry.request); e != nil {
		t.Fatal(e)
	}
	if e := RecoverResidentSockets(m.Direct.Root, records); e == nil {
		t.Fatal("symlink accepted")
	}
	if data, e := os.ReadFile(foreign); e != nil || string(data) != "preserve" {
		t.Fatal("foreign file changed")
	}
}
func TestRecoverResidentSocketsWithoutResidentRecords(t *testing.T) {
	if e := RecoverResidentSockets(filepath.Join(t.TempDir(), "missing"), nil); e != nil {
		t.Fatal(e)
	}
}

func TestRecoverResidentSocketsSkipsPriorOwnerMarker(t *testing.T) {
	m, entry, records := retiredResidentFixture(t)
	raw, e := os.ReadFile(filepath.Join(entry.dir, "resident-owner.json"))
	if e != nil {
		t.Fatal(e)
	}
	var old residentOwner
	if e = json.Unmarshal(raw, &old); e != nil {
		t.Fatal(e)
	}
	oldDir, e := os.MkdirTemp(m.Direct.Root, "resident-old-")
	if e != nil {
		t.Fatal(e)
	}
	old.ParentBirth += "-previous"
	old.Socket = filepath.Join(oldDir, "mux.sock")
	old.Request = filepath.Join(oldDir, "request.sock")
	raw, _ = json.Marshal(old)
	if e = os.WriteFile(filepath.Join(oldDir, "resident-owner.json"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: old.Socket, Net: "unix"})
	if e != nil {
		t.Fatal(e)
	}
	listener.SetUnlinkOnClose(false)
	listener.Close()
	if e = RecoverResidentSockets(m.Direct.Root, records); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Lstat(old.Socket); e != nil {
		t.Fatal("unmatched historical socket was touched")
	}
	if _, e = os.Lstat(entry.socket); !os.IsNotExist(e) {
		t.Fatal("current retired socket remains")
	}
}
