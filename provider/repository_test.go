package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	r "github.com/xleelz/fae/runtime"
	"github.com/xleelz/fae/workflow"
)

type listingBlock struct {
	Files     []string
	Truncated bool
}

func promptListing(t *testing.T, l r.BridgeConfig) (listingBlock, bool) {
	t.Helper()
	raw, ok := promptBlocks(t, []byte(providerFile(t, l.LogDirectory, "prompt.json")))["repository"]
	var listing listingBlock
	if ok {
		if e := json.Unmarshal(raw, &listing); e != nil {
			t.Fatal(e)
		}
	}
	return listing, ok
}

func promptKinds(t *testing.T, l r.BridgeConfig) []string {
	t.Helper()
	var blocks []struct{ Kind string }
	if e := json.Unmarshal([]byte(providerFile(t, l.LogDirectory, "prompt.json")), &blocks); e != nil {
		t.Fatal(e)
	}
	var kinds []string
	for _, b := range blocks {
		kinds = append(kinds, b.Kind)
	}
	return kinds
}

// indexPaths adds paths to the worktree's index without writing files, so a
// listing can be large and cheap.
func indexPaths(t *testing.T, root string, paths []string) {
	t.Helper()
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	hash := exec.Command("git", "-C", root, "hash-object", "-w", "--stdin")
	hash.Env = env
	out, e := hash.Output()
	if e != nil {
		t.Fatal(e)
	}
	var info strings.Builder
	for _, p := range paths {
		fmt.Fprintf(&info, "100644 %s\t%s\n", strings.TrimSpace(string(out)), p)
	}
	update := exec.Command("git", "-C", root, "update-index", "--add", "--index-info")
	update.Env, update.Stdin = env, strings.NewReader(info.String())
	if out, e := update.CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
}

// Every turn sees the worktree's tracked paths, sorted, in a repository block
// placed just before the files block; untracked files are not listed. The
// output protocol tells the model about the new blocks and edit forms.
func TestRepositoryListingBlock(t *testing.T) {
	blocks := []any{taskScope([]string{"answer.go"}, ""), map[string]any{"kind": "engine_feedback", "runs": []any{}}, map[string]any{"kind": "operator_context", "text": "context"}}
	c, config, l := turnFixture(t, "spec", blocks, acceptedNoEdits)
	l.Brief = json.RawMessage(strings.Replace(string(l.Brief), `[`, `[{"kind":"role_instructions","text":"role"},`, 1))
	writeLaunch(t, config, l)
	for _, name := range []string{"zeta.go", "pkg/beta.go", "alpha.go", "untracked.go"} {
		writeFile(t, l.Worktree, name, "package x\n")
	}
	gitRepository(t, l.Worktree, "zeta.go", "pkg/beta.go", "alpha.go", "answer.go")
	c.Files = nil
	if e := Run(context.Background(), c, config); e != nil {
		t.Fatal(e)
	}
	listing, ok := promptListing(t, l)
	if !ok || !slices.Equal(listing.Files, []string{"alpha.go", "answer.go", "pkg/beta.go", "zeta.go"}) || listing.Truncated {
		t.Fatal(ok, listing)
	}
	if got := strings.Join(promptKinds(t, l), ","); got != "task_header,role_instructions,stage,task_scope,engine_feedback,repository,files,operator_context,kernel_contract" {
		t.Fatal("prompt order", got)
	}
	var contract struct {
		OutputProtocol string `json:"output_protocol"`
	}
	json.Unmarshal(promptBlocks(t, []byte(providerFile(t, l.LogDirectory, "prompt.json")))["kernel_contract"], &contract)
	for _, want := range []string{"task_scope", "repository", "engine_feedback", "failed runs", "replacements", "exactly once", "*_test.go", "expected_failure", "compile failure", "-run '^Name$' ./...", "declared_files"} {
		if !strings.Contains(contract.OutputProtocol, want) {
			t.Error("output protocol lacks", want)
		}
	}
	if strings.Contains(contract.OutputProtocol, "whole-file edits") {
		t.Error("output protocol still limits edits to whole files")
	}
}

