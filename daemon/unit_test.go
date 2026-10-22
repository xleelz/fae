package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvelopePreservesLargeNumbers(t *testing.T) {
	var e envelope
	if err := decodeEnvelope([]byte(`{"job":{"evidence":[{"fields":{"n":9007199254740993}}]},"admission":{}}`), &e); err != nil {
		t.Fatal(err)
	}
	n, ok := e.Job.Evidence[0].Fields["n"].(json.Number)
	if !ok || n.String() != "9007199254740993" {
		t.Fatalf("number corrupted: %v", e.Job.Evidence)
	}
}
func TestPrivateWriteRefusesSymlinksAndUnsafeMode(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(link, []byte("replacement")); err == nil {
		t.Fatal("followed symlink")
	}
	b, _ := os.ReadFile(target)
	if string(b) != "original" {
		t.Fatal("foreign file changed")
	}
	if err := os.Chmod(target, 0644); err != nil {
		t.Fatal(err)
	}
	if err := writePrivate(target, []byte("replacement")); err == nil {
		t.Fatal("accepted unsafe mode")
	}
	b, _ = os.ReadFile(target)
	if string(b) != "original" {
		t.Fatal("unsafe file changed")
	}
}
