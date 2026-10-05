package runtime

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// leakedBinPath stands in for the operator-configured binary path that a
// *os.PathError or *exec.Error carries in its own text (design section
// "startErr keeps the cause and drops the path", #23): startErr must drop
// it, so TestStartErr asserts it never survives into the wrapped error.
const leakedBinPath = "/home/someone/bin/codex"

// TestStartErr proves startErr always wraps ErrStart, keeps whichever cause
// it recognizes (a syscall.Errno, exec.ErrNotFound/ErrDot, or a context
// error), and never lets the binary path in a *os.PathError or *exec.Error
// reach the result's text. An unrecognized error falls back to bare
// ErrStart, which loses the diagnosis but also drops whatever the original
// error's text carried.
func TestStartErr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		err   error
		cause error // nil means startErr falls back to bare ErrStart
	}{
		{
			name:  "fork/exec ETXTBSY",
			err:   &os.PathError{Op: "fork/exec", Path: leakedBinPath, Err: syscall.ETXTBSY},
			cause: syscall.ETXTBSY,
		},
		{
			name:  "pipe2 EMFILE",
			err:   os.NewSyscallError("pipe2", syscall.EMFILE),
			cause: syscall.EMFILE,
		},
		{
			name:  "bare EAGAIN",
			err:   syscall.EAGAIN,
			cause: syscall.EAGAIN,
		},
		{
			name:  "exec.ErrNotFound",
			err:   &exec.Error{Name: leakedBinPath, Err: exec.ErrNotFound},
			cause: exec.ErrNotFound,
		},
		{
			name:  "context canceled",
			err:   context.Canceled,
			cause: context.Canceled,
		},
		{
			name:  "unrecognized error",
			err:   errors.New("x " + leakedBinPath),
			cause: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := startErr(tc.err)

			if !errors.Is(got, ErrStart) {
				t.Fatalf("startErr(%v) = %v, want errors.Is ErrStart", tc.err, got)
			}

			wantText := ErrStart.Error()
			if tc.cause != nil {
				if !errors.Is(got, tc.cause) {
					t.Errorf("startErr(%v) = %v, want errors.Is %v", tc.err, got, tc.cause)
				}
				wantText = ErrStart.Error() + ": " + tc.cause.Error()
			}
			if got.Error() != wantText {
				t.Errorf("startErr(%v).Error() = %q, want %q", tc.err, got.Error(), wantText)
			}

			if strings.Contains(got.Error(), leakedBinPath) {
				t.Errorf("startErr(%v).Error() = %q, leaks the path", tc.err, got.Error())
			}
		})
	}
}
