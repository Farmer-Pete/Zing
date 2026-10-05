package runtime

import (
	"context"
	"errors"
	"fmt"
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
			violations, err := checkExecutableModes(file, nil)
			if err != nil {
				t.Fatalf("parse %s: %v", file, err)
			}
			for _, v := range violations {
				t.Error(v)
			}
		})
	}
}

// osModeArg maps an os package function name that takes a file mode to that
// mode argument's index.
var osModeArg = map[string]int{"WriteFile": 2, "OpenFile": 2, "Chmod": 1}

// osIdentName returns the local identifier that refers to the os package in
// f: the name an explicit import alias gives it, or the default "os" if it
// is imported unaliased or not imported at all (a source snippet in this
// file's own self-tests). A guard that instead matched the literal "os"
// would miss a file that imports os under an alias (review finding t0f,
// #23).
func osIdentName(f *ast.File) string {
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != "os" {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "os"
	}
	return "os"
}

// checkExecutableModes parses filename (read from disk when src is nil, or
// parsed from src otherwise, per go/parser.ParseFile) and returns one
// message per mode-bearing call whose mode is not an integer literal, or
// whose literal has an execute bit set: a call to os.WriteFile, os.OpenFile,
// os.Chmod, or a one-argument .Chmod method.
func checkExecutableModes(filename string, src any) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		return nil, err
	}
	osName := osIdentName(f)

	var violations []string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		fun, isSelector := call.Fun.(*ast.SelectorExpr)
		if !isSelector {
			return true
		}

		var callee string
		var modeArg ast.Expr

		pkgIdent, isPkg := fun.X.(*ast.Ident)
		isOSCall := isPkg && pkgIdent.Name == osName
		switch {
		case isOSCall:
			if idx, hasMode := osModeArg[fun.Sel.Name]; hasMode && len(call.Args) > idx {
				callee = "os." + fun.Sel.Name
				modeArg = call.Args[idx]
			}
		case fun.Sel.Name == "Chmod" && len(call.Args) == 1:
			callee = fun.Sel.Name
			modeArg = call.Args[0]
		}

		if modeArg == nil {
			return true
		}

		lit, ok := modeArg.(*ast.BasicLit)
		if !ok || lit.Kind != token.INT {
			violations = append(violations, fmt.Sprintf("%s: %s mode must be an integer literal, at %s",
				filename, callee, fset.Position(modeArg.Pos())))
			return true
		}

		mode, err := strconv.ParseInt(lit.Value, 0, 64)
		if err != nil {
			violations = append(violations, fmt.Sprintf("%s: %s mode literal %q: %v, at %s",
				filename, callee, lit.Value, err, fset.Position(lit.Pos())))
			return true
		}

		if mode&0o111 != 0 {
			violations = append(violations, fmt.Sprintf("%s: %s writes an executable mode (%s); commit the script under testdata instead, at %s",
				filename, callee, lit.Value, fset.Position(lit.Pos())))
		}

		return true
	})

	return violations, nil
}

// TestCheckExecutableModes is a self-test of checkExecutableModes: it feeds
// small source snippets with a known verdict, so a broken selector match, a
// wrong argument index, or a dropped 0o111 check would show up here even
// though the real package's tests, once fixed, give the guard nothing left
// to flag (review finding r1f4, #23).
func TestCheckExecutableModes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want int
	}{
		{"WriteFile executable", `os.WriteFile(p, b, 0o755)`, 1},
		{"OpenFile executable", `os.OpenFile(p, f, 0o700)`, 1},
		{"Chmod executable", `os.Chmod(p, 0o711)`, 1},
		{"method Chmod executable", `file.Chmod(0o755)`, 1},
		{"non-literal mode", `os.WriteFile(p, b, mode)`, 1},
		{"clean literal", `os.WriteFile(p, b, 0o644)`, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			src := "package p\n\nfunc f() {\n\t" + tc.body + "\n}\n"
			got, err := checkExecutableModes("snippet.go", src)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(got) != tc.want {
				t.Errorf("checkExecutableModes(%q) = %v, want %d violation(s)", tc.body, got, tc.want)
			}
		})
	}
}

// TestCheckExecutableModes_AliasedImport guards against a bypass review
// finding t0f (#23): the guard once matched the literal identifier "os",
// so a file that imports the os package under a local alias could call
// WriteFile with an executable mode and never be flagged. checkExecutableModes
// must resolve the file's own import of "os" to its local name, alias or
// not, before matching selector calls against it.
func TestCheckExecutableModes_AliasedImport(t *testing.T) {
	t.Parallel()

	src := `package p

import stdos "os"

func f() {
	stdos.WriteFile(p, b, 0o755)
}
`
	got, err := checkExecutableModes("snippet.go", src)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("checkExecutableModes(aliased os import) = %v, want 1 violation", got)
	}
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
			name:  "exec.ErrDot",
			err:   &exec.Error{Name: leakedBinPath, Err: exec.ErrDot},
			cause: exec.ErrDot,
		},
		{
			name:  "context canceled",
			err:   context.Canceled,
			cause: context.Canceled,
		},
		{
			name:  "context deadline exceeded",
			err:   context.DeadlineExceeded,
			cause: context.DeadlineExceeded,
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
