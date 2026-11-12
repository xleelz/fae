package processowner

import (
	"context"
	"strings"
	"testing"
	"time"
)

// FAE-ROB-014: every empty record Recover removes is reported to the caller,
// which logs and audits it, even when Recover then refuses another record of
// the registry; Recover itself still returns only the recovered records.
func TestRecoverClearingReportsEachRemovedRecord(t *testing.T) {
	dir := testDir(t)
	before := time.Now().Add(-time.Hour)
	empty := plantRecordFile(t, dir, "process-a-empty.json", "", 0600, before)
	emptyReceipt := plantRecordFile(t, dir, "process-b-empty.json.retired", "", 0600, before)
	recovered, cleared, e := RecoverClearing(context.Background(), dir, "owner")
	if e != nil || len(recovered) != 0 {
		t.Fatal(recovered, e)
	}
	if len(cleared) != 2 || !slicesHave(cleared, empty) || !slicesHave(cleared, emptyReceipt) {
		t.Fatal("removed records not reported", cleared)
	}
	again := plantRecordFile(t, dir, "process-a-empty.json", "", 0600, before)
	garbled := plantRecordFile(t, dir, "process-z-garbled.json", `{"PID":`, 0600, before)
	_, cleared, e = RecoverClearing(context.Background(), dir, "owner")
	if e == nil || !strings.Contains(e.Error(), garbled) || len(cleared) != 1 || cleared[0] != again {
		t.Fatal("a removal before a refusal went unreported", cleared, e)
	}
}

// slicesHave reports whether list holds s.
func slicesHave(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
