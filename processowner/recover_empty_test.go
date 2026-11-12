package processowner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// plantRecordFile writes a registry file with the given body, mode and
// modification time.
func plantRecordFile(t *testing.T, dir, name, body string, mode os.FileMode, modified time.Time) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if e := os.WriteFile(path, []byte(body), mode); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(path, mode); e != nil {
		t.Fatal(e)
	}
	if e := os.Chtimes(path, modified, modified); e != nil {
		t.Fatal(e)
	}
	return path
}

// FAE-CP-003: an empty record an earlier process left (before records were
// published whole, a crash between StartRole's truncate and write left one,
// and a failed rewrite that was retired left an empty receipt) names no
// process, so Recover removes it instead of refusing every start; the rest
// of the registry is recovered as before.
func TestRecoverClearsEmptyRecordsAnEarlierProcessLeft(t *testing.T) {
	dir := testDir(t)
	before := time.Now().Add(-time.Hour)
	empty := plantRecordFile(t, dir, "process-empty.json", "", 0600, before)
	emptyReceipt := plantRecordFile(t, dir, "process-emptyreceipt.json.retired", "", 0600, before)
	receipt := plantRecordFile(t, dir, "process-whole.json.retired", `{"PID":4242,"PGID":4242,"Birth":"b","ParentPID":4241,"ParentBirth":"p","Owner":"owner"}`, 0600, before)
	recovered, e := Recover(context.Background(), dir, "owner")
	if e != nil {
		t.Fatal("an empty record from an earlier process blocked recovery:", e)
	}
	if len(recovered) != 1 || recovered[0].PID != 4242 || recovered[0].Path != receipt {
		t.Fatal("the registry's other records were not recovered", recovered)
	}
	for _, gone := range []string{empty, emptyReceipt} {
		if _, e := os.Lstat(gone); !os.IsNotExist(e) {
			t.Fatal("empty record kept", gone, e)
		}
	}
	if _, e := os.Lstat(receipt); e != nil {
		t.Fatal("a whole receipt was removed", e)
	}
}

// FAE-CP-003: a record Recover cannot read, and an empty one it may not
// remove (written after this process started, not private, or a symlink),
// is refused with its path and the operator's remedy, and left in place.
func TestRecoverRefusesAnUnreadableRecordNamingIt(t *testing.T) {
	before := time.Now().Add(-time.Hour)
	for name, plant := range map[string]func(t *testing.T, dir string) string{
		"empty, written after this process started": func(t *testing.T, dir string) string {
			return plantRecordFile(t, dir, "process-fresh.json", "", 0600, time.Now())
		},
		"empty, not private": func(t *testing.T, dir string) string {
			return plantRecordFile(t, dir, "process-shared.json", "", 0644, before)
		},
		"truncated": func(t *testing.T, dir string) string {
			return plantRecordFile(t, dir, "process-truncated.json", `{"PID":`, 0600, before)
		},
		"truncated receipt": func(t *testing.T, dir string) string {
			return plantRecordFile(t, dir, "process-truncated.json.retired", `{"PID":`, 0600, before)
		},
		"symlink to an empty file": func(t *testing.T, dir string) string {
			target := plantRecordFile(t, t.TempDir(), "target.json", "", 0600, before)
			path := filepath.Join(dir, "process-link.json")
			if e := os.Symlink(target, path); e != nil {
				t.Fatal(e)
			}
			return path
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := testDir(t)
			path := plant(t, dir)
			_, e := Recover(context.Background(), dir, "owner")
			if e == nil {
				t.Fatal("an unreadable record was recovered")
			}
			if !strings.Contains(e.Error(), path) || !strings.Contains(e.Error(), "move the file out of the registry and start again") {
				t.Fatalf("refusal names neither the file nor the remedy: %v", e)
			}
			if _, statErr := os.Lstat(path); statErr != nil {
				t.Fatal("refused record was removed", statErr)
			}
		})
	}
}

// FAE-CP-003, FAE-ROB-014: file times come from a clock that advances once
// per tick, so a file this process wrote just after it started can carry a
// time a little before recoverProcessStarted (re-audit 9 measured up to 1.09
// ms). Recover removes an empty record only when it was last written more
// than the slack (emptyRecordClockSlack, 50 ms) before this process started;
// one written just before, or within the slack, is refused naming it and kept.
func TestRecoverKeepsAnEmptyRecordWithinTheFileClockSlack(t *testing.T) {
	dir := testDir(t)
	path := plantRecordFile(t, dir, "process-recent.json", "", 0600, recoverProcessStarted.Add(-time.Millisecond))
	_, e := Recover(context.Background(), dir, "owner")
	if e == nil || !strings.Contains(e.Error(), path) {
		t.Fatal("an empty record written within the file clock's slack of this start was removed:", e)
	}
	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatal("refused record removed", statErr)
	}
	old := plantRecordFile(t, dir, "process-recent.json", "", 0600, recoverProcessStarted.Add(-time.Second))
	if _, e = Recover(context.Background(), dir, "owner"); e != nil {
		t.Fatal("an empty record older than the slack was refused:", e)
	}
	if _, statErr := os.Lstat(old); !os.IsNotExist(statErr) {
		t.Fatal("empty record kept", statErr)
	}
}
