package api

import (
	"testing"
	"time"
)

func TestCleanupDeadlineScope(t *testing.T) {
	for _, method := range []string{"BackupReplicationUnpin", "BackupQuota", "BackupRetention", "BackupReplicate", "PlaneBackup", "CleanupRetired", "ReviewWorkspacePrepare", "Status", "History", "Admit", "CleanupScratch", ""} {
		want := 5 * time.Second
		if method == "BackupReplicationUnpin" || method == "BackupQuota" || method == "BackupRetention" || method == "BackupReplicate" || method == "PlaneBackup" || method == "CleanupRetired" || method == "ReviewWorkspacePrepare" {
			want = 2 * time.Minute
		}
		if requestBudget(method) != want {
			t.Fatal(method)
		}
	}
}
