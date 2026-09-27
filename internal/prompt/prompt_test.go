package prompt

import (
	"regexp"
	"testing"
)

// TestAssemble_Order asserts the exact block order and separators (plan
// section 4.2): the job prompt, a blank line, each style, the inputs
// block, each schema, then Tail — joined so a present section gets
// exactly one blank line on each side and an absent one contributes none.
func TestAssemble_Order(t *testing.T) {
	t.Parallel()

	got := Assemble(Input{
		JobPrompt: "JOB",
		Styles:    []string{"S1", "S2"},
		Inputs:    []NamedInput{{Label: "L", Text: "T", Untrusted: false}},
		Schemas:   []string{"SC1", "SC2"},
	})

	want := "JOB\n\nS1\n\nS2\n\nL:\nT\n\nSC1\n\nSC2\n\n" + Tail

	if got != want {
		t.Errorf("Assemble order mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestAssemble_EmptyInputsYieldsNoInputsBlock asserts that with no Inputs,
// Assemble emits no inputs block and no extra blank line for it: styles
// (if any) join straight to schemas (if any) with one blank line, exactly
// as if the inputs section had never existed.
func TestAssemble_EmptyInputsYieldsNoInputsBlock(t *testing.T) {
	t.Parallel()

	got := Assemble(Input{JobPrompt: "J", Schemas: []string{"SC"}})
	want := "J\n\nSC\n\n" + Tail
	if got != want {
		t.Errorf("Assemble with empty Inputs = %q, want %q", got, want)
	}

	gotWithStyles := Assemble(Input{JobPrompt: "J", Styles: []string{"S1"}, Schemas: []string{"SC"}})
	wantWithStyles := "J\n\nS1\n\nSC\n\n" + Tail
	if gotWithStyles != wantWithStyles {
		t.Errorf("Assemble with styles and empty Inputs = %q, want %q", gotWithStyles, wantWithStyles)
	}
}

// TestAssemble_TrimsTrailingNewlineBetweenBlocks asserts that a block
// ending in its own trailing newline (every prompt and style file does,
// and so does response.RenderTemplate's output) still yields exactly one
// blank line at the join, not two.
func TestAssemble_TrimsTrailingNewlineBetweenBlocks(t *testing.T) {
	t.Parallel()

	got := Assemble(Input{
		JobPrompt: "JOB\n",
		Styles:    []string{"S1\n"},
		Schemas:   []string{"SC\n"},
	})
	want := "JOB\n\nS1\n\nSC\n\n" + Tail
	if got != want {
		t.Errorf("Assemble did not trim trailing newlines before joining\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestAssemble_NilFenceUsesFenceWrap asserts that a nil Input.Fence falls
// back to fence.Wrap: a fresh 6-hex-character nonce on every call and the
// section 12 guidance line, not some ad hoc test-only wrapping.
func TestAssemble_NilFenceUsesFenceWrap(t *testing.T) {
	t.Parallel()

	got := Assemble(Input{
		JobPrompt: "J",
		Inputs:    []NamedInput{{Label: "ticket", Text: "secret", Untrusted: true}},
	})

	nonceRe := regexp.MustCompile(`<<<UNTRUSTED [0-9a-f]{6}>>>`)
	if !nonceRe.MatchString(got) {
		t.Errorf("Assemble with nil Fence did not produce a 6-hex-nonce fence header; got:\n%s", got)
	}

	if !regexp.MustCompile(regexp.QuoteMeta(testFenceGuidance)).MatchString(got) {
		t.Errorf("Assemble with nil Fence did not carry fence.Wrap's guidance line; got:\n%s", got)
	}
}
