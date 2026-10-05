// Package prompt assembles the text sent to a runtime for one job turn:
// the job prompt, the style files, the labeled inputs, the response
// schemas the model must choose among, and a shared closing instruction.
// Assemble is pure: it reads no files and calls no network. The caller
// (internal/job, in a later task) loads prompt and style files through
// zing.Assets and fences untrusted text before handing it here.
package prompt

import (
	"strings"

	"zing/internal/fence"
)

// Tail is the closing instruction appended after the schemas to every
// assembled prompt, byte-for-byte from the Zing Design Document section
// 22: what the model's final message must be, what to do on a question or
// an error, and the zing validate loop to run before finishing.
const Tail = "Your final message is exactly one <zing> document that follows the schema\n" +
	"above. Nothing else in the final message is read. If you need a decision\n" +
	"from the owner, return the question outcome. If you cannot continue, return\n" +
	"the error outcome with what, why, and what you tried.\n" +
	"\n" +
	"Before you finish, check the document with this command, and put nothing\n" +
	"before or after it on the command:\n" +
	"\n" +
	"zing validate - <<'EOF'\n" +
	"THE WHOLE DOCUMENT\n" +
	"EOF\n" +
	"\n" +
	"Fix every error it prints and run it again until it prints nothing. If\n" +
	"your tools cannot run it, return the document anyway: Zing validates every\n" +
	"document and sends any errors back.\n" +
	"\n" +
	"Write no angle brackets in prose. Write a placeholder in capitals, such as\n" +
	"RUN-ID-stderr.log, and a comparison in words, such as at most 64 KiB.\n" +
	"Where code needs a literal <, write it as &lt;, and write & as &amp;, even\n" +
	"inside backticks. Every claim is checked by a program, not a person."

// Input is everything Assemble needs for one prompt: the job prompt text,
// the style files layered over it, the labeled inputs (a ticket, an
// owner's answer, a model's findings, and so on), and the response
// schemas the model must choose among. Fence wraps an Untrusted input's
// text; nil means fence.Wrap. The four ForXxx constructors below build an
// Input for one job's fencing rule (design section 12, plan section 4.2);
// Schemas is always left for the caller to set from response.RenderTemplate.
type Input struct {
	JobPrompt string
	Styles    []string
	Inputs    []NamedInput
	Schemas   []string
	Fence     func(string) string // nil means fence.Wrap
}

// NamedInput is one labeled block of the inputs section. Label identifies
// it in the prompt ("ticket", "answer", "findings", ...). Untrusted marks
// text that did not originate with the owner's config or the machine's
// own closed vocabulary — a ticket body, an owner's reply, a model's own
// prior output — which Assemble fences before it reaches the model again
// (plan section 4.2, D15).
type NamedInput struct {
	Label     string
	Text      string
	Untrusted bool
}

// Assemble renders in as one prompt string, in order: the job prompt, a
// blank line, each style (blank line between), the inputs block (each
// input as "Label:" on its own line then its text, fenced when Untrusted,
// blank line between inputs), each schema (blank line between), then Tail.
// A section with nothing in it (no styles, no inputs) contributes no
// block and no extra blank line. Every block is trimmed of trailing
// newlines before joining, so a source file or a rendered schema that
// already ends in "\n" (every prompt and style file does; so does
// response.RenderTemplate, design section 6.8) still yields exactly one
// blank line at the join, not two. Assemble reads no files.
func Assemble(in Input) string {
	fenceFn := in.Fence
	if fenceFn == nil {
		fenceFn = fence.Wrap
	}

	blocks := make([]string, 0, 2+len(in.Styles)+len(in.Schemas))
	blocks = append(blocks, strings.TrimRight(in.JobPrompt, "\n"))
	for _, style := range in.Styles {
		blocks = append(blocks, strings.TrimRight(style, "\n"))
	}

	if len(in.Inputs) > 0 {
		entries := make([]string, len(in.Inputs))
		for i, input := range in.Inputs {
			text := input.Text
			if input.Untrusted {
				text = fenceFn(text)
			}
			entries[i] = input.Label + ":\n" + strings.TrimRight(text, "\n")
		}
		blocks = append(blocks, strings.Join(entries, "\n\n"))
	}

	for _, schema := range in.Schemas {
		blocks = append(blocks, strings.TrimRight(schema, "\n"))
	}
	blocks = append(blocks, Tail)

	return strings.Join(blocks, "\n\n")
}
