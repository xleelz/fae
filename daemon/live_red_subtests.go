package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"

	"github.com/xleelz/fae/execution"
)

// liveRedSubtestRefusal refuses a derived RED whose test function starts
// subtests with its own *testing.T's Run (FAE-WF-036, FAE-WF-073). Each
// subtest is one more executed test, and the RED gate needs exactly one
// executed test, so such a RED could never pass it: refused when its
// RedReport is produced, it costs the turn's retry, not a test run and the
// RED loop. Only Run selected on the test's own parameter in the function's
// body is seen (t.Run(...), run := t.Run); a subtest started through an alias
// of the parameter or a helper is left to the RED gate. A RED file that can't
// be read or parsed is left to the RED run, which reports it.
func (x *liveExecutor) liveRedSubtestRefusal(b execution.Binding, file, id string) error {
	source, err := x.testSource(b, file)
	if err != nil || !liveRedStartsSubtests(source, id) {
		return nil
	}
	return liveScopeRefusal("RedReport test %s calls t.Run: a RED is one top-level test function without subtests, since the RED gate needs exactly one executed test and each subtest counts as one", id)
}

// liveRedStartsSubtests reports whether Go source declares test function id
// and that function's body selects Run on its *testing.T parameter.
func liveRedStartsSubtests(source []byte, id string) bool {
	f, err := parser.ParseFile(token.NewFileSet(), "red_test.go", source, 0)
	if err != nil {
		return false
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Name.Name != id || fn.Body == nil || len(fn.Type.Params.List) != 1 || len(fn.Type.Params.List[0].Names) != 1 {
			continue
		}
		param := fn.Type.Params.List[0].Names[0].Obj
		found := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Run" {
				if ident, ok := sel.X.(*ast.Ident); ok && param != nil && ident.Obj == param {
					found = true
				}
			}
			return !found
		})
		return found
	}
	return false
}
