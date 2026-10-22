package workflow

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/scanner"
	"go/token"
	"sort"
)

// GoTestIdentity hashes normalized syntax of exactly the named Go test
// functions. Comments, formatting and unrelated functions do not affect it.
// File acquisition and immutable storage remain the caller's responsibility.
func GoTestIdentity(source []byte, names []string) (string, error) {
	if len(names) == 0 {
		return "", fmt.Errorf("no declared test functions")
	}
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "red_test.go", source, 0)
	if err != nil {
		return "", err
	}
	wanted := map[string]bool{}
	for _, n := range names {
		if n == "" || wanted[n] {
			return "", fmt.Errorf("empty or duplicate test name")
		}
		wanted[n] = true
	}
	functions := map[string]*ast.FuncDecl{}
	for _, decl := range file.Decls {
		if f, ok := decl.(*ast.FuncDecl); ok && f.Recv == nil && wanted[f.Name.Name] {
			if functions[f.Name.Name] != nil {
				return "", fmt.Errorf("duplicate test declaration")
			}
			functions[f.Name.Name] = f
		}
	}
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	var b bytes.Buffer
	for _, n := range sorted {
		f := functions[n]
		if f == nil {
			return "", fmt.Errorf("missing function %s", n)
		}
		part := source[fs.Position(f.Pos()).Offset:fs.Position(f.End()).Offset]
		var scan scanner.Scanner
		set := token.NewFileSet()
		scan.Init(set.AddFile("function", -1, len(part)), part, nil, 0)
		pendingSemi := false
		for {
			_, tok, lit := scan.Scan()
			if tok == token.EOF {
				break
			}
			if tok == token.SEMICOLON {
				pendingSemi = true
				continue
			}
			if pendingSemi && tok != token.RBRACE {
				fmt.Fprintf(&b, "%d:1:;;", token.SEMICOLON)
			}
			pendingSemi = false
			fmt.Fprintf(&b, "%d:%d:%s;", tok, len(lit), lit)
		}
		b.WriteByte(0)
	}
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:]), nil
}
