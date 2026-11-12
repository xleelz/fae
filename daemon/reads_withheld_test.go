package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// FAE-SCH-010 and FAE-API-001: READS.md documents every withheld reason the
// daemon can report, and no other. The code's set is every string constant
// named withheld* in the package's sources (each reason the drive records is
// one of them); the documented set is the list under "Withheld reasons".
// Before, READS.md named only molecule_limit.
func TestReadsDocumentsEveryWithheldReason(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	code := map[string]string{}
	fset := token.NewFileSet()
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			g, ok := decl.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				v := spec.(*ast.ValueSpec)
				for i, name := range v.Names {
					if !strings.HasPrefix(name.Name, "withheld") || i >= len(v.Values) {
						continue
					}
					if lit, ok := v.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						reason, err := strconv.Unquote(lit.Value)
						if err != nil {
							t.Fatal(err)
						}
						code[reason] = name.Name
					}
				}
			}
		}
	}
	if len(code) < 17 {
		t.Fatalf("only %d withheld reasons found in the code: %v", len(code), code)
	}
	raw, err := os.ReadFile("READS.md")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	start := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(l, "Withheld reasons") })
	if start < 0 {
		t.Fatal(`READS.md has no "Withheld reasons" list`)
	}
	item := regexp.MustCompile("^- `([a-z_]+)`: ")
	documented := map[string]bool{}
	listed := false
	for _, l := range lines[start+1:] {
		if m := item.FindStringSubmatch(l); m != nil {
			if documented[m[1]] {
				t.Errorf("READS.md lists withheld reason %s twice", m[1])
			}
			documented[m[1]], listed = true, true
			continue
		}
		if listed && !strings.HasPrefix(l, "  ") {
			break // the list ended
		}
	}
	var diff []string
	for reason, name := range code {
		if !documented[reason] {
			diff = append(diff, "undocumented "+reason+" ("+name+")")
		}
	}
	for reason := range documented {
		if _, ok := code[reason]; !ok {
			diff = append(diff, "documented but not in the code: "+reason)
		}
	}
	sort.Strings(diff)
	if len(diff) != 0 {
		t.Fatalf("READS.md's withheld reasons differ from the code:\n%s", strings.Join(diff, "\n"))
	}
}
