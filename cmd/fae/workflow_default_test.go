package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/xleelz/fae/defaults"
)

// `workflow default` prints the shipped content; --local its Local projection.
func TestCLIWorkflowDefaultShippedAndLocal(t *testing.T) {
	for args, want := range map[string]any{"": defaults.Shipped(), "--local": defaults.Workflow()} {
		argv := []string{"workflow", "default", "--json"}
		if args != "" {
			argv = append(argv, args)
		}
		var out bytes.Buffer
		if err := execute(context.Background(), argv, &out); err != nil {
			t.Fatal(err)
		}
		var got, expected any
		raw, _ := json.Marshal(want)
		if json.Unmarshal(out.Bytes(), &got) != nil || json.Unmarshal(raw, &expected) != nil {
			t.Fatal("invalid JSON")
		}
		a, _ := json.Marshal(got)
		b, _ := json.Marshal(expected)
		if !bytes.Equal(a, b) {
			t.Fatalf("workflow default %s printed other content", args)
		}
	}
}
