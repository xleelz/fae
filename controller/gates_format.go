package controller

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

// LineRange is an inclusive range of changed lines in the tip version.
type LineRange struct{ First, Last int }

// GoFormatItems is the item-scoped formatting check for one Go file. Each
// top-level declaration overlapping a changed line is formatted on its own,
// never the whole file, so its cost is bounded by the item, not the file.
func GoFormatItems(file string, base, tip []byte, changed []LineRange) []FormatItem {
	items := []FormatItem{}
	current, ok := goItems(tip)
	if !ok {
		return append(items, FormatItem{File: file, Item: "<unparseable file>"})
	}
	previous := map[string]bool{}
	if base != nil {
		if old, parsed := goItems(base); parsed {
			for _, i := range old {
				previous[i.key] = i.formatted
			}
		}
	}
	for _, i := range current {
		touched := false
		for _, r := range changed {
			touched = touched || r.First <= i.last && i.first <= r.Last
		}
		if !touched {
			continue
		}
		formatted, existed := previous[i.key]
		items = append(items, FormatItem{File: file, Item: i.key, ExistedBase: existed, FormattedBase: formatted, Formatted: i.formatted})
	}
	return items
}

type goItem struct {
	key         string
	first, last int
	formatted   bool
}

func goItems(src []byte) ([]goItem, bool) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "item.go", src, parser.ParseComments)
	if err != nil {
		return nil, false
	}
	items := []goItem{}
	seen := map[string]int{}
	for _, d := range f.Decls {
		start := d.Pos()
		switch x := d.(type) {
		case *ast.FuncDecl:
			if x.Doc != nil {
				start = x.Doc.Pos()
			}
		case *ast.GenDecl:
			if x.Doc != nil {
				start = x.Doc.Pos()
			}
		}
		from, to := fset.Position(start).Offset, fset.Position(d.End()).Offset
		if nl := bytes.IndexByte(src[to:], '\n'); nl >= 0 {
			to += nl
		} else {
			to = len(src)
		}
		key := declKey(d)
		seen[key]++
		if seen[key] > 1 {
			key += "#" + strconv.Itoa(seen[key])
		}
		items = append(items, goItem{key: key, first: fset.Position(start).Line, last: fset.Position(d.End()).Line, formatted: itemFormatted(src[from:to])})
	}
	return items, true
}

func itemFormatted(text []byte) bool {
	src := append([]byte("package p\n\n"), text...)
	src = append(src, '\n')
	out, err := format.Source(src)
	return err == nil && bytes.Equal(out, src)
}

func declKey(d ast.Decl) string {
	switch x := d.(type) {
	case *ast.FuncDecl:
		if x.Recv != nil && len(x.Recv.List) > 0 {
			return fmt.Sprintf("func (%s) %s", typeName(x.Recv.List[0].Type), x.Name.Name)
		}
		return "func " + x.Name.Name
	case *ast.GenDecl:
		names := []string{}
		for _, s := range x.Specs {
			switch spec := s.(type) {
			case *ast.TypeSpec:
				names = append(names, spec.Name.Name)
			case *ast.ValueSpec:
				for _, n := range spec.Names {
					names = append(names, n.Name)
				}
			case *ast.ImportSpec:
				names = append(names, spec.Path.Value)
			}
		}
		return strings.ToLower(x.Tok.String()) + " " + strings.Join(names, ",")
	}
	return "decl"
}

func typeName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return "*" + typeName(x.X)
	case *ast.Ident:
		return x.Name
	case *ast.IndexExpr:
		return typeName(x.X)
	case *ast.IndexListExpr:
		return typeName(x.X)
	case *ast.SelectorExpr:
		return typeName(x.X) + "." + x.Sel.Name
	}
	return "?"
}

// ChangedLines reads tip-side line ranges from unified-diff hunk headers. A
// pure deletion marks the lines on either side of the removed text.
func ChangedLines(diff string) []LineRange {
	out := []LineRange{}
	for _, line := range strings.Split(diff, "\n") {
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 || !strings.HasPrefix(fields[2], "+") {
			continue
		}
		start, count, _ := strings.Cut(strings.TrimPrefix(fields[2], "+"), ",")
		first, e := strconv.Atoi(start)
		if e != nil {
			continue
		}
		n := 1
		if count != "" {
			if n, e = strconv.Atoi(count); e != nil {
				continue
			}
		}
		if n == 0 {
			out = append(out, LineRange{first, first + 1})
			continue
		}
		out = append(out, LineRange{first, first + n - 1})
	}
	return out
}
