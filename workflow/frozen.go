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
	"strings"
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
		normalize(&b, source[fs.Position(f.Pos()).Offset:fs.Position(f.End()).Offset])
		b.WriteByte(0)
	}
	sum := sha256.Sum256(b.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// normalize appends the token stream of part, ignoring comments, whitespace
// and optional semicolons, so a formatter pass cannot change it.
func normalize(b *bytes.Buffer, part []byte) {
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
			fmt.Fprintf(b, "%d:1:;;", token.SEMICOLON)
		}
		pendingSemi = false
		fmt.Fprintf(b, "%d:%d:%s;", tok, len(lit), lit)
	}
}

// CheckFrozenFile admits a test file edited after the RED freeze only when
// every declaration of the frozen file survives with identical normalized
// tokens (so helpers, constants and imports the RED relies on are unchanged).
// Formatting, comments, added tests and added ordinary imports are allowed;
// added init or TestMain functions and blank or dot imports, which can alter
// the frozen test's behaviour, are refused (FAE-WF-076).
func CheckFrozenFile(frozen, current []byte, names []string) error {
	if _, err := GoTestIdentity(frozen, names); err != nil {
		return err
	}
	fp, fd, err := declarations(frozen)
	if err != nil {
		return err
	}
	cp, cd, err := declarations(current)
	if err != nil {
		return err
	}
	if fp != cp {
		return fmt.Errorf("package changed from %s to %s", fp, cp)
	}
	keys := []string{}
	for key := range fd {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if got, ok := cd[key]; !ok || got != fd[key] {
			return fmt.Errorf("frozen declaration %s changed or removed", key)
		}
	}
	keys = keys[:0]
	for key := range cd {
		if _, ok := fd[key]; !ok {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.HasPrefix(key, "func init#") || key == "func TestMain" || strings.HasPrefix(key, "import _ ") || strings.HasPrefix(key, "import . ") {
			return fmt.Errorf("added declaration %s can alter the frozen test", key)
		}
	}
	return nil
}
func declarations(source []byte) (string, map[string]string, error) {
	fs := token.NewFileSet()
	file, err := parser.ParseFile(fs, "red_test.go", source, 0)
	if err != nil {
		return "", nil, err
	}
	out := map[string]string{}
	text := func(n ast.Node) string {
		var b bytes.Buffer
		normalize(&b, source[fs.Position(n.Pos()).Offset:fs.Position(n.End()).Offset])
		return b.String()
	}
	add := func(key, value string) {
		for n := 1; ; n++ {
			k := key
			if n > 1 || strings.HasPrefix(key, "func init") {
				k = fmt.Sprintf("%s#%d", key, n)
			}
			if _, taken := out[k]; !taken {
				out[k] = value
				return
			}
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			key := "func " + d.Name.Name
			if d.Recv != nil {
				key = "method " + text(d.Recv) + d.Name.Name
			}
			add(key, text(d))
		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				for _, spec := range d.Specs {
					i := spec.(*ast.ImportSpec)
					name := ""
					if i.Name != nil {
						name = i.Name.Name
					}
					add("import "+name+" "+i.Path.Value, "")
				}
				continue
			}
			names := []string{}
			for _, spec := range d.Specs {
				switch v := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, v.Name.Name)
				case *ast.ValueSpec:
					for _, n := range v.Names {
						names = append(names, n.Name)
					}
				}
			}
			sort.Strings(names)
			add(d.Tok.String()+" "+strings.Join(names, ","), text(d))
		}
	}
	return file.Name.Name, out, nil
}
