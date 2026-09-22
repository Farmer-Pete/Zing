package main

import (
	"bytes"
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

	// Drain the pipe in a goroutine started before fn runs. Without a
	// concurrent reader, a write in fn large enough to fill the OS pipe
	// buffer (~64 KB) would block forever, since io.ReadAll only ran after
	// fn returned; the read end was also never closed.
	captured := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, r); err != nil {
			// t.Fatal is unsafe off the test goroutine; surface the error
			// through the channel so the caller's assertion fails visibly.
			captured <- "captureStderr: io.Copy: " + err.Error()
			return
		}
		captured <- buf.String()
	}()

	code = fn()

	if closeErr := w.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	stderr = <-captured
	if closeErr := r.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	return code, stderr
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

// planningReadyExample is a fully valid planning/ready document, shaped as
// a feature: no loop, no repro, no hypotheses, and its first test is kind
// unit rather than regression. Its one code claim's evidence names
// validate.go, which exists relative to this package's directory (go
// test's working directory), so the code-claim path check passes too.
const planningReadyExample = `<zing job="planning" outcome="ready">` +
	`<claims><claim kind="code" verdict="true" evidence="validate.go:1">the flag is parsed here</claim></claims>` +
	`<scenarios>` +
	`<scenario id="s1" kind="behavior"><given>a well-formed file</given><when>zing validate runs on it</when><then>it exits 0 with no output</then></scenario>` +
	`<scenario id="s2" kind="negative"><given>an invalid file</given><when>zing validate runs on it</when><then>it exits 1 and prints each error</then></scenario>` +
	`</scenarios>` +
	`<plan>` +
	`<overview>` +
	`<objective>Add a validate subcommand so a document can be checked outside a run.</objective>` +
	`<context>cmd/zing/main.go dispatches subcommands by name.</context>` +
	`<problem>Nothing today checks a document against the schema without running a job.</problem>` +
	`<goals><goal>zing validate exits 1 and lists every error for an invalid document</goal></goals>` +
	`<nongoals><nongoal>Fixing the document automatically is out of scope</nongoal></nongoals>` +
	`</overview>` +
	`<design>` +
	`<demo cmd="go run ./cmd/zing validate doc.xml">Running it against a bad document prints one error per line.</demo>` +
	`<shape>main.go gains a validate case that calls runValidate.</shape>` +
	`<migrations none="true"></migrations>` +
	`</design>` +
	`<delivery>` +
	`<files><file path="cmd/zing/validate.go" action="modify">wire the new subcommand</file></files>` +
	`<deletions none="true"></deletions>` +
	`<tests><test name="TestRunValidate" seam="runValidate" kind="unit" mocks="">a bad document exits 1</test></tests>` +
	`<tasks><task n="1" test="TestRunValidate" demo="true">Add runValidate and wire it into main.</task></tasks>` +
	`</delivery>` +
	`<review>` +
	`<trust_root>none</trust_root>` +
	`<alternatives><alternative>Check documents only inside a run: rejected, a standalone check is more useful for debugging.</alternative></alternatives>` +
	`<risks><risk>A stale schema copy would give a false pass.</risk></risks>` +
	`</review>` +
	`</plan>` +
	`</zing>`

// TestRunValidate_FeatureKindAcceptsAPlanningReadyDocument proves the same
// document that fails under --kind bug (below) passes cleanly under the
// default feature rules: it has none of the bug-only shape, and feature
// rules never ask for it.
func TestRunValidate_FeatureKindAcceptsAPlanningReadyDocument(t *testing.T) {
	path := writeFile(t, planningReadyExample)

	code, out := captureStderr(t, func() int { return runValidate([]string{path}) })

	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if out != "" {
		t.Errorf("stderr = %q, want empty", out)
	}
}

// TestRunValidate_KindBugRejectsAFeatureShapedPlan proves --kind bug
// actually activates the bug-plan-shape rules (design section 6.6): the
// very document that validates cleanly as a feature (above) fails under
// bug with exactly the four bug-shape errors, in Layer 2's fixed order,
// since it never claimed to be a bug plan in the first place.
func TestRunValidate_KindBugRejectsAFeatureShapedPlan(t *testing.T) {
	path := writeFile(t, planningReadyExample)

	code, out := captureStderr(t, func() int { return runValidate([]string{"--kind", "bug", path}) })

	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	want := "plan/overview/problem/loop: bug plan needs a loop\n" +
		"plan/overview/problem/repro: bug plan needs a repro\n" +
		"plan/overview/problem/hypotheses: bug plan needs three to five hypotheses\n" +
		"plan/delivery/tests/test[0]/kind: first test must be kind regression\n"
	if out != want {
		t.Errorf("stderr = %q, want %q", out, want)
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
