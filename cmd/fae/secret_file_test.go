package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSecretFileOwnerOnlyOutsideRepository(t *testing.T) {
	// TMPDIR may itself lie inside a checkout; secrets must not.
	root, e := os.MkdirTemp("/var/tmp", "fae-secret-file-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	private := filepath.Join(root, "live.json")
	if e := os.WriteFile(private, []byte(`{"environment":{"ANTHROPIC_API_KEY":"k"}}`), 0600); e != nil {
		t.Fatal(e)
	}
	if raw, e := readSecretFile(private); e != nil || !strings.Contains(string(raw), "ANTHROPIC_API_KEY") {
		t.Fatal(string(raw), e)
	}
	readable := filepath.Join(root, "readable.json")
	if e := os.WriteFile(readable, []byte(`{}`), 0644); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(root, "link.json")
	if e := os.Symlink(private, link); e != nil {
		t.Fatal(e)
	}
	repo := filepath.Join(root, "repo", "nested")
	if e := os.MkdirAll(repo, 0700); e != nil {
		t.Fatal(e)
	}
	// A linked worktree carries a .git file rather than a directory.
	if e := os.WriteFile(filepath.Join(root, "repo", ".git"), []byte("gitdir: /elsewhere\n"), 0600); e != nil {
		t.Fatal(e)
	}
	tracked := filepath.Join(repo, "live.json")
	if e := os.WriteFile(tracked, []byte(`{}`), 0600); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{readable, link, tracked, root, filepath.Join(root, "missing.json")} {
		if _, e := readSecretFile(bad); e == nil {
			t.Fatal("accepted", bad)
		}
	}
	err := execute(context.Background(), []string{"serve", "--home", root, "--workspace", root, "--adapter", "direct", "--live-config", readable}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "owner-only") {
		t.Fatal("serve accepted a group/world-readable live configuration", err)
	}
}
