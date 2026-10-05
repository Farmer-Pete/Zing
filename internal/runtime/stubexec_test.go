package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// TestNoTestWritesAnExecutable guards against the flake behind
// TestCodexOnStartRunsBeforePromptIsWritten (design section "The guard",
// #23): a test that os.WriteFiles an executable script and then execs it
// can lose the race to a parallel test's fork, which inherits the
// still-open write descriptor and fails its own exec with ETXTBSY. It
// parses every *_test.go in this package, written in the style of
// errors_contract_test.go:40-92, and fails for any call to os.WriteFile,
// os.OpenFile, os.Chmod, or a one-argument .Chmod method whose mode
// argument is not an integer literal, or whose literal sets any of the
// 0o111 execute bits. Scripts that must be executable belong in testdata,
// committed once, never written at run time.
func TestNoTestWritesAnExecutable(t *testing.T) {
	t.Parallel()

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("glob matched no test files")
	}

	for _, file := range files {
		t.Run(file, func(t *testing.T) {
			t.Parallel()
			checkNoExecutableMode(t, file)
		})
	}
}

// chmodMethod is the method name a value's own .Chmod(mode) call carries,
// as opposed to the package-level os.Chmod(path, mode).
const chmodMethod = "Chmod"

// checkNoExecutableMode parses filename (relative to this package) and
// fails t for every mode-bearing call whose mode is not an integer literal
// or whose literal has an execute bit set.
func checkNoExecutableMode(t *testing.T, filename string) {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}

	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		var callee string
		var modeArg ast.Expr

		if fun, isSelector := call.Fun.(*ast.SelectorExpr); isSelector {
			pkgIdent, isPkg := fun.X.(*ast.Ident)
			switch {
			case isPkg && pkgIdent.Name == "os" && fun.Sel.Name == "WriteFile" && len(call.Args) >= 3:
				callee = "os.WriteFile"
				modeArg = call.Args[2]
			case isPkg && pkgIdent.Name == "os" && fun.Sel.Name == "OpenFile" && len(call.Args) >= 3:
				callee = "os.OpenFile"
				modeArg = call.Args[2]
			case isPkg && pkgIdent.Name == "os" && fun.Sel.Name == chmodMethod && len(call.Args) >= 2:
				callee = "os." + chmodMethod
				modeArg = call.Args[1]
			case fun.Sel.Name == chmodMethod && len(call.Args) == 1:
				callee = chmodMethod
				modeArg = call.Args[0]
			}
		}

		if modeArg == nil {
			return true
		}

		lit, ok := modeArg.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			t.Errorf("%s: %s mode must be an integer literal, at %s", filename, callee, fset.Position(modeArg.Pos()))
			return true
		}

		mode, err := strconv.ParseInt(lit.Value, 0, 64)
		if err != nil {
			t.Errorf("%s: %s mode literal %q: %v, at %s", filename, callee, lit.Value, err, fset.Position(lit.Pos()))
			return true
		}

		if mode&0o111 != 0 {
			t.Errorf("%s: %s writes an executable mode (%s); commit the script under testdata instead, at %s",
				filename, callee, lit.Value, fset.Position(lit.Pos()))
		}

		return true
	})
}
