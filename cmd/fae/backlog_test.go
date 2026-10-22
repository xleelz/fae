package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestBacklogCLIValidation(t *testing.T) {
	for _, args := range [][]string{{"backlog"}, {"backlog", "edge"}, {"backlog", "edge", "bad"}, {"backlog", "admit"}, {"backlog", "admit", "--authorizing-message", "m"}} {
		if err := execute(context.Background(), args, &bytes.Buffer{}); err == nil {
			t.Fatal(args)
		}
	}
	for _, content := range []string{`{"unknown":1}`, `{} {}`} {
		path := filepath.Join(t.TempDir(), "payload.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if err := execute(context.Background(), []string{"backlog", "admit", "--file", path, "--authorizing-message", "m"}, &bytes.Buffer{}); err == nil {
			t.Fatal("invalid accepted", content)
		}
	}
}
