package commandshim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAE-WF-042: a role profile's allowed command shims bound what a session
// may run, before any capability or lease: a test-capable role limited to
// gofmt is refused go test.
func TestRoleProfileShimAllowlist(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "policy.json")
	raw, _ := json.Marshal(RoleShims{Broker: "/bin/false", Ledger: filepath.Join(dir, "ledger"), Worktree: dir, Test: true, Format: true, Shims: []string{"gofmt"}})
	if err := os.WriteFile(policy, raw, 0600); err != nil {
		t.Fatal(err)
	}
	code, err := RunShim(context.Background(), policy, HeavyOp{Argv: []string{"go", "test", "./..."}, Env: []string{}, Dir: dir})
	if code != Refused || err == nil || !strings.Contains(err.Error(), "not an allowed command shim") {
		t.Fatal(code, err)
	}
}
