package processowner

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A host command with a long argument vector (gofmt -l over a large tree
// expands to every file's absolute path) leaves a small record that
// Inventory and Retire read back.
func TestLongCommandRecordStaysReadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "processes")
	args := []string{"/bin/true"}
	for i := 0; i < 1000; i++ {
		args = append(args, "/var/tmp/a-long-worktree-path/"+strings.Repeat("x", 100))
	}
	cmd := exec.Command(args[0], args[1:]...)
	r, err := StartRole(cmd, dir, "instance-owner", "session-long", "host-command")
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(r.Path); err != nil || info.Size() > 32<<10 {
		t.Fatalf("record %v exceeds the read bound: %v", info.Size(), err)
	}
	inventory, err := Inventory(dir)
	if err != nil || len(inventory) != 1 {
		t.Fatal(inventory, err)
	}
	command := inventory[0].Command
	if command[0] != "/bin/true" || !strings.HasPrefix(command[len(command)-1], "... ") || !strings.HasSuffix(command[len(command)-1], " more arguments") {
		t.Fatalf("command not bounded with a count of the rest: %q", command[len(command)-1])
	}
	if err = Wait(cmd, r); err != nil {
		t.Fatal(err)
	}
	if err = Retire(r); err != nil {
		t.Fatal("retirement of a long-command record:", err)
	}
}

// A record written before commands were bounded holds the whole vector;
// recovery and retirement must still read it, or the instance can't start.
func TestLegacyLongCommandRecordIsReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "process-1.json")
	r := Record{PID: 4242, PGID: 4242, ParentPID: 4241, Birth: "b", ParentBirth: "p", Owner: "o", Session: "s", Command: []string{"gofmt", "-l"}}
	for i := 0; i < 2000; i++ {
		r.Command = append(r.Command, "/var/tmp/a-long-worktree-path/"+strings.Repeat("x", 100))
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := read(path)
	if err != nil || len(raw) < 200<<10 || len(got.Command) != 2002 {
		t.Fatal(len(raw), err)
	}
}

func TestRecordCommandKeepsShortVectors(t *testing.T) {
	argv := []string{"go", "test", "-json", "./..."}
	if got := recordCommand(argv); strings.Join(got, " ") != strings.Join(argv, " ") {
		t.Fatal(got)
	}
}
