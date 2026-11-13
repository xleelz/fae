package execution

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTranscriptDoesNotResumeEphemeralProviderConversation(t *testing.T) {
	for _, tc := range []struct{ name, audit, want string }{
		{"ephemeral", `{"thread_id":"ephemeral-id","resumable":false}`, ""},
		{"persistent", `{"thread_id":"persistent-id","resumable":true}`, "persistent-id"},
		{"legacy", `{"thread_id":"legacy-id"}`, "legacy-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "provider-test")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "provider-session.json"), []byte(tc.audit), 0600); err != nil {
				t.Fatal(err)
			}
			if got := Transcript(root, time.Now(), time.Now()).ProviderSession; got != tc.want {
				t.Fatalf("resume session %q, want %q", got, tc.want)
			}
		})
	}
}
