package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/xleelz/fae/defaults"
)

const replaceSource = "package answer\n\nfunc A() int { return 1 }\n\nfunc C() int { return 3 }\n"

// Replacements apply in order, each to the content the earlier ones left, and
// the result is written like a whole-file edit; whole-file edits (including a
// new file) mix with replacement edits in one response.
func TestReplacementEditsApplied(t *testing.T) {
	answer := `{"output":{"accepted":true},"edits":[
 {"path":"answer.go","content":null,"replacements":[{"old":"return 1","new":"return 10"},{"old":"10","new":"20"},{"old":"func A()","new":"func B()"}]},
 {"path":"pkg/new.go","content":"package pkg\n","replacements":[]}]}`
	c, config, l := turnFixture(t, "implement", nil, answer)
	writeFile(t, l.Worktree, "answer.go", replaceSource)
	c.Files = []string{"answer.go", "pkg/new.go"}
	if e := Run(context.Background(), c, config); e != nil {
		t.Fatal(e)
	}
	if got := readFile(t, l.Worktree, "answer.go"); got != "package answer\n\nfunc B() int { return 20 }\n\nfunc C() int { return 3 }\n" {
		t.Fatal(got)
	}
	if got := readFile(t, l.Worktree, "pkg/new.go"); got != "package pkg\n" {
		t.Fatal(got)
	}
	if _, e := os.Stat(l.Handoff); e != nil {
		t.Fatal(e)
	}
}

// A replacement whose old text is missing, ambiguous or empty, or one aimed at
// a file that does not exist, refuses the whole response: nothing is written,
// including the response's other valid edits, and no handoff is published.
func TestReplacementEditsRefused(t *testing.T) {
	valid := `{"path":"other.go","content":"package other\n","replacements":[]}`
	for name, tc := range map[string]struct{ edit, want string }{
		"missing old":      {`{"path":"answer.go","content":null,"replacements":[{"old":"return 1","new":"return 2"},{"old":"return 1","new":"x"}]}`, `edit "answer.go" replacements[1]: old text not found`},
		"ambiguous old":    {`{"path":"answer.go","content":null,"replacements":[{"old":"int { return","new":"x"}]}`, `edit "answer.go" replacements[0]: old text occurs more than once`},
		"overlapping old":  {`{"path":"answer.go","content":null,"replacements":[{"old":"return 1","new":"aaa"},{"old":"aa","new":"b"}]}`, `edit "answer.go" replacements[1]: old text occurs more than once`},
		"empty old":        {`{"path":"answer.go","content":null,"replacements":[{"old":"","new":"x"}]}`, `edit "answer.go" replacements[0]: old text is empty`},
		"missing file":     {`{"path":"absent.go","content":null,"replacements":[{"old":"a","new":"b"}]}`, `edit "absent.go": replacements need an existing file`},
		"both forms":       {`{"path":"answer.go","content":"x","replacements":[{"old":"return 1","new":"return 2"}]}`, "exactly one of content or replacements"},
		"neither form":     {`{"path":"answer.go","content":null,"replacements":[]}`, "exactly one of content or replacements"},
		"result over 1MiB": {`{"path":"answer.go","content":null,"replacements":[{"old":"return 1","new":"` + strings.Repeat("y", 600<<10) + `"}]}`, `edit "answer.go": result exceeds 1048576 bytes`},
	} {
		t.Run(name, func(t *testing.T) {
			c, config, l := turnFixture(t, "implement", nil, `{"output":{"accepted":true},"edits":[`+valid+`,`+tc.edit+`]}`)
			source := replaceSource + "// " + strings.Repeat("x", 600<<10) + "\n"
			writeFile(t, l.Worktree, "answer.go", source)
			c.Files = []string{"answer.go", "other.go", "absent.go"}
			e := Run(context.Background(), c, config)
			if e == nil || !strings.Contains(e.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, e)
			}
			if got := readFile(t, l.Worktree, "answer.go"); got != source {
				t.Fatal("refused response changed a file", got)
			}
			for _, name := range []string{"other.go", "absent.go"} {
				if _, e := os.Stat(filepath.Join(l.Worktree, name)); !os.IsNotExist(e) {
					t.Fatal("refused response wrote", name)
				}
			}
			if _, e := os.Lstat(l.Handoff); !os.IsNotExist(e) {
				t.Fatal("refused response published a handoff")
			}
		})
	}
}

// strictObjects checks that every object schema under v lists all of its
// properties as required and allows no others (Codex strict mode).
func strictObjects(t *testing.T, path string, v any) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if properties, ok := m["properties"].(map[string]any); ok {
		var keys, required []string
		for k := range properties {
			keys = append(keys, k)
		}
		for _, k := range m["required"].([]any) {
			required = append(required, k.(string))
		}
		slices.Sort(keys)
		slices.Sort(required)
		if !slices.Equal(keys, required) || m["additionalProperties"] != false {
			t.Errorf("%s is not strict: properties %v, required %v, additionalProperties %v", path, keys, required, m["additionalProperties"])
		}
		for k, p := range properties {
			strictObjects(t, path+"."+k, p)
		}
	}
	if items, ok := m["items"]; ok {
		strictObjects(t, path+"[]", items)
	}
}

// checkEditSchema checks the edit item shape of a provider response schema.
func checkEditSchema(t *testing.T, raw []byte) {
	t.Helper()
	var schema map[string]any
	if e := json.Unmarshal(raw, &schema); e != nil {
		t.Fatal(e, string(raw))
	}
	edits := schema["properties"].(map[string]any)["edits"]
	strictObjects(t, "edits", edits)
	item := edits.(map[string]any)["items"].(map[string]any)
	properties := item["properties"].(map[string]any)
	content, _ := json.Marshal(properties["content"])
	replacements, _ := json.Marshal(properties["replacements"])
	if string(content) != `{"anyOf":[{"type":"string"},{"type":"null"}]}` || !strings.Contains(string(replacements), `"type":"array"`) || !strings.Contains(string(replacements), `"old"`) {
		t.Errorf("edit item shape: content %s replacements %s", content, replacements)
	}
}

// Both CLIs get one strict edit item shape: path, content (string or null)
// and replacements, all required.
func TestProviderSchemaEditShape(t *testing.T) {
	c, config, l := turnFixture(t, "spec", nil, acceptedNoEdits)
	if e := Run(context.Background(), c, config); e != nil {
		t.Fatal(e)
	}
	checkEditSchema(t, []byte(providerFile(t, l.LogDirectory, "schema.json")))
}

// Every shipped artifact type's response schema fits Claude's argument bound
// and keeps the strict edit shape.
func TestClaudeSchemaForShippedArtifacts(t *testing.T) {
	for name, artifact := range defaults.Shipped().Artifacts {
		t.Run(name, func(t *testing.T) {
			c, config, l := claudeFixture(t, "")
			l.Brief, _ = json.Marshal([]any{map[string]any{"kind": "stage", "stage": "spec"}, map[string]any{"kind": "kernel_contract", "output_schema": artifact.Schema}})
			writeLaunch(t, config, l)
			// The fixture's output is not this artifact; only the schema matters.
			_ = Run(context.Background(), c, config)
			var schema string
			for _, arg := range strings.Split(providerFile(t, l.LogDirectory, "args.txt"), "\n") {
				if s, ok := strings.CutPrefix(arg, "--json-schema="); ok {
					schema = s
				}
			}
			if schema == "" || len(schema) > maxClaudeSchemaBytes {
				t.Fatal("schema missing or over the argument bound", len(schema))
			}
			checkEditSchema(t, []byte(schema))
		})
	}
}
