package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRun_NeverReturnsABareError guards the closed Run contract errors.go
// declares (design section 4.1, F026): Run returns nil or exactly one of
// ErrStart, ErrTimeout, ErrCanceled, ErrOutputTooLarge, *ExecError, or
// *InvalidOutputError, never a bare fmt.Errorf or errors.New. It walks
// claude.go and codex.go's AST and fails when a return statement inside Run
// itself, or the run helper Run forwards its result from, hands back a
// direct fmt.Errorf(...) or errors.New(...) call in a result position.
//
// It is a syntactic guard, not a proof: it sees only a constructor call
// written directly in the return, not an error routed through a local
// variable, and it stops at nested func literals (a closure returns from
// itself, not from Run). A helper Run/run calls (claudeArgv, newSessionUUID,
// codexOutputDir, readCapped, claudeToolLists) may still wrap internally; Run
// and run must translate that into a typed error before returning it, as
// this test's grep companion (see F026's spec) confirms by hand.
func TestRun_NeverReturnsABareError(t *testing.T) {
	t.Parallel()

	for _, file := range []string{"claude.go", "codex.go"} {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			checkNoBareErrorfReturn(t, file)
		})
	}
}

// checkNoBareErrorfReturn parses filename (relative to this package) and
// fails t for every return statement, inside a method named Run or run,
// whose result list includes a direct fmt.Errorf(...) or errors.New(...)
// call.
func checkNoBareErrorfReturn(t *testing.T, filename string) {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Recv == nil {
			return true
		}
		if fn.Name.Name != "Run" && fn.Name.Name != "run" {
			return true
		}

		ast.Inspect(fn.Body, func(n ast.Node) bool {
			// A return inside a nested func literal returns from that
			// literal, not from Run; do not descend into closures.
			if _, ok := n.(*ast.FuncLit); ok {
				return false
			}
			ret, ok := n.(*ast.ReturnStmt)
			if !ok {
				return true
			}
			for _, res := range ret.Results {
				call, ok := res.(*ast.CallExpr)
				if !ok {
					continue
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					continue
				}
				pkgIdent, ok := sel.X.(*ast.Ident)
				if !ok {
					continue
				}
				bareFmt := pkgIdent.Name == "fmt" && sel.Sel.Name == "Errorf"
				bareErrors := pkgIdent.Name == "errors" && sel.Sel.Name == "New"
				if bareFmt || bareErrors {
					t.Errorf("%s: %s.%s returns a bare %s.%s at %s",
						filename, recvTypeName(fn), fn.Name.Name, pkgIdent.Name, sel.Sel.Name, fset.Position(ret.Pos()))
				}
			}
			return true
		})
		return true
	})
}

// recvTypeName reports fn's receiver type name (Claude or Codex), stripping
// a leading pointer star, for the test failure message.
func recvTypeName(fn *ast.FuncDecl) string {
	expr := fn.Recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if ident, ok := expr.(*ast.Ident); ok {
		return ident.Name
	}
	return "?"
}
