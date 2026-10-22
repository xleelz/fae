package main

import (
	"bytes"
	"context"
	"testing"
)

func TestProphetCLIRefusals(t *testing.T) {
	for _, args := range [][]string{{"prophet"}, {"prophet", "unknown"}, {"prophet", "chat", "--text", "hello"}, {"prophet", "global", "--task", "a"}, {"prophet", "session", "--text", "hello"}} {
		if e := execute(context.Background(), args, &bytes.Buffer{}); e == nil || exitCode(e) != 2 {
			t.Fatal(args, e)
		}
	}
}
