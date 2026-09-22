package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// captureStderr runs fn with os.Stderr redirected to a pipe and returns
// fn's result plus everything written to stderr. It cannot run in
// parallel with anything else that touches the process os.Stderr.
func captureStderr(t *testing.T, fn func() int) (code int, stderr string) {
	t.Helper()

	r, w, pipeErr := os.Pipe()
	if pipeErr != nil {
		t.Fatal(pipeErr)
	}
	orig := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	code = fn()

	if closeErr := w.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	out, readErr := io.ReadAll(r)
	if readErr != nil {
		t.Fatal(readErr)
	}
	return code, string(out)
}

const validExample = `<zing job="classify" outcome="bug"><reason>it crashes on empty input</reason></zing>`

const invalidExample = `<zing job="classify" outcome="bug"><reason></reason></zing>`

func writeFile(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "doc.xml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunValidate_ValidFileExitsZeroSilently(t *testing.T) {
	path := writeFile(t, validExample)

	code, out := captureStderr(t, func() int { return runValidate([]string{path}) })

	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stderr = %q, want empty", out)
	}
}

func TestRunValidate_InvalidFileExitsOnePrintingEachError(t *testing.T) {
	path := writeFile(t, invalidExample)

	code, out := captureStderr(t, func() int { return runValidate([]string{path}) })

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	want := "reason: must not be empty\n"
	if out != want {
		t.Errorf("stderr = %q, want %q", out, want)
	}
}

func TestRunValidate_KindBugAccepted(t *testing.T) {
	path := writeFile(t, validExample)

	code, out := captureStderr(t, func() int { return runValidate([]string{"--kind", "bug", path}) })

	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stderr = %q, want empty", out)
	}
}

func TestRunValidate_KindInvalidValueExitsTwo(t *testing.T) {
	path := writeFile(t, validExample)

	code, _ := captureStderr(t, func() int { return runValidate([]string{"--kind", "nonsense", path}) })

	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestRunValidate_MissingFileExitsTwo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.xml")

	code, out := captureStderr(t, func() int { return runValidate([]string{path}) })

	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	if out == "" {
		t.Error("stderr should report the read error")
	}
}

func TestRunValidate_WrongArgCountExitsTwo(t *testing.T) {
	code, out := captureStderr(t, func() int { return runValidate(nil) })

	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
	want := "usage: zing validate [--kind bug|feature] <file>\n"
	if out != want {
		t.Errorf("stderr = %q, want %q", out, want)
	}
}

func TestRunValidate_TooManyArgsExitsTwo(t *testing.T) {
	path := writeFile(t, validExample)

	code, _ := captureStderr(t, func() int { return runValidate([]string{path, "extra"}) })

	if code != 2 {
		t.Errorf("code = %d, want 2", code)
	}
}

func TestRunValidate_ParseFailureExitsOne(t *testing.T) {
	path := writeFile(t, "no zing document at all here")

	code, out := captureStderr(t, func() int { return runValidate([]string{path}) })

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	want := "validate: no zing element in final message\n"
	if out != want {
		t.Errorf("stderr = %q, want %q", out, want)
	}
}
