package scratchfs

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestSessionCopyCreatorProcess(t *testing.T) {
	if os.Getenv("FAE_COPY_CREATOR") != "1" {
		return
	}
	path, session := os.Getenv("FAE_COPY_PATH"), os.Getenv("FAE_COPY_SESSION")
	if err := RegisterSessionCopy(path, session); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
func TestSessionCopyReaperRemovesOnlyProvenDeadOwnedCopies(t *testing.T) {
	root := t.TempDir()
	session := filepath.Join(root, "session-crashed")
	dead := filepath.Join(session, "filesystem-view-dead")
	live := filepath.Join(session, "directory-check-live")
	foreign := filepath.Join(session, "filesystem-view-foreign")
	state := filepath.Join(root, "provider-state", "filesystem-view-preserved")
	for _, path := range []string{dead, live, foreign, state} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	child := exec.Command(os.Args[0], "-test.run=^TestSessionCopyCreatorProcess$")
	child.Env = append(os.Environ(), "FAE_COPY_CREATOR=1", "FAE_COPY_PATH="+dead, "FAE_COPY_SESSION="+session)
	if out, err := child.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if err := RegisterSessionCopy(live, session); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dead, 0000); err != nil {
		t.Fatal(err)
	}
	if err := ReapSessionCopies(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dead); !os.IsNotExist(err) {
		t.Fatal("dead owned copy left behind", err)
	}
	for _, path := range []string{live, foreign, state} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("live,foreign or provider state removed", path, err)
		}
	}
}