// The listing keeps at most 5,000 paths and 256 KiB of path text, sorted,
// and says when it was cut.
func TestRepositoryListingCaps(t *testing.T) {
	var many []string
	for i := 0; i < 5001; i++ {
		many = append(many, fmt.Sprintf("dir/file%05d.go", i))
	}
	long := strings.Repeat("d", 200)
	var wide []string
	for i := 0; i < 1400; i++ {
		wide = append(wide, fmt.Sprintf("%s/%04d.go", long, i))
	}
	for name, tc := range map[string]struct {
		paths []string
		want  int
	}{
		"exactly the path cap": {many[:5000], 5000},
		"over the path cap":    {many, 5000},
		"over the byte cap":    {wide, (256 << 10) / (len(wide[0]) + 1)},
	} {
		t.Run(name, func(t *testing.T) {
			c, config, l := turnFixture(t, "spec", nil, acceptedNoEdits)
			indexPaths(t, l.Worktree, tc.paths)
			if e := Run(context.Background(), c, config); e != nil {
				t.Fatal(e)
			}
			listing, ok := promptListing(t, l)
			sorted := slices.Sorted(slices.Values(tc.paths))
			if !ok || len(listing.Files) != tc.want || !slices.Equal(listing.Files, sorted[:tc.want]) || listing.Truncated != (len(tc.paths) > tc.want) {
				t.Fatal(ok, len(listing.Files), listing.Truncated)
			}
		})
	}
}

// A worktree git cannot list refuses the turn before launch rather than
// sending a listing that looks complete.
func TestRepositoryListingGitFailureRefused(t *testing.T) {
	c, config, l := turnFixture(t, "spec", nil, acceptedNoEdits)
	if e := os.RemoveAll(filepath.Join(l.Worktree, ".git")); e != nil {
		t.Fatal(e)
	}
	if e := Run(context.Background(), c, config); e == nil || !strings.Contains(e.Error(), "repository listing failed") {
		t.Fatal(e)
	}
	if launched(l) {
		t.Fatal("provider launched without a repository listing")
	}
	// No git outside the role's refusing command shims also refuses.
	c, config, l = turnFixture(t, "spec", nil, acceptedNoEdits)
	t.Setenv("PATH", filepath.Join(l.LogDirectory, "command-shims"))
	if e := Run(context.Background(), c, config); e == nil || !strings.Contains(e.Error(), "outside the role command shims") {
		t.Fatal(e)
	}
}

// The runtime puts a refusing git first on the bridge's PATH for the model;
// the bridge's own listing uses the real git behind it.
func TestRepositoryListingSkipsRoleCommandShims(t *testing.T) {
	c, config, l := turnFixture(t, "spec", nil, acceptedNoEdits)
	gitRepository(t, l.Worktree, "answer.go")
	shims := filepath.Join(l.LogDirectory, "command-shims")
	writeFile(t, shims, "git", "#!/bin/sh\nexit 77\n")
	if e := os.Chmod(filepath.Join(shims, "git"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	if e := Run(context.Background(), c, config); e != nil {
		t.Fatal(e)
	}
	if listing, ok := promptListing(t, l); !ok || !slices.Equal(listing.Files, []string{"answer.go"}) {
		t.Fatal(ok, listing)
	}
}

// A provider loop gets the listing only when it already gets files.
func TestProviderLoopRepositoryListing(t *testing.T) {
	for _, withFiles := range []bool{false, true} {
		t.Run(fmt.Sprint(withFiles), func(t *testing.T) {
			c, config, l := turnFixture(t, "provider-loop", nil, acceptedNoEdits)
			script, _ := os.ReadFile(c.Binary)
			script = []byte(strings.Replace(string(script), "cat > prompt.json\n", "cat > prompt.json\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"12345678-1234-1234-1234-123456789abc\"}'\n", 1))
			if e := os.WriteFile(c.Binary, script, 0700); e != nil {
				t.Fatal(e)
			}
			l.WorkflowID, l.WorkflowRevision, l.Role, l.SessionPolicy = "fae-loop:demo", "v1", "oracle", "task-bound"
			l.Profile = workflow.Profile{Allowed: []string{"read", "handoff"}}
			writeLaunch(t, config, l)
			c.SessionRoot = sessionRoot(t)
			c.Timeout = 5 * time.Second
			c.Files = nil
			if withFiles {
				c.Files = []string{"answer.go"}
			} else if e := os.RemoveAll(filepath.Join(l.Worktree, ".git")); e != nil {
				t.Fatal(e)
			}
			if e := Run(context.Background(), c, config); e != nil {
				t.Fatal(e)
			}
			raw, _ := filepath.Glob(filepath.Join(c.SessionRoot, "*", "cwd", "prompt.json"))
			if len(raw) != 1 {
				t.Fatal(raw)
			}
			prompt, _ := os.ReadFile(raw[0])
			if _, ok := promptBlocks(t, prompt)["repository"]; ok != withFiles {
				t.Fatal("loop listing", ok)
			}
		})
	}
}
